package main

import (
	"regexp"
	"strings"
	"unicode"
)

// Terminal styles for an agent's Markdown, after the way Claude Code shows
// its own output: bold and italics as such, code in its own colour, pipe
// tables drawn as boxes that fit the width.
const (
	sgrItalic    = "\x1b[3m"
	sgrUnderline = "\x1b[4m"
	sgrCode      = "\x1b[33m"
)

// span is a run of text in one style (SGR codes added on top of the
// block's base style).
type span struct {
	sgr  string
	text string
}

// renderMarkdown renders an agent's Markdown for a terminal width columns
// wide. base is the SGR the surrounding block uses ("" or dim); every line
// opens with it and every reset re-applies it. text must already be free of
// control characters: the escapes in the result are the renderer's own.
func renderMarkdown(text string, width int, base string) string {
	if width < 20 {
		width = 20
	}
	lines := strings.Split(text, "\n")
	var out [][]span
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if fence := codeFence(trimmed); fence != "" {
			if lang := strings.TrimSpace(strings.TrimLeft(trimmed, fence[:1])); lang != "" {
				out = append(out, []span{{sgrDim, lang}})
			}
			for i++; i < len(lines); i++ {
				if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
					break
				}
				out = append(out, []span{{sgrCode, strings.ReplaceAll(lines[i], "\t", "    ")}})
			}
			continue
		}
		if i+1 < len(lines) && strings.Contains(line, "|") {
			if aligns := tableSeparator(lines[i+1]); aligns != nil && len(tableCells(line)) == len(aligns) {
				rows := [][]string{tableCells(line)}
				for i += 2; i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != ""; i++ {
					rows = append(rows, tableCells(lines[i]))
				}
				i--
				out = append(out, renderTable(rows, aligns, width)...)
				continue
			}
		}
		switch {
		case horizontalRule.MatchString(line):
			out = append(out, []span{{sgrDim, strings.Repeat("─", width)}})
		case heading.MatchString(line):
			m := heading.FindStringSubmatch(line)
			style := sgrBold
			if len(m[1]) <= 2 {
				style += sgrUnderline
			}
			out = append(out, inlineMarkdown(m[2], style))
		case bullet.MatchString(line):
			m := bullet.FindStringSubmatch(line)
			out = append(out, append([]span{{"", m[1] + "• "}}, inlineMarkdown(m[3], "")...))
		case quote.MatchString(line):
			m := quote.FindStringSubmatch(line)
			out = append(out, append([]span{{sgrDim, m[1] + "│ "}}, inlineMarkdown(m[2], "")...))
		default:
			out = append(out, inlineMarkdown(line, ""))
		}
	}
	rendered := make([]string, len(out))
	for i, l := range out {
		rendered[i] = spanLine(l, base)
	}
	return strings.Join(rendered, "\n")
}

