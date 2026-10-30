package main

import (
	"math/rand"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plainMD(text string, width int) string {
	return sgrPattern.ReplaceAllString(renderMarkdown(text, width, "", false), "")
}

func TestMarkdownTable(t *testing.T) {
	src := "**Testing:**\n\n| Check | Result |\n|---|---|\n| go build | pass |\n| go test | pass (3 skipped) |\n| `go vet` | **clean** |"
	want := `Testing:

┌──────────┬──────────────────┐
│  Check   │      Result      │
├──────────┼──────────────────┤
│ go build │ pass             │
├──────────┼──────────────────┤
│ go test  │ pass (3 skipped) │
├──────────┼──────────────────┤
│ go vet   │ clean            │
└──────────┴──────────────────┘`
	if got := plainMD(src, 100); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	out := renderMarkdown(src, 100, "", false)
	for _, w := range []string{sgrBold + "Testing:" + sgrReset, sgrCode + "go vet" + sgrReset, sgrBold + "clean" + sgrReset, sgrDim + "┌──"} {
		if !strings.Contains(out, w) {
			t.Errorf("lacks %q:\n%q", w, out)
		}
	}
}

func TestMarkdownTableAlignmentEscapesAndWrapping(t *testing.T) {
	src := "| Name | Count | Note |\n|:---|:---:|---:|\n| a \\| b | 7 | a note long enough that it has to wrap inside its column |"
	want := `┌───────┬───────┬────────────────┐
│ Name  │ Count │      Note      │
├───────┼───────┼────────────────┤
│ a | b │   7   │    a note long │
│       │       │ enough that it │
│       │       │    has to wrap │
│       │       │     inside its │
│       │       │         column │
└───────┴───────┴────────────────┘`
	got := plainMD(src, 34)
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	for _, l := range strings.Split(got, "\n") {
		if cells(l) > 34 {
			t.Errorf("row is %d cells wide: %s", cells(l), l)
		}
	}
	// A word longer than its column is split, not left to the terminal.
	for _, l := range strings.Split(plainMD("| x | y |\n|-|-|\n| supercalifragilistic | 1 |", 20), "\n") {
		if cells(l) > 20 {
			t.Errorf("row is %d cells wide: %s", cells(l), l)
		}
	}
}

func TestMarkdownWideCells(t *testing.T) {
	src := "| 名前 | ok |\n|---|---|\n| 東京都 | ✅ |\n| é́ | x |"
	want := `┌────────┬────┐
│  名前  │ ok │
├────────┼────┤
│ 東京都 │ ✅ │
├────────┼────┤
│ é́      │ x  │
└────────┴────┘`
	if got := plainMD(src, 80); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMarkdownInline(t *testing.T) {
	cases := map[string]string{
		"**bold** and __bold__":           sgrBold + "bold" + sgrReset + " and " + sgrBold + "bold" + sgrReset,
		"*it* and _it_":                   sgrItalic + "it" + sgrReset + " and " + sgrItalic + "it" + sgrReset,
		"snake_case_name and a*b*c":       "snake_case_name and a*b*c",
		"2 * 3 * 4 and ** not bold **":    "2 * 3 * 4 and ** not bold **",
		"run `go test ./...` now":         "run " + sgrCode + "go test ./..." + sgrReset + " now",
		"``a ` b``":                       sgrCode + "a ` b" + sgrReset,
		"see [docs](https://x.io/a_b_c)":  "see docs (" + sgrDim + "https://x.io/a_b_c" + sgrReset + ")",
		"**bold *and it* inside**":        sgrBold + "bold " + sgrReset + sgrBold + sgrItalic + "and it" + sgrReset + sgrBold + " inside" + sgrReset,
		`\*not it\* and C:\Users`:         `*not it* and C:\Users`,
		"# Title":                         sgrBold + sgrUnderline + "Title" + sgrReset,
		"### Small":                       sgrBold + "Small" + sgrReset,
		"`**not bold** | no table`":       sgrCode + "**not bold** | no table" + sgrReset,
		"plain line, nothing to do here.": "plain line, nothing to do here.",
	}
	for src, want := range cases {
		if got := renderMarkdown(src, 80, "", false); got != want {
			t.Errorf("%q:\ngot  %q\nwant %q", src, renderMarkdown(src, 80, "", false), want)
		}
	}
}

func TestMarkdownBlocks(t *testing.T) {
	src := "- one\n  - two *it*\n    + three\n1. first\n> quoted\n***\n```go\n  x := a**b | c\n| a | b |\n|---|---|\n```\nafter"
	want := "• one\n  • two it\n    • three\n1. first\n│ quoted\n" + strings.Repeat("─", 30) +
		"\ngo\n  x := a**b | c\n| a | b |\n|---|---|\nafter"
	if got := plainMD(src, 30); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if out := renderMarkdown("```\n**x**\n```", 30, "", false); out != sgrCode+"**x**"+sgrReset {
		t.Errorf("fence: %q", out)
	}
}

// An earlier answer is dim throughout: every reset the renderer writes puts
// dim back, and each line opens with it.
func TestMarkdownKeepsTheBaseStyle(t *testing.T) {
	got := renderMarkdown("**b** x `c`\nplain", 40, sgrDim, false)
	want := sgrDim + sgrBold + "b" + sgrReset + sgrDim + " x " + sgrCode + "c" + sgrReset + sgrDim + sgrReset + "\n" +
		sgrDim + "plain" + sgrReset
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// Without colour (a pipe, a file) the answer is the Markdown as written.
func TestConvoPrintsRawMarkdownWithoutColour(t *testing.T) {
	answer := "**Testing:**\n\n| Check | Result |\n|---|---|\n| build | pass |"
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{
		{Prompt: "test it", Earlier: []string{"*first*"}, Reply: answer, Started: at, Ended: at.Add(time.Minute)},
	}}
	out := renderConvo(s, convoTarget{}, 0, false, at)
	if !strings.Contains(out, "\n▌ claude · earlier\n*first*\n") || !strings.Contains(out, "\n▌ claude · 1m\n"+answer+"\n") {
		t.Fatalf("got:\n%s", out)
	}
}

// Transcript text is cleaned before it is rendered: an escape inside a
// table cell or a code span does not survive into the output.
func TestMarkdownCannotSmuggleEscapes(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	answer := "| a |\n|---|\n| x\x1b]52;c;Zm9v\x07y |\n\n`q\x1b[2Jr` and \x1b]133;A\x1b\\"
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{{Prompt: "p", Reply: answer, Started: at, Ended: at}}}
	out := renderConvo(s, convoTarget{}, 0, true, at)
	if strings.Contains(out, "\x1b]") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Fatalf("escape survived:\n%q", out)
	}
	if !strings.Contains(out, "x]52;c;Zm9vy") || !strings.Contains(out, sgrCode+"`q[2Jr`"+sgrReset) {
		t.Fatalf("text lost:\n%q", out)
	}
}

