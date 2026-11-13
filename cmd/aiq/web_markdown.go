package main

// Each role uses a distinct complete component, including roles that share a
// color in the terminal. Return a value so callers cannot mutate shared styles.
func webMarkdownPalette() convoPalette {
	return convoPalette{
		band: "\x1b[48;5;201m", you: "\x1b[38;5;202m", agent: "\x1b[38;5;203m",
		dim: "\x1b[38;5;204m", accent: "\x1b[38;5;205m", code: "\x1b[38;5;206m",
	}
}

type webMarkdownSpan struct {
	text       string
	classes    []string
	decorative bool
}

type webMarkdownLine struct {
	kind      string
	spans     []webMarkdownSpan
	tableRows int
}

// webMarkdownLines is a same-input Markdown primitive, not a conversation
// paint adapter. Do not concatenate folded copy text to erase cut visibility
// or parse boundaries. Its actual consumer must use its approved render guard.
func webMarkdownLines(raw string, width int, markup bool) []webMarkdownLine {
	lines := (markdownContext{palette: webMarkdownPalette()}).markdownLines(sanitize(raw), width, markup)
	if lines == nil {
		return nil
	}
	out := make([]webMarkdownLine, len(lines))
	for i, line := range lines {
		out[i] = webMarkdownLine{kind: line.kind, tableRows: line.tableRows}
		if line.spans == nil {
			continue
		}
		out[i].spans = make([]webMarkdownSpan, len(line.spans))
		for j, span := range line.spans {
			classes, _ := webStyleClasses(span.sgr)
			out[i].spans[j] = webMarkdownSpan{text: span.text, classes: classes, decorative: span.decorative}
		}
	}
	return out
}

// webStyleClasses consumes trusted style components only, never body text.
// Coverage is explicit for the producer tests: unknown components retain their
// text without inventing classes; malformed bytes stop parsing, not resync it.
func webStyleClasses(style string) ([]string, bool) {
	var classes []string
	covered := true
	seen := map[string]bool{}
	for style != "" {
		if len(style) < 3 || style[:2] != "\x1b[" {
			return classes, false
		}
		end := 2
		for end < len(style) && (style[end] >= '0' && style[end] <= '9' || style[end] == ';') {
			end++
		}
		if end == 2 || end >= len(style) || style[end] != 'm' {
			return classes, false
		}
		component := style[:end+1]
		style = style[end+1:]
		class := ""
		switch component {
		case "\x1b[1m":
			class = "b"
		case "\x1b[2m":
			class = "dim"
		case "\x1b[3m":
			class = "i"
		case "\x1b[4m":
			class = "u"
		case "\x1b[7m":
			class = "inverse"
		case "\x1b[48;5;201m":
			class = "band"
		case "\x1b[38;5;202m":
			class = "you"
		case "\x1b[38;5;203m":
			class = "agent"
		case "\x1b[38;5;204m":
			class = "muted"
		case "\x1b[38;5;205m":
			class = "accent"
		case "\x1b[38;5;206m":
			class = "code"
		default:
			covered = false
		}
		if class != "" && !seen[class] {
			seen[class] = true
			classes = append(classes, class)
		}
	}
	return classes, covered
}
