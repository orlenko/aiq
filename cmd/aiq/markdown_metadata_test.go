package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Frozen terminal output from the renderer before metadata was introduced.
// The three palettes cover coincident terminal styles, zero, and distinct roles.
var metadataParityFixtures = []struct {
	name, raw string
	width     int
	digests   [3]string
}{
	{"empty", "", 27, [3]string{"de02c9b04670e7c5c9af3c5c3db07232dffd1051a78d3952e58c23871b1883b6", "de02c9b04670e7c5c9af3c5c3db07232dffd1051a78d3952e58c23871b1883b6", "de02c9b04670e7c5c9af3c5c3db07232dffd1051a78d3952e58c23871b1883b6"}},
	{"empty-fence", "```\n```", 27, [3]string{"1c522e9efd26256001ae4f786f7df66f16419cc4c1436e509e254c05f221eea6", "eceed18a1856990f31109c8d592269b2bbf33c87b3c71ac81e7587927fa0338c", "7be134d28810134a47b4d1a7b7f10d839236ede8fd27a212e2e531ecdd543ae0"}},
	{"blank-code", "```\n\n```", 27, [3]string{"3e12a2613e41a6bca7c249e08b845ccbc6af8e562926970a02f312bad280e86c", "9b3b917cc14763bb9fff83c0bfebb7f7c1746d91571425f2bd3e1eb99cd345de", "966742f9802f2fe3f2bca61d17a41753c3edff0d38ed4d5ac9b5de8e39343f0e"}},
	{"unterminated", "```go\n  x  y", 5, [3]string{"3709af3530a01b826e2914943ec5f47cf0b212f0646680a6b42abd3a6554e387", "e4da582c6ad7697553413df412cd01af9cde2b77428c8ba857614036a792095c", "c8cf1f8d97e9028fc900480bdb583b66d2bc2cb7eeb4b7505100ebe001848922"}},
	{"inline-code-row", "`inline  code  words  here`", 7, [3]string{"d62dd07e69318444ef834b134d5ef66cecd29abf1b5e12c17cafb2c7bc668d61", "64d2c95e467983cfb65babd828913c5c0daa5950c6c8e03f4a2af68f999b6dc5", "dbf1176c2dff4f53f5caa83755ee9d943066cb9255367ee869d658b9b1be4b21"}},
	{"quote-rule", "  > **a** [link](https://example.invalid)\n***\nnot │ decoration", 13, [3]string{"9f2123365664ce6b06fa6e5a160f5c4e0ef9eb4a859f520927f5553b50bfde9b", "93d95cc77e6e86a92f659b0806c9ed87375715e816080067531f8d9e8e56f45d", "6db857137d19946a6ad98a11c3f6c698dc560189170ed1a9820cf7b7b7db47be"}},
	{"fallback-table", "| h | v |\n|---|---|\n| x | y |\n\n| z | w |\n|---|---|\n| a | b |", 1, [3]string{"14d53e86d3c420b17864036179dea1384637977be10babe6a6ced883712e7c0a", "7a06a085bc3c9f8fcfcee9a3345655593e2293c34b1c43222b28b92c323b962a", "8d1d20e63ebfdeb6d37207b8ff26c298c43287d85c243822da15254789904114"}},
	{"wrapped-table", "| head | other |\n|:---|---:|\n| many words in one cell | 日本語 |\n| a \\| b | **bold** |", 19, [3]string{"ff30d29157d50c8223a2da82f8161a7701692c588a64851a934dbb1c7ba4560e", "523e5da39991e49695f9938aabfaedfddcd5a37997f7a54bb1da92a196fe9a88", "79e4cc5893e9df49aa0546d42ca44fceda71f1771a4ead157f1766bfa2675a4e"}},
	{"header-table", "| a | b |\n|---|---|", 100, [3]string{"6d1c7d4e5732fe8d4729149f0d845bcfc788b70663dd85befccf49c86151ae0c", "132939ae160f4cbd54130b779d38a90b0c6cf397553809a7f14a5fe94230ee47", "db063137a0531519c814036bbdf61ebff9f2dabd347cd3e1375e2526a94a1d46"}},
	{"greedy-table", "| a | b |\n|---|---|\n| x | y |\n| c | d |\n|---|---|\n| e | f |", 25, [3]string{"ed159645fae52cead742029c4a6211c9d2b041ede78040ae2c549ec51625db19", "23fb90e591e3ed91d024e4d3dfc1c9b2bafc4facb0e6c179ea9dc01c2b20135c", "a08a8baff68db14b736c4f9323cdbdf254b47caadc0c5330a05f9300bedd6af1"}},
	{"mixed", "```lang\ncode  │\n```\n\n> q\n\n---\n\n| a | b |\n|---|---|\n| x | y |", 100, [3]string{"783535b4d52c90c60472e9e644b0e19524becd3e9e8aa4856fd0233ed508c947", "476f6fb17db09c1337b3bf7bd0c2508dd80014c29bc809091f7430fe76b0debf", "f69e580e59b4cb2e207de1aabdb7d1f5fc6b409ce3348d553cfed9f9465b45dc"}},
	{"unicode", "- 日本語 é 👩‍💻\n**bold *nested* `code` [link](u)**\n\tend", 4, [3]string{"57cfc64963d3a6e5b3dbff75d1d7f3c3bd1957fb38e05ab15989c2c78435b85f", "2454b0b7c0b1677d80efa15448637b06f2f3ace10ff2de3f1744240af10ece2d", "14dedf777488d72f04ad726aad0e8825fffb9d9c2db32cab3054aeb83e06735d"}},
	{"trailing", "end\n", 27, [3]string{"3f114992dc8a82511032943e6d14bb56978c86282db369663a39342d7e366065", "3f114992dc8a82511032943e6d14bb56978c86282db369663a39342d7e366065", "3f114992dc8a82511032943e6d14bb56978c86282db369663a39342d7e366065"}},
	{"negative-width", "## hi", -1, [3]string{"d7d9e5868f12a49a5769eb88650a56fc8df59b611730e961336ec590f244e639", "d7d9e5868f12a49a5769eb88650a56fc8df59b611730e961336ec590f244e639", "d7d9e5868f12a49a5769eb88650a56fc8df59b611730e961336ec590f244e639"}},
	{"empty-fence-trailing", "```\n```\n", 8, [3]string{"3af76e779b76e71bb3f35dc7fd1a6c8ee20e3ed4246542a063c94ddd3c750cb2", "2ab895d040957f44c9a18e38112a8fb031c0c3ff24772a1a3b3364bd1ab6908f", "bd3305b77df247394442ccc5ab63eea903d09c9a4f12152a43132a36a290d7b3"}},
}