// widest is the widest line of a rendering, in cells, styles stripped.
func widest(out string) int {
	w := 0
	for _, l := range strings.Split(sgrPattern.ReplaceAllString(out, ""), "\n") {
		w = max(w, cells(l))
	}
	return w
}

// Wide characters in columns shrunk past them used to make negative
// padding, and a panic.
func TestMarkdownTableWideCharactersInNarrowColumns(t *testing.T) {
	cases := map[string]int{
		"| a | b | c | d |\n|---|---|---|---|\n| 日本 | 日本 | 日本 | 日本 |":                                           20,
		"|" + strings.Repeat(" ✅ |", 8) + "\n|" + strings.Repeat("--:|", 8) + "\n|" + strings.Repeat(" ✅ |", 8): 40,
	}
	for src, width := range cases {
		for w := 1; w <= width; w++ {
			out := renderMarkdown(src, w, "", false)
			if got := widest(out); got > max(w, 2) {
				t.Errorf("width %d: a line is %d cells:\n%s", w, got, sgrPattern.ReplaceAllString(out, ""))
			}
		}
	}
	// Too narrow for a box: the rows as header: value lines.
	got := plainMD("| Name | Note |\n|---|---|\n| a | b c |", 9)
	if got != "Name: a\nNote: b c" {
		t.Errorf("got %q", got)
	}
}

