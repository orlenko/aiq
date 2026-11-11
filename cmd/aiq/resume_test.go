package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
		{transcript.Session{Provider: "agy", ID: "g1", Bypass: true}, []string{"--model", "gemini-3.1-pro-high"},
			[]string{"--dangerously-skip-permissions", "--model", "gemini-3.1-pro-high", "--conversation", "g1"}},
		{transcript.Session{Provider: "agy", ID: "g1"}, nil, []string{"--conversation", "g1"}},
	}
	for _, c := range cases {
		if got, err := resumeArgs(c.s, c.s.Bypass, c.extra); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %v: got %v, want %v", c.s.Provider, c.extra, got, c.want)
		}
	}
}

func TestFindSession(t *testing.T) {
	list := []transcript.Session{{ID: "abc123"}, {ID: "abd456"}, {ID: "ab"}}
	if s, err := findSession(list, nil, "abc"); err != nil || s.ID != "abc123" {
		t.Fatal(s, err)
	}
	if s, err := findSession(list, nil, "ab"); err != nil || s.ID != "ab" {
		t.Fatal("exact id must win over prefix matches", s, err)
	}
	if _, err := findSession(list, nil, "a"); err == nil || !strings.Contains(err.Error(), "matches 3") {
		t.Fatal(err)
	}
	if _, err := findSession(list, nil, "zz"); err == nil {
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
	if pick, done := b.handle(keypress{r: 'r'}); done || pick != nil || !strings.Contains(b.confirmText, "pid 42") {
		t.Fatalf("first r should ask: %v %v %q", pick, done, b.confirmText)
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

// Once stopped, readKeys must leave the terminal alone: the tmux client
// started next has to get every byte, the terminal's query answers included.
func TestReadKeysStopsReading(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	keys, stop := readKeys(int(r.Fd()))
	w.Write([]byte("j"))
	select {
	case got := <-keys:
		if string(got) != "j" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no key delivered")
	}
	stop()
	stop() // idempotent
	if _, ok := <-keys; ok {
		t.Fatal("keys still open after stop")
	}
	const reply = "\x1b[?64;1;2c"
	w.Write([]byte(reply))
	time.Sleep(150 * time.Millisecond) // past the reader's select timeout
	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != reply {
		t.Fatalf("after stop read %q, %v; want %q left for the next reader", buf[:n], err, reply)
	}
}

// resume --print and the plain session list keep a transcript's control characters off the
// terminal, as convo does: none reaches the output from the title, model, id, prompt or
// reply, and a line separator in a prompt stays inside the quote.
func TestResumePlainDropsControlCharacters(t *testing.T) {
	esc := "\x1b]52;c;aGk=\x07\x1b[2J\u009b31m\u202e\x7f"
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)
	s := transcript.Session{Provider: "claude", ID: "abc" + esc + "def", Model: "claude-x" + esc, Title: "title" + esc, Updated: at,
		Turns: []transcript.Turn{{Prompt: "ask" + esc + "\u2028more", Reply: "reply" + esc, Source: transcript.Human, Started: at, Ended: at}}}
	untitled := s
	untitled.Title = ""
	for name, out := range map[string]string{
		"turns":            renderTurnsPlain(s, nil),
		"sessions":         renderSessionsPlain("dir"+esc, []transcript.Session{s, untitled}, map[string]liveNote{s.ID: {note: "live" + esc}}, nil),
		"no sessions":      renderSessionsPlain("dir"+esc, nil, nil, nil),
		"untitled session": renderTurnsPlain(untitled, nil),
	} {
		for _, r := range out {
			if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f || r == 0x202e) {
				t.Errorf("%s: control character %U in output:\n%q", name, r, out)
				break
			}
		}
	}
	if out := renderTurnsPlain(s, nil); !strings.Contains(out, "> ask]52;c;aGk=[2J31m\n> more") {
		t.Errorf("prompt not quoted line by line:\n%q", out)
	}
}

// The resume browser's three screens and the start menu keep a transcript's control
// characters off the terminal too; only the screens' own escapes reach it.
func TestScreensDropControlCharacters(t *testing.T) {
	esc := "\x1b]52;c;aGk=\x07\x1b[2J\u009b31m\u202e\x7f"
	at := time.Now().Add(-time.Hour)
	s := transcript.Session{Provider: "claude", ID: "abc" + esc, Model: "claude-x" + esc + "\u2028y", Title: "title" + esc + "\u2028next", Updated: at,
		Turns: []transcript.Turn{{Prompt: "ask" + esc, Reply: "reply" + esc, Source: transcript.Human, Started: at, Ended: at}}}
	check := func(name string, draw func(*os.File)) {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "screen")
		if err != nil {
			t.Fatal(err)
		}
		draw(f)
		f.Close()
		raw, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		out := string(raw)
		if !strings.Contains(out, "title") {
			t.Fatalf("%s: drew no title:\n%q", name, out)
		}
		for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\u009b", "\u202e", "\x7f", "\u2028", "\t"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: %q reached the terminal:\n%q", name, bad, out)
			}
		}
		// The screens end each row with their own \r\n; a line break from the text would
		// split a row.
		if strings.Count(out, "\n") != strings.Count(out, "\r\n") {
			t.Errorf("%s: a line break from the text split a row:\n%q", name, out)
		}
	}
	b := newBrowser("/w", []transcript.Session{s}, map[string]liveNote{s.ID: {note: "live\tnote" + esc}}, false)
	b.w, b.h = 120, 30
	for _, v := range []int{viewSessions, viewTurns, viewTurn} {
		b.view = v
		check([]string{"sessions", "turns", "turn"}[v], b.draw)
	}
	c := defaultChoice()
	c.Session = "resume"
	m := &menu{env: testMenuEnv(), c: c, focus: "session", w: 200, h: 30}
	m.env.sessions = &menuSessions{count: 1, latest: s}
	m.env.dir = "/w\tdir"
	check("menu", m.draw)
}

