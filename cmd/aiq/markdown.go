package main

import (
	"regexp"
	"sort"
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
	width = max(width, 1)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = expandTabs(l)
	}
	var out [][]span
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if fence := codeFence(trimmed); fence != "" {
			if lang := strings.TrimSpace(strings.TrimLeft(trimmed, fence[:1])); lang != "" {
				out = append(out, []span{{pal.dim, lang}})
			}
			for i++; i < len(lines); i++ {
				if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == "" {
					break
				}
				out = append(out, []span{{pal.code, lines[i]}})
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
			out = append(out, []span{{pal.dim, strings.Repeat("─", width)}})
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
			out = append(out, append([]span{{pal.dim, m[1] + "│ "}}, inlineMarkdown(m[2], "")...))
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

// expandTabs turns tabs into spaces to the next stop of 4 cells: a tab
// counted as one cell would throw table columns out of line.
func expandTabs(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var b strings.Builder
	col := 0
	for _, r := range line {
		if r == '\t' {
			n := 4 - col%4
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col += runeCells(r)
	}
	return b.String()
}

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
// width, shrinking the widest columns and wrapping their cells to fit. A
// column is never narrower than its widest character; when the box cannot
// fit at those widths, each row becomes "header: value" lines.
func renderTable(rows [][]string, aligns []align, width int) [][]span {
	n := len(aligns)
	cells := make([][][]span, len(rows))
	natural := make([]int, n)
	least := make([]int, n)
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
			natural[c] = max(natural[c], 1, spansCells(cells[r][c]))
			least[c] = max(least[c], widestRune(cells[r][c]))
		}
	}
	room := width - (3*n + 1) // borders and a space either side of each cell
	widths := make([]int, n)
	for c := range widths {
		widths[c] = max(natural[c], least[c])
		least[c] = min(widths[c], max(least[c], 2)) // shrink no column below 2 cells
	}
	if room < sum(least) {
		return tableAsLines(cells, width)
	}
	for total := sum(widths); total > room; total-- {
		widest := -1
		for c := range widths {
			if widths[c] > least[c] && (widest < 0 || widths[c] > widths[widest]) {
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
		return []span{{pal.dim, left + strings.Join(parts, mid) + right}}
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
			line := []span{{pal.dim, "│"}}
			for c, w := range widths {
				var cell []span
				if l < len(wrapped[c]) {
					cell = wrapped[c][l]
				}
				a := aligns[c]
				if r == 0 {
					a = alignCenter // Claude Code centres the header
				}
				gap := max(0, w-spansCells(cell))
				left := 0
				switch a {
				case alignCenter:
					left = gap / 2
				case alignRight:
					left = gap
				}
				line = append(line, span{"", " " + strings.Repeat(" ", left)})
				line = append(line, cell...)
				line = append(line, span{"", strings.Repeat(" ", gap-left) + " "}, span{pal.dim, "│"})
			}
			out = append(out, line)
		}
	}
	return append(out, rule("└", "┴", "┘"))
}

// tableAsLines is a table too wide for any box: each body row as
// "header: value" lines wrapped to width, rows apart by a blank line.
func tableAsLines(cells [][][]span, width int) [][]span {
	var out [][]span
	for r := 1; r < len(cells); r++ {
		if r > 1 {
			out = append(out, nil)
		}
		for c, value := range cells[r] {
			line := append(append([]span(nil), cells[0][c]...), span{sgrBold, ":"}, span{"", " "})
			out = append(out, wrapSpans(append(line, value...), width)...)
		}
	}
	if len(cells) == 1 {
		for _, head := range cells[0] {
			out = append(out, wrapSpans(head, width)...)
		}
	}
	return out
}

// widestRune is the most cells one character of spans takes.
func widestRune(spans []span) int {
	w := 0
	for _, sp := range spans {
		for _, r := range sp.text {
			w = max(w, runeCells(r))
		}
	}
	return w
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// inlineMarkdown renders emphasis, code spans and links in one line, with
// style under all of it. It indexes the line once, so a line full of
// delimiters that never close costs no more than one that is plain.
func inlineMarkdown(s, style string) []span {
	ix := indexInline(s)
	return ix.render(0, len(s), style)
}

// inlineIndex is where each kind of closing delimiter sits in one line.
type inlineIndex struct {
	s string
	// closers holds, for each emphasis delimiter (* or _, run of 1 or 2),
	// the sorted positions of runs that can close it.
	closers map[emphasisKey][]int
	ticks   map[int][]int // backtick runs by length, sorted
	bracket []int         // the next ] at or after each position, else -1
	urls    []int         // where each bare http(s):// run ends, by start; -1 elsewhere
}

type emphasisKey struct {
	c byte
	n int
}

func indexInline(s string) *inlineIndex {
	ix := &inlineIndex{s: s, closers: map[emphasisKey][]int{}, ticks: map[int][]int{}, bracket: make([]int, len(s)+1)}
	for j := 0; j < len(s); {
		c := s[j]
		if c != '*' && c != '_' && c != '`' {
			j++
			continue
		}
		run := runLength(s, j, c)
		if c == '`' {
			ix.ticks[run] = append(ix.ticks[run], j)
		} else if run <= 2 && canClose(s, j, run, c) {
			ix.closers[emphasisKey{c, run}] = append(ix.closers[emphasisKey{c, run}], j)
		}
		j += run
	}
	next := -1
	for j := len(s); j >= 0; j-- {
		if j < len(s) && s[j] == ']' {
			next = j
		}
		ix.bracket[j] = next
	}
	return ix
}

// first is the first position in list after from, if it lies before limit.
func first(list []int, from, limit int) int {
	k := sort.SearchInts(list, from+1)
	if k < len(list) && list[k] < limit {
		return list[k]
	}
	return -1
}

func (ix *inlineIndex) render(lo, hi int, style string) []span {
	s := ix.s
	var out []span
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			out = append(out, span{style, plain.String()})
			plain.Reset()
		}
	}
	for i := lo; i < hi; {
		c := s[i]
		switch {
		case c == '\\' && i+1 < hi && strings.IndexByte("\\`*_[]()#+-.!|>~", s[i+1]) >= 0:
			plain.WriteByte(s[i+1])
			i += 2
			continue
		case (c == 'h' || c == 'H') && bareURL(s, i):
			// A URL is written as it is: its _ and * are not emphasis.
			end := i
			for end < hi && !isSpace(s[end]) {
				end++
			}
			plain.WriteString(s[i:end])
			i = end
			continue
		case c == '`':
			n := runLength(s, i, '`')
			if end := first(ix.ticks[n], i, hi); end >= 0 {
				code := s[i+n : end]
				if len(code) > 1 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
					code = code[1 : len(code)-1]
				}
				flush()
				out = append(out, span{style + pal.code, code})
				i = end + n
				continue
			}
			plain.WriteString(s[i : i+n])
			i += n
			continue
		case c == '[':
			if textEnd, urlStart, urlEnd, ok := ix.link(i, hi); ok {
				flush()
				out = append(out, ix.render(i+1, textEnd, style)...)
				out = append(out, span{style, " ("}, span{style + pal.dim, s[urlStart:urlEnd]}, span{style, ")"})
				i = urlEnd + 1
				continue
			}
		case c == '*' || c == '_':
			run := runLength(s, i, c)
			n := min(run, 2)
			if canOpen(s, i, n, c) {
				// The closer must leave something between: past i+n.
				if end := first(ix.closers[emphasisKey{c, n}], i+n, hi); end >= 0 && end+n <= hi {
					code := sgrItalic
					if n == 2 {
						code = sgrBold
					}
					flush()
					out = append(out, ix.render(i+n, end, style+code)...)
					i = end + n
					continue
				}
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

// canOpen says a run of n delimiters c at i opens emphasis: text follows
// it, and a slash on neither side (src/**/*.go is a path). Single * and
// either _ also need a word boundary before, so a*b*c and snake_case stay;
// none opens straight after a letter when punctuation follows it.
func canOpen(s string, i, n int, c byte) bool {
	if i+n >= len(s) {
		return false
	}
	next, prev := s[i+n], byte(' ')
	if i > 0 {
		prev = s[i-1]
	}
	switch {
	case isSpace(next) || next == '/' || prev == '/':
		return false
	case (c == '_' || n == 1) && isWordByte(prev):
		return false
	case isWordByte(prev) && isPunct(next):
		return false
	}
	return true
}

// canClose is canOpen mirrored, for a run of n delimiters c at j.
func canClose(s string, j, n int, c byte) bool {
	if j == 0 {
		return false
	}
	prev, next := s[j-1], byte(' ')
	if j+n < len(s) {
		next = s[j+n]
	}
	switch {
	case isSpace(prev) || prev == '/' || next == '/':
		return false
	case (c == '_' || n == 1) && isWordByte(next):
		return false
	case isWordByte(next) && isPunct(prev):
		return false
	}
	return true
}

func bareURL(s string, i int) bool {
	rest := strings.ToLower(s[i:min(len(s), i+8)])
	return strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://")
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// isWordByte is a letter or digit (any non-ASCII byte counts as a letter).
func isWordByte(b byte) bool {
	return b >= 0x80 || unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b))
}

func isPunct(b byte) bool {
	return b < 0x80 && unicode.IsPunct(rune(b)) || b < 0x80 && unicode.IsSymbol(rune(b))
}

// linkDestinationMax bounds how far a link's (url) is looked for, so a
// line of unclosed "[x](" stays cheap.
const linkDestinationMax = 2048

// link parses [text](url) at i, within hi: the text ends at textEnd, the
// URL spans urlStart..urlEnd, and its parentheses may nest.
func (ix *inlineIndex) link(i, hi int) (textEnd, urlStart, urlEnd int, ok bool) {
	s := ix.s
	close := ix.bracket[i]
	if close < 0 || close+1 >= hi || s[close+1] != '(' || close == i+1 {
		return 0, 0, 0, false
	}
	depth := 0
	for j := close + 2; j < hi && j < close+2+linkDestinationMax; j++ {
		switch s[j] {
		case ' ', '\t':
			return 0, 0, 0, false
		case '(':
			depth++
		case ')':
			if depth == 0 {
				if j == close+2 {
					return 0, 0, 0, false
				}
				return close, close + 2, j, true
			}
			depth--
		}
	}
	return 0, 0, 0, false
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
