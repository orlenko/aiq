package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const markdownContextFixture = "# Heading\n\n> quote **bold [*label* `code`](https://example.invalid/a_b)**\n- 日本語 é 👩‍💻\n\n```go\n  x := 1\n\tdeep code words longer than width\n```\n***\n| **Name** | Value |\n|:---|---:|\n| [`x`](u) | *italic* |\n| a \\| b | 日本語\t✅ |"

func markdownTestPalettes() []convoPalette {
	return []convoPalette{darkPalette, lightPalette,
		{dim: "\x1b[38;5;101m", code: "\x1b[38;5;102m", accent: "\x1b[38;5;103m"}, {}}
}

func markdownContextOutput(md markdownContext, width int) []byte {
	// Only strings/slices are marshaled; retaining row boundaries and nilness
	// makes this a byte oracle for both line and screen rendering.
	data, err := json.Marshal([]any{
		md.renderMarkdown(markdownContextFixture, width, "", false),
		md.renderMarkdown(markdownContextFixture, width, sgrDim, true),
		md.renderMarkdownRows(markdownContextFixture, width, sgrDim),
	})
	if err != nil {
		panic(err)
	}
	return data
}

func TestMarkdownContextPreservesTerminalBytes(t *testing.T) {
	// Literal digests captured from the pre-refactor renderer, covering both
	// markup modes, base restoration, recursion, tables, tabs and narrow rows.
	cases := []struct {
		palette, width int
		digest         string
	}{
		{0, 0, "58eea7bd9df70edc894c30ce7e7e8c369aa785b0a8424fe0900650fc0f8f3fc1"},
		{0, 1, "58eea7bd9df70edc894c30ce7e7e8c369aa785b0a8424fe0900650fc0f8f3fc1"},
		{0, 7, "736ffd93886cdce631162bbb7ba262eee8edafbfdc1257a4cc97dcaf17ad2c24"},
		{0, 24, "918372a7fdf77b43842e84a08f1a5a667babddcca79068742ccb4d10136af444"},
		{0, 80, "10fda8ea9328b34a0ca45d7e05e9381f8185dfb84a91b1b1036ebe334292ea35"},
		{1, 0, "86eab97e585913f5f58f26262a7c0c84fbb720704b6287d55bb7df4312d47de7"},
		{1, 1, "86eab97e585913f5f58f26262a7c0c84fbb720704b6287d55bb7df4312d47de7"},
		{1, 7, "6570b0aab6c1ba444bee56ea99136fcef76d1a15501743bc8dc3361df279ebe3"},
		{1, 24, "62e8a6d68c311464437094309938cb585fbdf2f6434bb3caf8adb6420131f7d0"},
		{1, 80, "daefb9dbde0f9b5c6c87bccae8cfc2e60ffe60f8eacaefd43e8585f3ff72f2b7"},
		{2, 0, "2ee70ec605d68f16526618a717925770318d38350f61bd51c04e16eae89e97ea"},
		{2, 1, "2ee70ec605d68f16526618a717925770318d38350f61bd51c04e16eae89e97ea"},
		{2, 7, "33e76f7105ee1de148190c38c5a30612f86441edd1316efea70b4aac82fd1e06"},
		{2, 24, "529203c39f822a6b3bece44ead98e4d8a11d1be72102fb0d5257cd734034b5c1"},
		{2, 80, "8a436fb9dfbef5034acc250996e3df422b6669e38fbafbbca45a3b02dcc9967f"},
		{3, 0, "df9fe648281db1cb0d3bec0bb3535c777623ef4e83249ab4b0c1da0ec964408d"},
		{3, 1, "df9fe648281db1cb0d3bec0bb3535c777623ef4e83249ab4b0c1da0ec964408d"},
		{3, 7, "06484af7326635bf2dc74d805e7cec81c951dbc4b1602f00e571f916cedf8b88"},
		{3, 24, "92249cb14ca70bdca2a8bbf8c405a61433a844068a1942c0960c790ef3bd2b05"},
		{3, 80, "4b05d60ea384c3712c19389b3cbe59695860bf40987b97804816da289b34d759"},
	}
	saved := pal
	defer func() { pal = saved }()
	palettes := markdownTestPalettes()
	for _, tc := range cases {
		md := markdownContext{palette: palettes[tc.palette]}
		// An unrelated ambient choice must not affect the explicit path. Zero
		// is a deliberate supplied palette, not a request for CLI defaults.
		pal = palettes[(tc.palette+1)%len(palettes)]
		got := markdownContextOutput(md, tc.width)
		if sum := fmt.Sprintf("%x", sha256.Sum256(got)); sum != tc.digest {
			t.Errorf("palette %d width %d: digest %s, want %s; output %s", tc.palette, tc.width, sum, tc.digest, got)
		}
		pal = palettes[tc.palette]
		legacy, err := json.Marshal([]any{
			renderMarkdown(markdownContextFixture, tc.width, "", false),
			renderMarkdown(markdownContextFixture, tc.width, sgrDim, true),
			renderMarkdownRows(markdownContextFixture, tc.width, sgrDim),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, legacy) {
			t.Errorf("legacy wrapper differs: palette %d width %d", tc.palette, tc.width)
		}
		if !reflect.DeepEqual(md.markdownSpans(markdownContextFixture, tc.width, true), markdownSpans(markdownContextFixture, tc.width, true)) {
			t.Errorf("legacy span wrapper differs: palette %d width %d", tc.palette, tc.width)
		}
	}
}

func TestMarkdownContextOwnsRecursiveStyles(t *testing.T) {
	palette := convoPalette{code: "\x1b[38;5;102m", dim: "\x1b[38;5;101m", accent: "\x1b[38;5;103m"}
	md := markdownContext{palette: palette}
	// Changing the original value or CLI palette after construction cannot
	// change the code/URL styles inside nested link labels and emphasis.
	palette.code = "\x1b[31m"
	saved := pal
	defer func() { pal = saved }()
	pal = palette
	got := md.renderMarkdown("**[`code`](url)**", 80, "", false)
	want := sgrBold + "\x1b[38;5;102m" + "code" + sgrReset + sgrBold + " (" + sgrReset + sgrBold + "\x1b[38;5;101m" + "url" + sgrReset + sgrBold + ")" + sgrReset
	if got != want {
		t.Fatalf("nested styles: got %q, want %q", got, want)
	}
	if strings.Contains(got, palette.code) || strings.Contains(got, md.palette.accent) {
		t.Fatalf("unrelated style in %q", got)
	}
}

func TestMarkdownContextsRenderConcurrently(t *testing.T) {
	palettes := markdownTestPalettes()
	widths := []int{1, 7, 24, 80}
	type fixture struct {
		md    markdownContext
		width int
		want  []byte
	}
	var fixtures []fixture
	for _, p := range palettes {
		for _, width := range widths {
			md := markdownContext{palette: p}
			fixtures = append(fixtures, fixture{md, width, markdownContextOutput(md, width)})
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range fixtures {
		wg.Add(1)
		go func(f fixture) {
			defer wg.Done()
			<-start
			for i := 0; i < 30; i++ {
				if got := markdownContextOutput(f.md, f.width); !bytes.Equal(got, f.want) {
					t.Errorf("concurrent render differs at width %d", f.width)
					return
				}
			}
		}(f)
	}
	close(start)
	wg.Wait()
}