var (
	heading        = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)(?:\s+#+)?\s*$`)
	horizontalRule = regexp.MustCompile(`^ {0,3}(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	bullet         = regexp.MustCompile(`^(\s*)([-*+])\s+(.*)$`)
	quote          = regexp.MustCompile(`^(\s*)>\s?(.*)$`)
	tableAlign     = regexp.MustCompile(`^\s*(:?)-+(:?)\s*$`)
)

// codeFence returns the fence (``` or ~~~, or a longer run) a line opens a
// code block with, else "".
func codeFence(trimmed string) string {
	for _, c := range "`~" {
		run := strings.Repeat(string(c), 3)
		if strings.HasPrefix(trimmed, run) {
			n := len(trimmed) - len(strings.TrimLeft(trimmed, string(c)))
			if c == '`' && strings.Contains(trimmed[n:], "`") {
				return "" // ```x``` is inline code
			}
			return trimmed[:n]
		}
	}
	return ""
}

type align uint8

const (
	alignLeft align = iota
	alignCenter
	alignRight
)

// tableSeparator parses the |---|:---:| row under a table's header; nil
// when line is not one.
func tableSeparator(line string) []align {
	if !strings.Contains(line, "-") {
		return nil
	}
	var out []align
	for _, c := range tableCells(line) {
		m := tableAlign.FindStringSubmatch(c)
		if m == nil {
			return nil
		}
		switch {
		case m[1] != "" && m[2] != "":
			out = append(out, alignCenter)
		case m[2] != "":
			out = append(out, alignRight)
		default:
			out = append(out, alignLeft)
		}
	}
	return out
}

// tableCells splits a table row on the pipes that are not escaped.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// renderTable draws rows (the first is the header) as a box no wider than
// width, shrinking the widest columns and wrapping their cells to fit.
func renderTable(rows [][]string, aligns []align, width int) [][]span {
	n := len(aligns)
	cells := make([][][]span, len(rows))
	natural := make([]int, n)
	for r, row := range rows {
		cells[r] = make([][]span, n)
		for c := 0; c < n; c++ {
			text := ""
			if c < len(row) {
				text = row[c]
			}
			style := ""
			if r == 0 {
				style = sgrBold
			}
			cells[r][c] = inlineMarkdown(text, style)
			natural[c] = max(natural[c], max(1, spansCells(cells[r][c])))
		}
	}
	room := width - (3*n + 1) // borders and a space either side of each cell
	if room < n {
		// No room for even one cell per column: the table as written.
		var out [][]span
		for _, row := range rows {
			out = append(out, []span{{"", "| " + strings.Join(row, " | ") + " |"}})
		}
		return out
	}
	widths := append([]int(nil), natural...)
	for total := sum(widths); total > room; total-- {
		widest := 0
		for c := range widths {
			if widths[c] > widths[widest] {
				widest = c
			}
		}
		widths[widest]--
	}

	rule := func(left, mid, right string) []span {
		parts := make([]string, n)
		for c, w := range widths {
			parts[c] = strings.Repeat("─", w+2)
		}
		return []span{{sgrDim, left + strings.Join(parts, mid) + right}}
	}
	out := [][]span{rule("┌", "┬", "┐")}
	for r := range rows {
		if r > 0 {
			out = append(out, rule("├", "┼", "┤"))
		}
		wrapped := make([][][]span, n)
		height := 1
		for c := range widths {
			wrapped[c] = wrapSpans(cells[r][c], widths[c])
			height = max(height, len(wrapped[c]))
		}
		for l := 0; l < height; l++ {
			line := []span{{sgrDim, "│"}}
			for c, w := range widths {
				var cell []span
				if l < len(wrapped[c]) {
					cell = wrapped[c][l]
				}
				a := aligns[c]
				if r == 0 {
					a = alignCenter // Claude Code centres the header
				}
				gap := w - spansCells(cell)
				left := 0
				switch a {
				case alignCenter:
					left = gap / 2
				case alignRight:
					left = gap
				}
				line = append(line, span{"", " " + strings.Repeat(" ", left)})
				line = append(line, cell...)
				line = append(line, span{"", strings.Repeat(" ", gap-left) + " "}, span{sgrDim, "│"})
			}
			out = append(out, line)
		}
	}
	return append(out, rule("└", "┴", "┘"))
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// inlineMarkdown renders emphasis, code spans and links in one line, with
// style under all of it.
func inlineMarkdown(s, style string) []span {
	var out []span
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			out = append(out, span{style, plain.String()})
			plain.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && strings.IndexByte("\\`*_[]()#+-.!|>~", s[i+1]) >= 0:
			plain.WriteByte(s[i+1])
			i += 2
			continue
		case c == '`':
			n := runLength(s, i, '`')
			if end := closingTicks(s, i+n, n); end >= 0 {
				code := s[i+n : end]
				if len(code) > 1 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
					code = code[1 : len(code)-1]
				}
				flush()
				out = append(out, span{style + sgrCode, code})
				i = end + n
				continue
			}
			plain.WriteString(s[i : i+n])
			i += n
			continue
		case c == '[':
			if text, url, end, ok := link(s, i); ok {
				flush()
				out = append(out, inlineMarkdown(text, style)...)
				out = append(out, span{style, " ("}, span{style + sgrDim, url}, span{style, ")"})
				i = end
				continue
			}
		case c == '*' || c == '_':
			run := runLength(s, i, c)
			n := min(run, 2)
			if end := closingEmphasis(s, i, n, c); end >= 0 {
				code := sgrItalic
				if n == 2 {
					code = sgrBold
				}
				flush()
				out = append(out, inlineMarkdown(s[i+n:end], style+code)...)
				i = end + n
				continue
			}
			plain.WriteString(s[i : i+run])
			i += run
			continue
		}
		plain.WriteByte(c)
		i++
	}
	flush()
	return out
}

