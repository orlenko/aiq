package main

import (
	"strings"
	"testing"
	"time"
)

func TestKitLineFillsTheWidth(t *testing.T) {
	for _, w := range []int{1, 10, 45, 200} {
		got := kitLine([]kitSeg{{style: sgrBold, text: "aa\x1b[2Jbb"}, {text: "\x1b[1mcc\x1b[0m", raw: true}}, "\x1b[48;5;254m", w)
		if strings.Contains(got, "\x1b[2J") {
			t.Fatalf("an escape in plain text got through: %q", got)
		}
		if n := cells(kitPlain(got)); n != w {
			t.Fatalf("width %d: %d cells in %q", w, n, got)
		}
	}
}

func TestKitMarksFallBack(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "▶"},
		{map[string]string{"LANG": "en_CA.UTF-8"}, "▶"},
		{map[string]string{"LC_ALL": "C", "LANG": "en_CA.UTF-8"}, ">"},
		{map[string]string{"LC_CTYPE": "en_US.utf8", "LANG": "C"}, "▶"},
		{map[string]string{"LANG": "C"}, ">"},
	}
	for _, c := range cases {
		if got := kitMarksFor(func(k string) string { return c.env[k] }).selected; got != c.want {
			t.Fatalf("%v: %q", c.env, got)
		}
	}
}

func TestKitTintFollowsTheBackground(t *testing.T) {
	saved, savedBg := pal, termBackground
	defer func() { pal, termBackground = saved, savedBg }()
	t.Setenv("COLORTERM", "truecolor")
	pal = lightPalette
	termBackground.r, termBackground.g, termBackground.b, termBackground.ok = 0.99, 0.96, 0.89, true
	if th := kitThemeFor(false); !strings.HasPrefix(th.itemBand, "\x1b[48;2;") || th.itemBand == th.selBand {
		t.Fatalf("%+v", th)
	}
	t.Setenv("COLORTERM", "")
	if th := kitThemeFor(false); th.itemBand != kitLight.itemBand {
		t.Fatalf("without 24-bit colour the bands are the 256 greys: %q", th.itemBand)
	}
	termBackground.ok = false
	t.Setenv("COLORTERM", "truecolor")
	if th := kitThemeFor(false); th.itemBand != kitLight.itemBand {
		t.Fatalf("with no background reported the bands are the 256 greys: %q", th.itemBand)
	}
	if th := kitThemeFor(true); th.itemBand != "" || th.ink != "" {
		t.Fatalf("no colour: %+v", th)
	}
}

func TestAsksCodeAndTablesKeepTheirSpacing(t *testing.T) {
	rows := renderMarkdownRows("```\n  indented code\n```\n\n| a | b |\n|---|---|\n| one | two |", 40, "")
	joined := kitPlain(strings.Join(rows, "\n"))
	if !strings.Contains(joined, "  indented code") || !strings.Contains(joined, "one") {
		t.Fatalf("%q", joined)
	}
	if !strings.Contains(joined, "┌") {
		t.Fatalf("a table should be drawn: %q", joined)
	}
	for _, row := range renderMarkdownRows(strings.Repeat("word ", 40)+strings.Repeat("x", 90), 30, "") {
		if n := cells(kitPlain(row)); n > 30 {
			t.Fatalf("row of %d cells: %q", n, row)
		}
	}
}

// Dismissing or restoring an item keeps the selection where it was: on the next item down,
// else the one above.

// Joined emoji, skin tones and the emoji variation selector count at least as wide as
// terminals draw them, and a joined emoji stays one glyph.
func TestKitLineCountsEmojiAsOneGlyph(t *testing.T) {
	for text, want := range map[string]int{"👩\u200d💻": 4, "👍🏽": 2, "❤\ufe0f": 2, "雪": 2, "e\u0301": 1, "ab": 2,
		"❤\ufe0f\u200d🔥": 4, "foo\u200dbar": 6, strings.Repeat("A\u200d", 12): 12, "👩\u200dA": 3,
		strings.Repeat("\u2318\u200d", 20): 20, "\u2318\u200d\u2318": 2,
		"👩\u200d" + strings.Repeat("\u2318\u200d", 20): 22, "👨\u200d👩\u200d👧\u200d👦": 8,
		"\u270c\U0001f3fd": 2, "\u261d\U0001f3fd": 2, "\u270d\U0001f3fd": 2, "\u270c\ufe0f\U0001f3fd": 2, "a\U0001f3fd": 3,
		strings.Repeat("👩\u200d", 20): 8 + 16*2} {
		if got := cells(text); got != want {
			t.Errorf("cells(%q) = %d, want %d", text, got, want)
		}
		if got := cells(kitPlain(kitLine([]kitSeg{{text: text}}, "", 10))); got != 10 {
			t.Errorf("kitLine(%q) is %d cells", text, got)
		}
	}
	if got := truncate("👩\u200d💻👩\u200d💻👩\u200d💻", 9); got != "👩\u200d💻👩\u200d💻…" {
		t.Errorf("truncate cut inside a glyph: %q", got)
	}
}

