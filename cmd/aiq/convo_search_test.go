package main

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/transcript"
)

func searchType(c *convoScreen, text string) {
	var tok = asksTokenizer{literal: true}
	for _, e := range tok.feed([]byte(text)) {
		c.input(e)
	}
}

func TestConvoSearchSelectsAndFocusesMessage(t *testing.T) {
	s := screenTurns(4, func(i int) string {
		if i == 0 || i == 2 {
			return "A NEEDLE in this answer."
		}
		return "Another answer."
	})
	c := screenOf(false, 80, 18, s)
	sel, top := c.sel, c.top
	searchType(c, "/needle")
	if c.search == nil || !reflect.DeepEqual(c.search.matches, []int{1, 5}) || c.sel != sel || c.top != top {
		t.Fatalf("search %+v, original view moved to %d/%d", c.search, c.sel, c.top)
	}
	frame := strings.Join(plainFrame(c), "\n")
	if !strings.Contains(frame, "A NEEDLE in this answer.") || !strings.Contains(frame, "/needle") || !strings.Contains(frame, "2 matches") {
		t.Fatalf("search frame:\n%s", frame)
	}
	press(c, kDown, keypress{code: keyEnter})
	if c.search != nil || c.sel != 5 || !c.visible(5) {
		t.Fatalf("Enter selected %d, search %+v", c.sel, c.search)
	}
	// A second search is local and starts with an empty query. Escape keeps the view.
	sel, top = c.sel, c.top
	searchType(c, "/prompt")
	if c.search.query != "prompt" || len(c.search.matches) != 4 {
		t.Fatalf("second search %+v", c.search)
	}
	if press(c, keypress{code: keyBack}); c.search != nil || c.sel != sel || c.top != top {
		t.Fatal("Escape quit or changed the view")
	}
	if !press(c, kQuitChar) {
		t.Fatal("q no longer quits outside search")
	}
}

func TestConvoSearchLiteralEditingAndBounds(t *testing.T) {
	c := screenOf(false, 60, 14, screenTurns(1, func(int) string { return "q j k h l g G 日本語 👩‍💻" }))
	searchType(c, "/q j k h l g G 日本語 👩‍💻")
	if c.search == nil || c.search.query != "q j k h l g G 日本語 👩‍💻" || len(c.search.matches) != 1 {
		t.Fatalf("navigation aliases became actions: %+v", c.search)
	}
	searchType(c, "\x7f\x08")
	if c.search.query != "q j k h l g G 日本語 👩" || !utf8.ValidString(c.search.query) {
		t.Fatalf("backspace split UTF-8: %q", c.search.query)
	}
	searchType(c, strings.Repeat("x", convoSearchLimit+20))
	if utf8.RuneCountInString(c.search.query) != convoSearchLimit || !strings.Contains(c.message, "limited") {
		t.Fatalf("unbounded query %d, status %q", utf8.RuneCountInString(c.search.query), c.message)
	}
	before := c.search.query
	searchType(c, "\x1b[200~q\rignored\x1b[201~")
	if c.search.query != before || c.search == nil || c.message != "Pasted text does nothing here" {
		t.Fatal("paste acted as query keys")
	}
	if !press(c, keypress{code: keyQuit}) {
		t.Fatal("Ctrl-C/D must quit during search")
	}
}

func TestConvoSearchEmptyAndNoMatchDoNotMove(t *testing.T) {
	c := screenOf(false, 80, 16, screenTurns(3, nil))
	for _, query := range []string{"", "not-present"} {
		sel, top := c.sel, c.top
		searchType(c, "/"+query)
		press(c, kUp, kDown, keypress{code: keyEnter})
		if c.search != nil || c.sel != sel || c.top != top {
			t.Fatalf("%q moved original view", query)
		}
		if query != "" && c.message != "No matching messages" {
			t.Fatalf("no-match result %q", c.message)
		}
	}
	searchType(c, "/\x7f") // erase on an empty field
	if c.search == nil || c.search.query != "" {
		t.Fatal("empty backspace canceled search")
	}
}

func TestConvoSearchFocusesPassageAcrossWraps(t *testing.T) {
	for _, phrase := range []string{"matching words across rows", "日本語abcdefghijklmnopqrstuv"} {
		s := screenTurns(2, func(i int) string {
			return strings.Repeat("earlier line\n", 80) + phrase + " final text"
		})
		c := screenOf(false, 22, 14, s)
		searchType(c, "/"+phrase)
		if len(c.search.matches) != 2 {
			t.Fatal("wrapped phrase lost from copy text")
		}
		press(c, keypress{code: keyEnter})
		if c.sel != 1 || c.top <= c.msgs[1].from+30 || strings.Contains(c.message, "Match in message text") {
			t.Fatalf("%q was not focused: sel %d top %d, %q", phrase, c.sel, c.top, c.message)
		}
		if !strings.Contains(strings.Join(plainFrame(c), "\n"), strings.Split(phrase, " ")[0]) {
			// The first token itself can be glyph-wrapped; row location still has to be near it.
			if !strings.Contains(phrase, "日本語") {
				t.Fatalf("focused frame misses %q", phrase)
			}
		}
	}
}