func TestMarkdownMetadataOld45Vectors(t *testing.T) {
	palettes := []convoPalette{darkPalette, {}, {dim: "\x1b[38;5;101m", code: "\x1b[38;5;102m", accent: "\x1b[38;5;103m"}}
	for _, fixture := range metadataParityFixtures {
		for i, palette := range palettes {
			md := markdownContext{palette: palette}
			payload, err := json.Marshal([]any{
				md.renderMarkdown(fixture.raw, fixture.width, "", false),
				md.renderMarkdown(fixture.raw, fixture.width, sgrDim, true),
				md.renderMarkdownRows(fixture.raw, fixture.width, sgrDim),
			})
			if err != nil {
				t.Fatal(err)
			}
			if digest := fmt.Sprintf("%x", sha256.Sum256(payload)); digest != fixture.digests[i] {
				t.Errorf("%s palette %d: %s want %s; output %s", fixture.name, i, digest, fixture.digests[i], payload)
			}
		}
	}
}

func metadataText(spans []span, decoratedOnly bool) string {
	var out strings.Builder
	for _, sp := range spans {
		if !decoratedOnly || sp.decorative {
			out.WriteString(sp.text)
		}
	}
	return out.String()
}

func TestMarkdownMetadataLiteralKinds(t *testing.T) {
	md := markdownContext{palette: convoPalette{dim: "\x1b[38;5;101m", code: "\x1b[38;5;102m"}}
	for _, tc := range []struct {
		raw          string
		marks        bool
		kinds, texts []string
	}{
		{"", false, []string{"text"}, []string{""}},
		{"```\n```", false, nil, nil},
		{"```\n```", true, []string{"code", "code"}, []string{"```", "```"}},
		{"```\n\n```", false, []string{"code"}, []string{""}},
		{"```go\n\n```", false, []string{"code", "code"}, []string{"go", ""}},
		{"```go\n\n```", true, []string{"code", "code", "code"}, []string{"```go", "", "```"}},
		{"```go\n  x", false, []string{"code", "code"}, []string{"go", "  x"}},
		{"# h\n- b\n> q\n---\nordinary │ ─ >", false, []string{"text", "text", "quote", "rule", "text"}, []string{"h", "• b", "│ q", "────────", "ordinary │ ─ >"}},
		{"# h\n- b\n> q\n---", true, []string{"text", "text", "quote", "rule"}, []string{"# h", "- b", "> q", "---"}},
	} {
		lines := md.markdownLines(tc.raw, 8, tc.marks)
		var kinds, texts []string
		for _, line := range lines {
			kinds, texts = append(kinds, line.kind), append(texts, metadataText(line.spans, false))
			if line.tableRows != 0 {
				t.Fatal("non-table acquired row count")
			}
		}
		if !reflect.DeepEqual(kinds, tc.kinds) || !reflect.DeepEqual(texts, tc.texts) {
			t.Fatalf("%q marks %v: kinds %v texts %q", tc.raw, tc.marks, kinds, texts)
		}
		legacy := md.markdownSpans(tc.raw, 8, tc.marks)
		if (legacy == nil) != (lines == nil) || len(legacy) != len(lines) {
			t.Fatal("span adapter changed nilness/line boundaries")
		}
		for i := range lines {
			if !reflect.DeepEqual(legacy[i], lines[i].spans) {
				t.Fatal("span adapter changed spans")
			}
		}
	}
	// Whole inline code is text metadata, but the Rows adapter must keep its
	// historical code-span hard-cut rule, including every repeated space.
	if line := md.markdownLines("`a  b  c`", 3, false); len(line) != 1 || line[0].kind != "text" {
		t.Fatal("inline code became a block")
	}
	want := []string{"\x1b[38;5;102ma  \x1b[0m", "\x1b[38;5;102mb  \x1b[0m", "\x1b[38;5;102mc\x1b[0m"}
	if got := md.renderMarkdownRows("`a  b  c`", 3, ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("inline code rows: %q want %q", got, want)
	}
}

