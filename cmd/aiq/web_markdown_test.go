package main

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestWebMarkdownStyleClasses(t *testing.T) {
	for _, tc := range []struct {
		style   string
		classes []string
		covered bool
	}{
		{"", nil, true},
		{"\x1b[1m\x1b[2m\x1b[3m\x1b[4m\x1b[7m", []string{"b", "dim", "i", "u", "inverse"}, true},
		{"\x1b[48;5;201m\x1b[38;5;202m\x1b[38;5;203m\x1b[38;5;204m\x1b[38;5;205m\x1b[38;5;206m", []string{"band", "you", "agent", "muted", "accent", "code"}, true},
		{"\x1b[38;5;206m\x1b[1m\x1b[38;5;206m\x1b[1m", []string{"code", "b"}, true},
		{"\x1b[31m\x1b[3m", []string{"i"}, false},
		{"\x1b[1;7m\x1b[2m", []string{"dim"}, false}, // do not split numeric fragments
		{"\x1b[1m\x1b[38;5;999m\x1b[4m", []string{"b", "u"}, false},
		{"literal\x1b[1m", nil, false},
		{"\x1b[1m!\x1b[3m", []string{"b"}, false},
		{"\x1b[1X\x1b[3m", nil, false},
		{"\x1b[m\x1b[3m", nil, false},
		{"\x1b[1m\x1b[3", []string{"b"}, false},
		{"\x1b[38;5;206m\x00\x1b[1m", []string{"code"}, false},
	} {
		classes, covered := webStyleClasses(tc.style)
		if covered != tc.covered || !reflect.DeepEqual(classes, tc.classes) {
			t.Errorf("style %q: %v/%v want %v/%v", tc.style, classes, covered, tc.classes, tc.covered)
		}
	}
	want := convoPalette{band: "\x1b[48;5;201m", you: "\x1b[38;5;202m", agent: "\x1b[38;5;203m", dim: "\x1b[38;5;204m", accent: "\x1b[38;5;205m", code: "\x1b[38;5;206m"}
	p := webMarkdownPalette()
	if p != want {
		t.Fatalf("fixed palette: %+v", p)
	}
	p.code, p.dim = "changed", "changed"
	if webMarkdownPalette() != want {
		t.Fatal("palette factory shared mutable state")
	}
}

func TestWebMarkdownLiteralLines(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		width int
		marks bool
		want  []webMarkdownLine
	}{
		{"", 4, false, []webMarkdownLine{{kind: "text"}}},
		{"# h", 5, false, []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "h", classes: []string{"b", "u"}}}}}},
		{"# h", 5, true, []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "# h", classes: []string{"b"}}}}}},
		{"```\n```", 4, false, nil},
		{"---", 3, false, []webMarkdownLine{{kind: "rule", spans: []webMarkdownSpan{{text: "───", classes: []string{"muted"}, decorative: true}}}}},
		{"---", -2, false, []webMarkdownLine{{kind: "rule", spans: []webMarkdownSpan{{text: "─", classes: []string{"muted"}, decorative: true}}}}},
		{"---", 3, true, []webMarkdownLine{{kind: "rule", spans: []webMarkdownSpan{{text: "---", classes: []string{"muted"}}}}}},
		{"> q", 3, false, []webMarkdownLine{{kind: "quote", spans: []webMarkdownSpan{{text: "│ ", classes: []string{"muted"}, decorative: true}, {text: "q"}}}}},
		{"> q", 3, true, []webMarkdownLine{{kind: "quote", spans: []webMarkdownSpan{{text: "> ", classes: []string{"muted"}}, {text: "q"}}}}},
		{"`code`", 2, false, []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "code", classes: []string{"code"}}}}}},
		{"**bold**", 2, false, []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "bold", classes: []string{"b"}}}}}},
		{"```go\n<b>\n```", 3, false, []webMarkdownLine{{kind: "code", spans: []webMarkdownSpan{{text: "go", classes: []string{"muted"}}}}, {kind: "code", spans: []webMarkdownSpan{{text: "<b>", classes: []string{"code"}}}}}},
		{"<b>\x00\u0085\u202e\x1b[1m\x1b]52;c;fixture\a\u2028\xff", 5, false, []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "<b>[1m]52;c;fixture"}}}, {kind: "text", spans: []webMarkdownSpan{{text: "�"}}}}},
	} {
		if got := webMarkdownLines(tc.raw, tc.width, tc.marks); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("raw %q marks %v: got %#v want %#v", tc.raw, tc.marks, got, tc.want)
		}
	}
}