func TestMarkdownTabsKeepColumnsAligned(t *testing.T) {
	got := plainMD("| k | v |\n|---|---|\n| a\tb | 1 |\n| abcdefgh | 2 |", 80)
	want := `┌──────────┬───┐
│    k     │ v │
├──────────┼───┤
│ a b      │ 1 │
├──────────┼───┤
│ abcdefgh │ 2 │
└──────────┴───┘`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMarkdownPathsGlobsAndURLs(t *testing.T) {
	for _, src := range []string{
		"src/**/*.go and lib/**/*.rs",
		"**/*.ts",
		"a/**/b",
		"see https://x.io/a_b_c_d/*x*/e and http://h/__init__",
		"x**.y** and 2*3*4",
	} {
		if got := renderMarkdown(src, 80, "", false); got != src {
			t.Errorf("%q became %q", src, got)
		}
	}
	if got := renderMarkdown("at https://x.io/a_b and *it*", 80, "", false); got != "at https://x.io/a_b and "+sgrItalic+"it"+sgrReset {
		t.Errorf("got %q", got)
	}
	if got := renderMarkdown("[docs](https://e.com/a_(b)_c) and (more)", 80, "", false); got != "docs ("+sgrDim+"https://e.com/a_(b)_c"+sgrReset+") and (more)" {
		t.Errorf("got %q", got)
	}
}

// A line of delimiters that never close renders in linear time.
func TestMarkdownPathologicalLinesAreFast(t *testing.T) {
	for _, unit := range []string{"_a ", "*a ", "**a ", "`a ", "[a](", "[[", "a](", "__a_ ", "http://x ", "**a**__b__"} {
		src := strings.Repeat(unit, 200_000/len(unit))
		start := time.Now()
		renderMarkdown(src, 100, "", false)
		renderMarkdown(src, 100, "", true)
		// The quadratic matcher this guards against took seconds per line;
		// the bound leaves room for slow shared CI runners.
		if d := time.Since(start); d > time.Second {
			t.Errorf("%q × %d took %v", unit, len(src)/len(unit), d)
		}
	}
}

// Random tables of ASCII, wide, emoji, combining and tab cells at random
// widths: no panic, and no line wider than the width.
func TestMarkdownTablesFitAnyWidth(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pieces := []string{"a", "word", "longerword", "日本語", "✅", "é́", "\t", " ", "**b**", "`c d`", "_i_", "x\\|y", "https://x.io/a_b", "[l](u)"}
	cell := func() string {
		var b strings.Builder
		for k := rng.Intn(6); k > 0; k-- {
			b.WriteString(pieces[rng.Intn(len(pieces))])
			if rng.Intn(2) == 0 {
				b.WriteString(" ")
			}
		}
		return b.String()
	}
	for iter := 0; iter < 1500; iter++ {
		cols := 1 + rng.Intn(6)
		row := func() string {
			cs := make([]string, cols)
			for c := range cs {
				cs[c] = cell()
			}
			return "| " + strings.Join(cs, " | ") + " |"
		}
		lines := []string{row(), "|" + strings.Repeat(":---:|", cols)}
		for r := rng.Intn(4); r > 0; r-- {
			lines = append(lines, row())
		}
		src := strings.Join(lines, "\n")
		width := 5 + rng.Intn(116)
		if got := widest(renderMarkdown(src, width, sgrDim, false)); got > width {
			t.Fatalf("width %d: a line is %d cells; source:\n%s\nrendered:\n%s", width, got, src, plainMD(src, width))
		}
	}
	for width := 10; width <= 120; width++ {
		if got := widest(renderMarkdown("x\n\n---\n\n| A | B |\n|---|---|\n| 1 | 2 |", width, "", false)); got > width {
			t.Errorf("width %d: a line is %d cells", width, got)
		}
	}
}

// A renderer that panics costs the answer its styling, not the view.
func TestConvoSurvivesARendererPanic(t *testing.T) {
	saved := markdownRenderer
	defer func() { markdownRenderer = saved }()
	markdownRenderer = func(string, int, string, bool) string { panic("bug") }
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{{Prompt: "p", Reply: "**raw** answer", Started: at, Ended: at}}}
	if out := renderConvo(s, convoTarget{}, 0, true, at); !strings.Contains(out, "**raw** answer") {
		t.Fatalf("got %q", out)
	}
}