func TestMarkdownMetadataTablesAndDecoration(t *testing.T) {
	md := markdownContext{palette: convoPalette{dim: "\x1b[38;5;101m", code: "\x1b[38;5;102m"}}
	for _, tc := range []struct {
		raw    string
		width  int
		marks  bool
		kinds  []string
		counts []int
	}{
		{"| a | b |\n|---|---|", 100, false, []string{"table", "table", "table"}, []int{1, 0, 0}},
		{"| a | b |\n|---|---|", 100, true, []string{"table", "table"}, []int{1, 0}},
		{"| a | b |\n|---|---|", 1, false, []string{"text", "text"}, []int{0, 0}},
		{"|a|\n|-|\n|b|\n\n|c|\n|-|\n|d|", 30, true, []string{"table", "table", "table", "text", "table", "table", "table"}, []int{2, 0, 0, 0, 2, 0, 0}},
		{"|a|b|\n|-|-|\n|x|y|\n|c|d|\n|-|-|\n|e|f|", 30, true, []string{"table", "table", "table", "table", "table", "table"}, []int{5, 0, 0, 0, 0, 0}},
		{"|a|\n|-|\n|b|\n```go\nx\n```", 30, true, []string{"table", "table", "table", "code", "code", "code"}, []int{2, 0, 0, 0, 0, 0}},
	} {
		lines := md.markdownLines(tc.raw, tc.width, tc.marks)
		var kinds []string
		var counts []int
		for _, line := range lines {
			kinds, counts = append(kinds, line.kind), append(counts, line.tableRows)
		}
		if !reflect.DeepEqual(kinds, tc.kinds) || !reflect.DeepEqual(counts, tc.counts) {
			t.Fatalf("table %q: kinds %v counts %v", tc.raw, kinds, counts)
		}
	}
	box := md.markdownLines("|a|\n|-|\n|│|", 20, false)
	wantBorders := []string{"┌───┐", "││", "├───┤", "││", "└───┘"}
	if len(box) != len(wantBorders) {
		t.Fatalf("box lines: %+v", box)
	}
	for i, line := range box {
		if got := metadataText(line.spans, true); got != wantBorders[i] {
			t.Errorf("border %d: %q want %q", i, got, wantBorders[i])
		}
	}
	if metadataText(box[3].spans, false) != "│ │ │" {
		t.Fatal("literal border cell/padding changed")
	}
	quote := md.markdownLines("> [q](url)\n---\n- b\n```go\n│\n```\n│ ordinary", 8, false)
	wantDecoration := []string{"│ ", "────────", "", "", "", ""}
	if len(quote) != len(wantDecoration) {
		t.Fatalf("quote lines: %+v", quote)
	}
	for i, line := range quote {
		if got := metadataText(line.spans, true); got != wantDecoration[i] {
			t.Fatalf("decoration %d: %q", i, got)
		}
	}
	for _, fixture := range metadataParityFixtures {
		for _, line := range md.markdownLines(fixture.raw, fixture.width, true) {
			for _, sp := range line.spans {
				if sp.decorative {
					t.Fatalf("marks mode hid content in %s", fixture.name)
				}
			}
		}
	}
}