// What resume says before it hands over the terminal drops control characters too.
func TestResumeNoteDropsControlCharacters(t *testing.T) {
	var b strings.Builder
	resumeNote(&b, "aiq: session %s is %s\n", "abc\x1b]52;c;aGk=\x07", "live\x1b[2J")
	if got := b.String(); got != "aiq: session abc]52;c;aGk= is live[2J\n" {
		t.Fatalf("got %q", got)
	}
}

// aiq's failures drop control characters too: an error can quote a session id.
func TestErrorTextDropsControlCharacters(t *testing.T) {
	err := configErr("no-launcher", "session %s ran through launcher x", "abc\x1b]52;c;aGk=\x07")
	if got := errorText(err); got != "session abc]52;c;aGk= ran through launcher x" {
		t.Fatalf("got %q", got)
	}
}

// resumeSession says what it does through resumeNote only; it cannot be run here, since it
// hands the terminal to a provider, so this reads its source.
func TestResumeSessionNotesGoThroughResumeNote(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "resume.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	notes := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "resumeSession" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch c := call.Fun.(type) {
			case *ast.Ident:
				if c.Name == "resumeNote" {
					notes++
				}
			case *ast.SelectorExpr:
				if x, ok := c.X.(*ast.Ident); ok && x.Name == "fmt" && strings.Contains(c.Sel.Name, "rint") {
					t.Errorf("resumeSession prints with fmt.%s; use resumeNote", c.Sel.Name)
				}
			}
			return true
		})
	}
	if notes != 4 {
		t.Errorf("resumeSession calls resumeNote %d times, want 4", notes)
	}
}

// main prints a failure through errorText, never the error itself.
func TestMainPrintsErrorsThroughErrorText(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	uses := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "errorText" {
				uses++
				return false
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "fmt" {
					for _, a := range call.Args {
						if id, ok := a.(*ast.Ident); ok && id.Name == "err" {
							t.Errorf("main prints err with fmt.%s; use errorText(err)", sel.Sel.Name)
						}
					}
				}
			}
			return true
		})
	}
	if uses == 0 {
		t.Error("main never calls errorText")
	}
}