// A long word of emoji splits between glyphs and keeps each row within its width; a tab
// after a joined emoji stops where the glyph's cells say.
func TestPlainWrapSplitsOnGlyphs(t *testing.T) {
	for _, word := range []string{strings.Repeat("\u2764\ufe0f", 6), strings.Repeat("👩\u200d💻", 8)} {
		for _, row := range wrap(word, 10) {
			if cells(row) > 10 || strings.HasPrefix(row, "\u200d") || strings.HasPrefix(row, "💻") || strings.HasPrefix(row, "\ufe0f") {
				t.Fatalf("wrap(%q): row %q, %d cells", word, row, cells(row))
			}
		}
	}
	family := "👨\u200d👩\u200d👧\u200d👦"
	if got := truncate(family, 5); got != "…" {
		t.Fatalf("truncate(family, 5) = %q", got)
	}
	for _, row := range wrap("aaaaaa"+family, 10) {
		if strings.Contains(row, "\u200d") && !strings.Contains(row, family) {
			t.Fatalf("the family split: %q", wrap("aaaaaa"+family, 10))
		}
	}
	// An indent that leaves less room than one glyph needs still moves on.
	done := make(chan []string, 1)
	go func() { done <- wrap("     "+family+family, 10) }()
	select {
	case rows := <-done:
		if strings.Join(rows, "") != family+family {
			t.Fatalf("indented family rows %q", rows)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wrap did not finish")
	}
	if got := expandTabs("x\t\u0301z"); strings.Contains(got, "\t") {
		t.Fatalf("a tab with a mark after it stayed a tab: %q", got)
	}
	if got := expandTabs("👩\u200d💻\tx"); got != "👩\u200d💻    x" {
		t.Fatalf("expandTabs: %q", got)
	}
}

// A code line longer than the width is cut, keeping its indentation and every space.
func TestWrappedCodeKeepsItsSpaces(t *testing.T) {
	code := "    aa  bb          cc   dd"
	rows := renderMarkdownRows("```\n"+code+"\n```", 10, "")
	joined := ""
	for _, r := range rows {
		p := kitPlain(r)
		if cells(p) > 10 {
			t.Fatalf("row of %d cells: %q", cells(p), p)
		}
		joined += p
	}
	if joined != code {
		t.Fatalf("code changed: %q", joined)
	}
}

// Wrapping keeps a joined emoji whole, as the column widths count it.
func TestWrapKeepsAGlyphWhole(t *testing.T) {
	for _, w := range []int{1, 2, 3, 10} {
		for _, row := range wrapSpans([]span{{"", "a 👩\u200d💻 b", false}}, w) {
			for _, sp := range row {
				if strings.HasPrefix(sp.text, "\u200d") || strings.HasPrefix(sp.text, "💻") || strings.HasSuffix(sp.text, "\u200d") {
					t.Fatalf("width %d split the glyph: %q", w, sp.text)
				}
			}
		}
	}
	// A table too narrow for a joined emoji keeps its rows within its borders.
	for _, row := range markdownSpans("| x |\n|---|\n| 👩\u200d💻 |", 6, false) {
		text := ""
		for _, sp := range row {
			text += sp.text
		}
		if c := cells(text); c > 6 {
			t.Fatalf("a row of %d cells in a 6-cell table: %q", c, text)
		}
	}
	rows := renderMarkdownRows("| x |\n|---|\n| 👩\u200d💻 |", 40, "")
	if !strings.Contains(kitPlain(strings.Join(rows, "\n")), "👩\u200d💻") {
		t.Fatalf("table split the glyph: %q", rows)
	}
}

// A cut never leaves a joiner before the ellipsis.
func TestTruncateDropsADanglingJoiner(t *testing.T) {
	got := truncate(strings.Repeat("A\u200d", 12), 10)
	if strings.Contains(got, "\u200d…") || cells(got) != 10 {
		t.Fatalf("%q", got)
	}
}