func TestMarkdownMetadataWrapFlagsAndBytes(t *testing.T) {
	dim := "\x1b[2m"
	input := []span{{sgr: dim, text: "aa"}, {sgr: dim, text: " ", decorative: true}, {sgr: dim, text: "bb"}}
	want := [][]span{{{sgr: dim, text: "aa"}, {sgr: dim, text: " ", decorative: true}, {sgr: dim, text: "bb"}}}
	if got := wrapSpans(input, 5); !reflect.DeepEqual(got, want) {
		t.Fatalf("equal-style gap lost flag: %#v", got)
	}
	if got := spanLine(want[0], "\x1b[7m"); got != "\x1b[7m\x1b[2maa bb\x1b[0m\x1b[7m\x1b[0m" {
		t.Fatalf("flag-only boundary introduced reset: %q", got)
	}
	if got := wrapSpans(input, 2); !reflect.DeepEqual(got, [][]span{{{sgr: dim, text: "aa"}}, {{sgr: dim, text: "bb"}}}) {
		t.Fatalf("gap on wrap: %#v", got)
	}
	glyphs := []span{{sgr: dim, text: "é👩‍💻", decorative: true}, {sgr: dim, text: "界"}}
	wrapped := wrapSpans(glyphs, 2)
	expected := [][]span{{{sgr: dim, text: "é", decorative: true}}, {{sgr: dim, text: "👩‍💻", decorative: true}}, {{sgr: dim, text: "界"}}}
	if !reflect.DeepEqual(wrapped, expected) {
		t.Fatalf("glyph/flag split: %#v", wrapped)
	}
	if !reflect.DeepEqual(input, []span{{sgr: dim, text: "aa"}, {sgr: dim, text: " ", decorative: true}, {sgr: dim, text: "bb"}}) {
		t.Fatal("wrap changed input")
	}
}