func TestConvoSearchFocusSkipsTrustedHeader(t *testing.T) {
	s := screenTurns(1, nil)
	s.Turns[0].Prompt = strings.Repeat("earlier line\n", 80) + "you found the actual body"
	c := screenOf(false, 24, 14, s)
	searchType(c, "/you")
	press(c, keypress{code: keyEnter})
	if c.sel != 0 || c.top < c.msgs[0].body+50 || !strings.Contains(strings.Join(plainFrame(c), "\n"), "you found") {
		t.Fatalf("focused header instead of body: sel %d top %d", c.sel, c.top)
	}
}

// A real later hit must win over fragments separated by any hard boundary.
// Display wrapping remains joinable at the precise glyph/space break it added.
func TestConvoSearchFocusPreservesHardBreaks(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, query string
		width               int
		folded              bool
	}{
		{"hard glyph boundary", "need\nle", "needle", 78, false},
		{"hard space boundary", "matching\nwords", "matching words", 78, false},
		{"blank line boundary", "need\n\nle", "needle", 78, false},
		{"styled hard boundary", "\x1b[31mneed\x1b[0m\nle", "needle", 78, false},
		{"Unicode hard boundary", "東\n京", "東京", 78, false},
		{"hard boundary beside glyph wraps", "need\nle", "needle", 4, false},
		{"hard boundary beside word wraps", "matching\nwords", "matching words", 9, false},
		{"folded continuation boundary", "need\nle", "needle", 78, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			padding := strings.Repeat("filler\n", 80)
			body := tc.prefix + "\n" + padding + tc.query
			c := newConvoScreen(false)
			var text string
			var cuts []convoCut
			if tc.folded {
				first, rest, _ := strings.Cut(body, "\n")
				text = first + "\n" + rest + "\n"
				cuts = []convoCut{{end: len(first) + 1, kind: "line", text: first}, {end: len(text), kind: "more", text: rest}}
			} else {
				text = body + "\n"
				cuts = []convoCut{{end: len(text), kind: "line", text: sanitize(body)}}
			}
			c.cut(0, text, cuts, tc.width)
			if row := c.searchRow(0, tc.query); row < 80 {
				t.Fatalf("focused manufactured early hit at row %d", row)
			}
			searchType(c, "/"+tc.query)
			press(c, keypress{code: keyEnter})
			if c.top < 60 || c.message != "" {
				t.Fatalf("did not focus real rendered match: top %d, %q", c.top, c.message)
			}
		})
	}
}

func TestConvoWrapMetadataRestoresOnlyDisplayBreaks(t *testing.T) {
	for _, text := range []string{"", "a\nb", "need\nle\nneedle", "one two   three four", "abcdefghijklmnop", "日本語の長い文章", "\x1b[31mstyled long words\x1b[0m", "need\n\nle"} {
		for _, width := range []int{2, 4, 9, 78} {
			var got strings.Builder
			for i, line := range strings.Split(text, "\n") {
				if i > 0 {
					got.WriteByte('\n')
				}
				for _, row := range convoWrapParts(line, width) {
					if row.wrap {
						got.WriteString(row.gap)
					} else if row.gap != "" {
						t.Fatal("hard boundary carries a wrap gap")
					}
					got.WriteString(kitPlain(row.text))
				}
			}
			if got.String() != kitPlain(text) {
				t.Fatalf("width %d restored %q as %q", width, text, got.String())
			}
		}
	}
}

func TestConvoSearchIncludesFoldedHiddenContinuations(t *testing.T) {
	for _, query := range []string{"tail", "invisible"} {
		text, cuts := foldFixture(0)
		c := newConvoScreen(false)
		c.cut(0, text, cuts, 70)
		searchType(c, "/"+query)
		if len(c.search.matches) != 1 || c.search.matches[0] != 1 {
			t.Fatalf("%q did not find folded owner: %+v", query, c.search)
		}
		if !strings.Contains(strings.ToLower(strings.Join(plainFrame(c), "\n")), query) {
			t.Fatal("matching continuation absent from snippet")
		}
		press(c, keypress{code: keyEnter})
		if c.sel != 1 || (query == "invisible") != strings.Contains(c.message, "Match in message text:") {
			t.Fatalf("%q focus %d, status %q", query, c.sel, c.message)
		}
	}
}