// Independent component vocabulary: terminal roles may coincide, but every
// emitted component must be accounted for under the supplied palette.
func metadataStyleCovered(style string, p convoPalette) bool {
	components := []string{"\x1b[1m", "\x1b[2m", "\x1b[3m", "\x1b[4m", "\x1b[7m", p.band, p.you, p.agent, p.dim, p.accent, p.code}
	for style != "" {
		consumed := false
		for _, component := range components {
			if component != "" && strings.HasPrefix(style, component) {
				style, consumed = style[len(component):], true
				break
			}
		}
		if !consumed {
			return false
		}
	}
	return true
}

func checkMetadataLines(t *testing.T, lines []markdownLine, palette convoPalette, marks bool) {
	t.Helper()
	for _, line := range lines {
		switch line.kind {
		case "text", "table", "rule", "quote", "code":
		default:
			t.Fatalf("unknown kind %q", line.kind)
		}
		if line.tableRows < 0 || line.kind != "table" && line.tableRows != 0 {
			t.Fatalf("invalid row count %+v", line)
		}
		for _, sp := range line.spans {
			if !metadataStyleCovered(sp.sgr, palette) {
				t.Fatalf("unaccounted trusted style %q", sp.sgr)
			}
			if marks && sp.decorative {
				t.Fatal("marks-mode decoration")
			}
		}
	}
}

func checkWebMarkdownPrimitive(t *testing.T, raw string, width int, marks bool) []webMarkdownLine {
	t.Helper()
	got := webMarkdownLines(raw, width, marks)
	if !reflect.DeepEqual(got, webMarkdownLines(raw, width, marks)) {
		t.Fatal("nondeterministic web lines")
	}
	p := webMarkdownPalette()
	md := markdownContext{palette: p}
	lines := md.markdownLines(sanitize(raw), width, marks)
	checkMetadataLines(t, lines, p, marks)
	if len(got) != len(lines) || (got == nil) != (lines == nil) {
		t.Fatal("web line/empty boundaries changed")
	}
	vocabulary := map[string]bool{"b": true, "dim": true, "i": true, "u": true, "inverse": true, "band": true, "you": true, "agent": true, "muted": true, "accent": true, "code": true}
	for i, line := range got {
		if line.kind != lines[i].kind || line.tableRows != lines[i].tableRows || len(line.spans) != len(lines[i].spans) {
			t.Fatal("web metadata/spans changed")
		}
		for j, sp := range line.spans {
			original := lines[i].spans[j]
			if _, covered := webStyleClasses(original.sgr); !covered {
				t.Fatalf("producer emitted uncovered style %q", original.sgr)
			}
			if sp.text != original.text || sp.decorative != original.decorative || !utf8.ValidString(sp.text) || strings.ContainsAny(sp.text, "\x00\x1b\r\u202e\u2066") {
				t.Fatal("web text/flag boundary changed")
			}
			seen := map[string]bool{}
			for _, class := range sp.classes {
				if !vocabulary[class] || seen[class] {
					t.Fatalf("untrusted/repeated class %q", class)
				}
				seen[class] = true
			}
		}
	}
	return got
}

