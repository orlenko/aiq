package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func TestParseResumeArgs(t *testing.T) {
	o, err := parseResumeArgs([]string{"--all", "--launcher=box", "abc", "--", "--model", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !o.all || o.launcher != "box" || o.id != "abc" || !reflect.DeepEqual(o.extra, []string{"--model", "x"}) {
		t.Fatalf("got %+v", o)
	}
	for _, bad := range [][]string{{"--bogus"}, {"a", "b"}, {"--launcher"}} {
		if _, err := parseResumeArgs(bad); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
}

func TestResumeArgs(t *testing.T) {
	cases := []struct {
		s     transcript.Session
		extra []string
		want  []string
	}{
		{transcript.Session{Provider: "claude", ID: "s1"}, nil, []string{"--resume", "s1"}},
		{transcript.Session{Provider: "claude", ID: "s1", Bypass: true}, []string{"--model", "opus"},
			[]string{"--dangerously-skip-permissions", "--model", "opus", "--resume", "s1"}},
		{transcript.Session{Provider: "codex", ID: "c1", Bypass: true}, nil,
			[]string{"--dangerously-bypass-approvals-and-sandbox", "resume", "c1"}},
		{transcript.Session{Provider: "codex", ID: "c1"}, []string{"-m", "gpt"}, []string{"-m", "gpt", "resume", "c1"}},
	}
	for _, c := range cases {
		if got := resumeArgs(c.s, c.extra); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: got %v, want %v", c.s.Provider, c.extra, got, c.want)
		}
	}
}

func TestFindSession(t *testing.T) {
	list := []transcript.Session{{ID: "abc123"}, {ID: "abd456"}, {ID: "ab"}}
	if s, err := findSession(list, "abc"); err != nil || s.ID != "abc123" {
		t.Fatal(s, err)
	}
	if s, err := findSession(list, "ab"); err != nil || s.ID != "ab" {
		t.Fatal("exact id must win over prefix matches", s, err)
	}
	if _, err := findSession(list, "a"); err == nil || !strings.Contains(err.Error(), "matches 3") {
		t.Fatal(err)
	}
	if _, err := findSession(list, "zz"); err == nil {
		t.Fatal("want not found")
	}
}

func TestParseKeys(t *testing.T) {
	got := parseKeys([]byte("\x1b[A\x1b[B\x1bOC\x1b[5~\x1b[6~\rjq\x1b\x03é"))
	want := []keypress{{code: keyUp}, {code: keyDown}, {code: keyRight}, {code: keyPgUp}, {code: keyPgDn},
		{code: keyEnter}, {code: keyDown}, {r: 'q'}, {code: keyBack}, {code: keyQuit}, {r: 'é'}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got := parseKeys([]byte{0x1b}); !reflect.DeepEqual(got, []keypress{{code: keyBack}}) {
		t.Fatalf("lone escape: %+v", got)
	}
}

func TestCellsAndTruncate(t *testing.T) {
	if n := cells("🗣 hi"); n != 5 {
		t.Errorf("cells(🗣 hi) = %d", n)
	}
	if n := cells("日本"); n != 4 {
		t.Errorf("cells(日本) = %d", n)
	}
	if n := cells("é"); n != 1 {
		t.Errorf("cells(e + combining acute) = %d", n)
	}
	if got := truncate("日本語テキスト", 7); got != "日本語…" || cells(got) != 7 {
		t.Errorf("truncate = %q (%d cells)", got, cells(got))
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate = %q", got)
	}
	if got := pad("ab", 4); got != "ab  " {
		t.Errorf("pad = %q", got)
	}
}

func TestWrap(t *testing.T) {
	got := wrap("one two three four five six\n\n  indented words that wrap around\nhttps://example.com/a/very/long/path/that/must/split", 20)
	for _, l := range got {
		if cells(l) > 20 {
			t.Errorf("row %q is %d cells", l, cells(l))
		}
	}
	joined := strings.Join(got, "|")
	for _, want := range []string{"one two three four|five six||", "  indented words", "https://example.com/"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrap output %q lacks %q", joined, want)
		}
	}
	if !strings.Contains(strings.ReplaceAll(joined, "|", ""), "https://example.com/a/very/long/path/that/must/split") {
		t.Errorf("hard-split lost text: %q", joined)
	}
}

func browserFixture() *browser {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.Local)
	turns := func(n int) []transcript.Turn {
		var out []transcript.Turn
		for i := 0; i < n; i++ {
			out = append(out, transcript.Turn{Prompt: "ask", Reply: "done", Started: now, Ended: now.Add(time.Minute), Tools: 2})
		}
		return out
	}
	all := []transcript.Session{
		{Provider: "claude", ID: "open1111", Updated: now, Turns: turns(3)},
		{Provider: "codex", ID: "work2222", Updated: now, Worker: true, Turns: turns(1)},
		{Provider: "codex", ID: "long3333", Updated: now, Turns: turns(40), Title: "named"},
	}
	live := map[string]liveNote{
		"open1111": {pid: 42, note: "open in pid 42"},
		"long3333": {tmux: "aiq-x", note: "running in aiq long"},
	}
	b := newBrowser("/w", all, live, false)
	b.now, b.w, b.h = now, 80, 20
	return b
}

func TestBrowserNavigation(t *testing.T) {
	b := browserFixture()
	if len(b.list) != 2 || b.hiddenWorkers() != 1 {
		t.Fatalf("workers should start hidden: %d shown", len(b.list))
	}
	b.handle(keypress{code: keyDown})
	if b.current().ID != "long3333" {
		t.Fatalf("selected %s", b.current().ID)
	}
	// Toggling workers keeps the selection on the same session.
	b.handle(keypress{r: 'a'})
	if len(b.list) != 3 || b.current().ID != "long3333" {
		t.Fatalf("after toggle: %d shown, selected %s", len(b.list), b.current().ID)
	}
	b.handle(keypress{code: keyDown})
	b.handle(keypress{code: keyDown})
	if b.current().ID != "long3333" {
		t.Fatal("selection must stop at the last session")
	}
	b.handle(keypress{code: keyEnter})
	if b.view != viewTurns || b.tsel != 39 {
		t.Fatalf("enter should open turns at the last one: view %d turn %d", b.view, b.tsel)
	}
	b.handle(keypress{code: keyHome})
	b.handle(keypress{code: keyEnter})
	b.handle(keypress{code: keyRight})
	if b.view != viewTurn || b.tsel != 1 {
		t.Fatalf("→ in a turn moves to the next: view %d turn %d", b.view, b.tsel)
	}
	b.handle(keypress{code: keyBack})
	b.handle(keypress{code: keyBack})
	if b.view != viewSessions {
		t.Fatalf("esc twice should return to sessions, view %d", b.view)
	}
	// A long session resumes (attaches) without a confirmation.
	if pick, done := b.handle(keypress{r: 'r'}); !done || pick.ID != "long3333" {
		t.Fatalf("r: %v %v", pick, done)
	}
	if _, done := b.handle(keypress{code: keyBack}); !done {
		t.Fatal("esc on the session list quits")
	}
}

func TestBrowserConfirmsOpenSession(t *testing.T) {
	b := browserFixture()
	if pick, done := b.handle(keypress{r: 'r'}); done || pick != nil || !strings.Contains(b.confirm, "pid 42") {
		t.Fatalf("first r should ask: %v %v %q", pick, done, b.confirm)
	}
	if pick, done := b.handle(keypress{r: 'r'}); !done || pick.ID != "open1111" {
		t.Fatalf("second r resumes: %v %v", pick, done)
	}
	b = browserFixture()
	b.handle(keypress{r: 'r'})
	b.handle(keypress{code: keyUp}) // any other key cancels
	if b.confirm != "" {
		t.Fatal("confirmation should clear")
	}
	if _, done := b.handle(keypress{r: 'r'}); done {
		t.Fatal("r after a cancelled confirmation asks again")
	}
}

func TestBrowserRenderFitsScreen(t *testing.T) {
	b := browserFixture()
	for _, size := range [][2]int{{80, 20}, {40, 8}, {120, 3}} {
		b.w, b.h = size[0], size[1]
		for _, view := range []int{viewSessions, viewTurns, viewTurn} {
			b.view, b.sel, b.tsel = view, 1, 20
			b.tsel = clamp(b.tsel, 0, len(b.current().Turns)-1)
			rows := b.render()
			if len(rows) != b.h {
				t.Errorf("%dx%d view %d: %d rows", b.w, b.h, view, len(rows))
			}
		}
	}
	b.w, b.h, b.view, b.sel = 80, 20, viewTurns, 1
	b.tsel = 20
	var text []string
	for _, l := range b.render() {
		text = append(text, l.text)
	}
	screen := strings.Join(text, "\n")
	if !strings.Contains(screen, "▸ #21") || !strings.Contains(screen, "running in aiq long") {
		t.Fatalf("turns screen:\n%s", screen)
	}
}