func TestConvoSearchFollowResizeAndRewrite(t *testing.T) {
	full := screenTurns(5, func(int) string { return "matching answer" })
	upTo := func(n int) *transcript.Session {
		s := *full
		s.Turns = full.Turns[:n]
		return &s
	}
	c := screenOf(true, 80, 16, upTo(3))
	searchType(c, "/matching")
	press(c, kDown)
	id, original, top := c.search.id, c.msgs[c.sel].id, c.top
	c.apply(convoUpdate{s: upTo(5)})
	c.resize(42, 16)
	if c.search.id != id || len(c.search.matches) != 5 || c.msgs[c.sel].id != original || c.following() {
		t.Fatalf("follow/resize moved selection: search %+v sel %d top %d (was %d)", c.search, c.sel, c.top, top)
	}
	press(c, keypress{code: keyEnter})
	if c.msgs[c.sel].id != id || c.following() || c.newBelow() != 4 {
		t.Fatalf("focused wrong live result %d, %d new", c.sel, c.newBelow())
	}
	press(c, kLast)
	if !c.following() || c.newBelow() != 0 {
		t.Fatal("G did not resume following")
	}
	searchType(c, "/matching")
	press(c, kDown, kDown, kDown, kDown)
	c.apply(convoUpdate{s: upTo(1)})
	if len(c.search.matches) != 1 || c.search.sel != 0 {
		t.Fatalf("rewrite retained stale result: %+v", c.search)
	}
	press(c, keypress{code: keyEnter})
	if c.sel != 1 {
		t.Fatal("rewritten result no longer navigable")
	}
}

func TestConvoSearchRespectsLastAndFrameSafety(t *testing.T) {
	c := newConvoScreen(false)
	c.w, c.h = 80, 16
	s := screenTurns(3, nil)
	c.apply(convoUpdate{s: s, from: 2, begin: true})
	searchType(c, "/prompt 0")
	if len(c.search.matches) != 0 {
		t.Fatal("search escaped --last cutoff")
	}
	for _, w := range []int{1, 10, 12, 40, 80} {
		c.resize(w, 16)
		for _, line := range c.frame() {
			if cells(kitPlain(line)) != w {
				t.Fatalf("width %d frame row %q", w, line)
			}
		}
	}
	if got := convoSearchSnippet("İİİ before 東京 after\x1b]52;c;fixture\a", "東京"); !strings.Contains(got, "東京") || strings.ContainsAny(got, "\x1b\a") {
		t.Fatalf("unsafe or byte-misaligned Unicode snippet %q", got)
	}
}

// Reader-side modal prediction sees the same order even when events arrive in a
// single read and the screen cannot consume them yet. Only a real quit may bypass it.
func TestConvoSearchReaderDoesNotQuitForQueryKeys(t *testing.T) {
	for _, data := range []string{"/q j k g G\x7f\r", "/q\x1b", "/\x1b[200~q\r\x1b[201~q"} {
		t.Run(data, func(t *testing.T) {
			keys, events := make(chan []byte), make(chan asksInput)
			settled, finished := make(chan struct{}), make(chan struct{})
			defer close(finished)
			var quitting, pasting atomic.Bool
			go convoInput(keys, events, func() { quitting.Store(true) }, &pasting, settled, finished)
			keys <- []byte(data)
			var tok = asksTokenizer{literal: true}
			want := len(tok.feed([]byte(data)))
			if strings.HasSuffix(data, "\x1b") {
				want++
			}
			c := screenOf(false, 80, 16, screenTurns(2, nil))
			for range want {
				select {
				case e := <-events:
					if c.input(e) {
						t.Fatal("screen quit for query input")
					}
				case <-time.After(time.Second):
					t.Fatal("query event missing")
				}
			}
			if quitting.Load() {
				t.Fatal("reader quit for query q/Escape/backspace/paste")
			}
			keys <- []byte{3}
			select {
			case <-events:
			case <-time.After(time.Second):
				t.Fatal("Ctrl-C event missing")
			}
			if !quitting.Load() {
				t.Fatal("Ctrl-C failed to bypass the screen")
			}
		})
	}
}

func FuzzConvoSearchInput(f *testing.F) {
	for _, seed := range []string{"/q j k g G\x7f\r", "/東京\x1bq", "/\x1b[200~q\r\x1b[201~", "/needle\x1b[B\r", "/👩‍💻\x08\x1b"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			return
		}
		c := newConvoScreen(false)
		c.cut(0, "needle 東京\n", []convoCut{{end: len("needle 東京\n"), kind: "line", text: "needle 東京"}}, 78)
		tok := asksTokenizer{literal: true}
		mode := false
		for len(data) > 0 {
			n := min(3, len(data)) // deliberately split UTF-8 and terminal sequences
			for _, e := range tok.feed(data[:n]) {
				// This is a read-only state-machine fixture; do not invoke clipboard helpers.
				if !mode && (e.key.r == 'y' || e.key.r == 'c') {
					continue
				}
				mode = convoSearchMode(mode, e)
				if c.input(e) {
					return
				}
				if mode != (c.search != nil) {
					t.Fatalf("reader/screen mode disagreement for %+v", e)
				}
				if c.search != nil && (!utf8.ValidString(c.search.query) || utf8.RuneCountInString(c.search.query) > convoSearchLimit) {
					t.Fatal("query is invalid or unbounded")
				}
			}
			data = data[n:]
		}
	})
}