func runLength(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingTicks finds the run of exactly n backticks that closes a code span
// opened before from.
func closingTicks(s string, from, n int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		run := runLength(s, j, '`')
		if run == n {
			return j
		}
		j += run
	}
	return -1
}

// closingEmphasis finds where the emphasis that a run of n delimiters c
// opens at i ends, or -1 when it does not open one. A delimiter opens when
// text follows it and closes when text precedes it. Single * and either _
// also need a word boundary outside, so a*b*c and snake_case_names stay.
func closingEmphasis(s string, i, n int, c byte) int {
	boundary := c == '_' || n == 1
	if i+n >= len(s) || isSpace(s[i+n]) || boundary && i > 0 && isWordByte(s[i-1]) {
		return -1
	}
	for j := i + n; j < len(s); {
		if s[j] != c {
			j++
			continue
		}
		run := runLength(s, j, c)
		if run == n && j > i+n && !isSpace(s[j-1]) && !(boundary && j+n < len(s) && isWordByte(s[j+n])) {
			return j
		}
		j += run
	}
	return -1
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// isWordByte is a letter or digit (any non-ASCII byte counts as a letter).
func isWordByte(b byte) bool {
	return b >= 0x80 || unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b))
}

// link parses [text](url) at i.
func link(s string, i int) (text, url string, end int, ok bool) {
	close := strings.IndexByte(s[i:], ']')
	if close < 0 || i+close+1 >= len(s) || s[i+close+1] != '(' {
		return "", "", 0, false
	}
	paren := strings.IndexByte(s[i+close+2:], ')')
	if paren < 0 {
		return "", "", 0, false
	}
	text, url = s[i+1:i+close], s[i+close+2:i+close+2+paren]
	if text == "" || url == "" || strings.ContainsAny(url, " \t") {
		return "", "", 0, false
	}
	return text, url, i + close + 2 + paren + 1, true
}

func spansCells(spans []span) int {
	n := 0
	for _, sp := range spans {
		n += cells(sp.text)
	}
	return n
}

// wrapSpans breaks styled text into lines of at most w cells at spaces,
// splitting a word longer than a line.
func wrapSpans(spans []span, w int) [][]span {
	type styled struct {
		r   rune
		sgr string
	}
	// Each word keeps the style of the space before it, so a code span
	// that wraps stays one colour across its words.
	var words [][]styled
	var gaps []string
	var word []styled
	gap := ""
	for _, sp := range spans {
		for _, r := range sp.text {
			if r == ' ' {
				if word != nil {
					words, word = append(words, word), nil
				}
				gap = sp.sgr
				continue
			}
			if word == nil {
				gaps = append(gaps, gap)
			}
			word = append(word, styled{r, sp.sgr})
		}
	}
	if word != nil {
		words = append(words, word)
	}
	var lines [][]styled
	var line []styled
	used := 0
	for k, wd := range words {
		wc := 0
		for _, s := range wd {
			wc += runeCells(s.r)
		}
		if used > 0 && used+1+wc > w {
			lines, line, used = append(lines, line), nil, 0
		}
		if used > 0 {
			line = append(line, styled{' ', gaps[k]})
			used++
		}
		for _, s := range wd {
			c := runeCells(s.r)
			if used+c > w && used > 0 {
				lines, line, used = append(lines, line), nil, 0
			}
			line = append(line, s)
			used += c
		}
	}
	if line != nil || len(lines) == 0 {
		lines = append(lines, line)
	}
	out := make([][]span, len(lines))
	for i, l := range lines {
		for _, s := range l {
			if n := len(out[i]); n > 0 && out[i][n-1].sgr == s.sgr {
				out[i][n-1].text += string(s.r)
			} else {
				out[i] = append(out[i], span{s.sgr, string(s.r)})
			}
		}
	}
	return out
}

// spanLine writes one line: base first, each styled span closed with a
// reset that puts base back, and a final reset that ends base.
func spanLine(spans []span, base string) string {
	var b strings.Builder
	b.WriteString(base)
	for _, sp := range spans {
		if sp.sgr == "" {
			b.WriteString(sp.text)
			continue
		}
		b.WriteString(sp.sgr + sp.text + sgrReset + base)
	}
	if base != "" {
		b.WriteString(sgrReset)
	}
	return b.String()
}