func TestWebMarkdownOwnershipConcurrencyAndCoverage(t *testing.T) {
	raw := "> **bold** [x](url)\n```go\n<code>\n```\n|h|\n|-|\n|body|"
	oldPalette, oldMarkup, oldRenderer := pal, convoMarkup, markdownRenderer
	defer func() { pal, convoMarkup, markdownRenderer = oldPalette, oldMarkup, oldRenderer }()
	pal, convoMarkup = convoPalette{dim: "poison", code: "poison"}, true
	markdownRenderer = func(string, int, string, bool) string { panic("ambient renderer used") }
	var baseline [2][]webMarkdownLine
	var metadata [2][]markdownLine
	contexts := []markdownContext{{palette: darkPalette}, {palette: convoPalette{dim: "\x1b[38;5;101m", code: "\x1b[38;5;102m"}}}
	for i := range baseline {
		baseline[i] = checkWebMarkdownPrimitive(t, raw, 17+i*20, i != 0)
		metadata[i] = contexts[i].markdownLines(raw, 17+i*20, i != 0)
	}
	changedMetadata := contexts[0].markdownLines(raw, 17, false)
	for i := range changedMetadata {
		changedMetadata[i].kind, changedMetadata[i].tableRows = "changed", -1
		for j := range changedMetadata[i].spans {
			changedMetadata[i].spans[j] = span{sgr: "changed", text: "changed", decorative: true}
		}
	}
	if !reflect.DeepEqual(contexts[0].markdownLines(raw, 17, false), metadata[0]) {
		t.Fatal("primary metadata storage shared across calls")
	}
	mutated := webMarkdownLines(raw, 17, false)
	for i := range mutated {
		mutated[i].kind, mutated[i].tableRows = "changed", -1
		for j := range mutated[i].spans {
			sp := &mutated[i].spans[j]
			sp.text, sp.decorative = "changed", !sp.decorative
			for k := range sp.classes {
				sp.classes[k] = "changed"
			}
			sp.classes = append(sp.classes, "appended")
		}
		mutated[i].spans = append(mutated[i].spans, webMarkdownSpan{text: "appended"})
	}
	if !reflect.DeepEqual(webMarkdownLines(raw, 17, false), baseline[0]) {
		t.Fatal("returned storage shared across calls")
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for job := 0; job < 12; job++ {
		i := job % 2
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 4; n++ {
				if !reflect.DeepEqual(webMarkdownLines(raw, 17+i*20, i != 0), baseline[i]) || !reflect.DeepEqual(contexts[i].markdownLines(raw, 17+i*20, i != 0), metadata[i]) {
					t.Error("concurrent explicit context changed")
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, fixture := range metadataParityFixtures {
		for _, marks := range []bool{false, true} {
			checkWebMarkdownPrimitive(t, fixture.raw, fixture.width, marks)
			for _, p := range []convoPalette{darkPalette, lightPalette, {}, webMarkdownPalette()} {
				checkMetadataLines(t, (markdownContext{palette: p}).markdownLines(fixture.raw, fixture.width, marks), p, marks)
			}
		}
	}
}

func FuzzMarkdownMetadata(f *testing.F) {
	for _, seed := range metadataParityFixtures {
		f.Add(seed.raw, uint8(max(seed.width, 1)), false)
		f.Add(seed.raw, uint8(max(seed.width, 1)), true)
	}
	f.Fuzz(func(t *testing.T, raw string, requested uint8, marks bool) {
		if len(raw) > 512 {
			t.Skip()
		}
		width, clean := int(requested%64)+1, sanitize(raw)
		md := markdownContext{palette: webMarkdownPalette()}
		lines := md.markdownLines(clean, width, marks)
		checkMetadataLines(t, lines, md.palette, marks)
		if !reflect.DeepEqual(lines, md.markdownLines(clean, width, marks)) {
			t.Fatal("nondeterministic metadata")
		}
		var rendered []string
		for _, line := range lines {
			rendered = append(rendered, spanLine(line.spans, ""))
		}
		if strings.Join(rendered, "\n") != md.renderMarkdown(clean, width, "", marks) {
			t.Fatal("same-input primitive parity")
		}
		// Controlled prefix makes first-line row-count position observable even
		// when unrelated fuzz text contains more tables or empty code blocks.
		prefix := md.markdownLines("|h|\n|-|\n|v|\n\n"+clean, 30, marks)
		length := 5
		if marks {
			length = 3
		}
		for i := 0; i < length; i++ {
			want := 0
			if i == 0 {
				want = 2
			}
			if prefix[i].kind != "table" || prefix[i].tableRows != want {
				t.Fatal("first-table count position changed")
			}
		}
	})
}

func FuzzWebMarkdownLines(f *testing.F) {
	for _, raw := range []string{"", "**open", "```go\nbody", "|a|\n|-|\n|b|", "\x1b]52;c;fixture\a\u202e\xff", "é 👩‍💻"} {
		f.Add(raw, uint8(17), false)
		f.Add(raw, uint8(1), true)
	}
	f.Fuzz(func(t *testing.T, raw string, requested uint8, marks bool) {
		if len(raw) > 512 {
			t.Skip()
		}
		width := int(requested%64) + 1
		checkWebMarkdownPrimitive(t, raw, width, marks)
		projection := projectConvoCuts("", []convoCut{
			{key: "fixture", kind: "answer", text: raw}, {key: "fixture", kind: "more", text: "tail\n```"},
			{key: "fixture", kind: "part", text: "invisible **"}, {key: "fixture", kind: "none"},
		}, nil)
		selected := 0
		for _, part := range projection[0].parts {
			if part.kind == "answer" || part.kind == "more" {
				checkWebMarkdownPrimitive(t, part.text, width, marks)
				selected++
			}
		}
		if selected != 2 {
			t.Fatal("selected visible cut boundary changed")
		}
		// Deliberately no joined folded-body paint-equivalence assertion: each
		// selected answer/continuation keeps its own Markdown parse boundary.
	})
}
