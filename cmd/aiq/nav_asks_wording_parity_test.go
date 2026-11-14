package main

import (
	"reflect"
	"strings"
	"testing"
)

// Freeze existing terminal/primitive facts before extracting paint preparation.
// Expected text is literal; it is never passed through the producer under test.
func TestNavAsksWordingLegacyParity(t *testing.T) {
	cases := []struct {
		name, raw, copy string
		terminal, web   []string
	}{
		{"code suffix", "```\nx" + strings.Repeat(" ", 19) + "\n```", "> ```\n> x\n> ```\n\n",
			[]string{"x" + strings.Repeat(" ", 9)}, []string{"x" + strings.Repeat(" ", 19)}},
		{"blank code tail", "```\nx\n \n```", "> ```\n> x\n>\n> ```\n\n", []string{"x"}, []string{"x", " "}},
		{"empty fence", "```\n```", "> ```\n> ```\n\n", []string{}, []string{}},
		{"rule", "---", "> ---\n\n", []string{strings.Repeat("─", 10)}, []string{strings.Repeat("─", 10)}},
		{"empty quote", ">", "> >\n\n", []string{"│ "}, []string{"│ "}},
		{"ragged table", "a|b|c\n---|---|---\nx|y\nx|y", "> a|b|c\n> ---|---|---\n> x|y\n> x|y\n\n",
			[]string{"a: x", "b: y", "c:", "", "a: x", "b: y", "c:"},
			[]string{"a: x", "b: y", "c:", "", "a: x", "b: y", "c:"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newAsksScreen()
			b.noColor = true
			rows := b.asksQuoteRows(tc.raw, true, 10, "")
			plain := make([]string, 0, len(rows))
			for _, row := range rows {
				plain = append(plain, kitPlain(row))
			}
			if !reflect.DeepEqual(plain, tc.terminal) {
				t.Fatalf("terminal: %#v, want %#v", plain, tc.terminal)
			}
			lines, covered := webMarkdownLinesChecked(tc.raw, 10, false)
			web := make([]string, 0, len(lines))
			for _, line := range lines {
				var text strings.Builder
				for _, span := range line.spans {
					text.WriteString(span.text)
				}
				web = append(web, text.String())
			}
			if !covered || !reflect.DeepEqual(web, tc.web) {
				t.Fatalf("web: %#v, covered %v, want %#v", web, covered, tc.web)
			}
			if got := quoteForReply(sanitize(tc.raw)); got != tc.copy {
				t.Fatalf("copy: %q, want %q", got, tc.copy)
			}
		})
	}
}