// With markup the Markdown stays in the text under its styles, so a copy
// from the terminal pastes as Markdown.
func TestMarkdownMarkupKeepsTheSource(t *testing.T) {
	src := "**Alex's `clock_sync` owns the Jetson clock**. `kestrel-gnss-time` \"was just temporary\" and goes away."
	want := sgrBold + "**Alex's " + sgrReset + sgrBold + sgrCode + "`clock_sync`" + sgrReset + sgrBold + " owns the Jetson clock**" + sgrReset +
		". " + sgrCode + "`kestrel-gnss-time`" + sgrReset + " \"was just temporary\" and goes away."
	if got := renderMarkdown(src, 80, "", true); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for _, src := range []string{
		"## Plan ##\n\n- one *two* __three__\n  * nested `x`\n1. first\n\n> quoted **b**\n\n---\n",
		"```go\nfunc f() { return **x** }\n```\nafter",
		"~~~\nunclosed fence\n**x**",
		"see [the docs](https://e.com/a_(b)_c) and https://x.io/a_b",
		`escaped \*not italic\* and \` + "`not code\\`",
		"src/**/*.go and x**.y** and 2*3*4",
		"_a **b _c_ d** e_ and ``a ` b``",
	} {
		if got := sgrPattern.ReplaceAllString(renderMarkdown(src, 30, sgrDim, true), ""); got != src {
			t.Errorf("source not kept:\ngot  %q\nwant %q", got, src)
		}
	}
	if got := renderMarkdown("# Title", 80, "", true); got != sgrBold+"# Title"+sgrReset {
		t.Errorf("heading: %q", got)
	}
}

// A table stays a pipe table: aligned when it fits, as written when not.
func TestMarkdownMarkupTable(t *testing.T) {
	src := "| Check | Result |\n|:---|---:|\n| `go vet` | **clean** |\n| a\\|b | 1 |"
	want := "| Check    |    Result |\n" +
		"| :------- | --------: |\n" +
		"| `go vet` | **clean** |\n" +
		"| a\\|b     |         1 |"
	if got := sgrPattern.ReplaceAllString(renderMarkdown(src, 80, "", true), ""); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	narrow := "| Check | Result |\n| :-- | --: |\n| `go vet` | **clean** |\n| a\\|b | 1 |"
	if got := sgrPattern.ReplaceAllString(renderMarkdown(src, 20, "", true), ""); got != narrow {
		t.Fatalf("narrow: got\n%s\nwant\n%s", got, narrow)
	}
}

// Random Markdown-ish text: with markup, stripping the styles gives the
// source back, whatever the delimiters do.
func TestMarkdownMarkupRoundTrips(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"*", "**", "_", "__", "`", "``", "[", "]", "(", ")", "\\", "#", "- ", "> ", "a", "b ", " ", "\n", "https://x.io/a_b ", "```\n", "日"}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := rng.Intn(40); n > 0; n-- {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		src := b.String()
		if got := sgrPattern.ReplaceAllString(renderMarkdown(src, 1+rng.Intn(60), "", true), ""); got != src {
			t.Fatalf("source not kept:\ngot  %q\nwant %q", got, src)
		}
	}
}

// Neighbouring spans in one style are written in one pass: merging them by
// concatenation copied the line once per span, gigabytes for a long line.
func TestMarkdownMarkupMergesSpansInLinearMemory(t *testing.T) {
	src := strings.Repeat("**a**__b__", 20_000)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	renderMarkdown(src, 100, "", true)
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 100<<20 {
		t.Errorf("rendering %d bytes allocated %d MB", len(src), got>>20)
	}
}
