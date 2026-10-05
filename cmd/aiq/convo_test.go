package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestParseConvoArgs(t *testing.T) {
	o, err := parseConvoArgs([]string{"--pane", "7", "--follow", "--last=3"})
	if err != nil || o.pane != "%7" || !o.follow || o.last != 3 || o.id != "" {
		t.Fatalf("got %+v, %v", o, err)
	}
	o, err = parseConvoArgs([]string{"--pane=%12", "-f"})
	if err != nil || o.pane != "%12" || !o.follow {
		t.Fatalf("got %+v, %v", o, err)
	}
	if o, err := parseConvoArgs([]string{"abc123"}); err != nil || o.id != "abc123" {
		t.Fatalf("got %+v, %v", o, err)
	}
	for _, bad := range [][]string{{"--last", "x"}, {"--last", "-1"}, {"--pane"}, {"--pane", "%1", "abc"}, {"a", "b"}, {"--nope"}} {
		if _, err := parseConvoArgs(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func convoFixture() *transcript.Session {
	at := func(m int) time.Time { return time.Date(2026, 9, 1, 14, m, 0, 0, time.Local) }
	return &transcript.Session{Provider: "claude", ID: "abcdef123456", Cwd: "/w", Model: "claude-opus-5", Turns: []transcript.Turn{
		{Prompt: "first question", Reply: "first answer", Started: at(0), Ended: at(3), Tools: 2},
		{Prompt: "aiq moved this session to claude/b", Source: transcript.Aiq, Reply: "Picking up where it left off.\nmore", Started: at(4), Ended: at(5)},
		{Prompt: "fix the build", Steers: []string{"also run the tests"}, Earlier: []string{"Build fixed; waiting on CI."},
			Reply: "CI is green.", Started: at(10), Ended: at(22), Tools: 47},
		{Prompt: "Another Claude session sent a message: hi", Source: transcript.Peer, Reply: "Replied to the peer.", Started: at(23), Ended: at(24)},
		{Prompt: "[agent-nudge] PR #4 has a review", Source: transcript.Notice, Started: at(25), Ended: at(25)},
		{Prompt: "silent one", Started: at(30), Ended: at(31)},
		{Prompt: "now ship it", Steers: []string{"wait for me"}, Earlier: []string{"Tagged."}, Open: true, Started: at(40), Tools: 5},
	}}
}

func TestRenderConvoPlain(t *testing.T) {
	s := convoFixture()
	now := s.Turns[6].Started.Add(12 * time.Minute)
	out := renderConvo(s, convoTarget{account: "claude/a", pane: "%3"}, 0, false, now)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("plain output has escape sequences:\n%s", out)
	}
	want := []string{
		"claude opus-5 · claude/a · /w · session abcdef12 · pane %3\n",
		"\n▌ you · " + whenLabel(s.Turns[0].Started, time.Now()) + "\nfirst question\n",
		"\n▌ claude · 3m · 2 tools\nfirst answer\n",
		"\n· aiq: aiq moved this session to claude/b → Picking up where it left off. more\n",
		"\n▌ you, while it worked\nalso run the tests\n",
		"\n▌ claude · earlier\nBuild fixed; waiting on CI.\n",
		"\n▌ claude · 12m · 47 tools\nCI is green.\n",
		"\n· peer agent: Another Claude session sent a message: hi → Replied to the peer.\n",
		"\n· notice: [agent-nudge] PR #4 has a review\n",
		"\n▌ claude · 1m\n(no reply)\n",
		"\n▌ you, while it worked\nwait for me\n\n▌ claude · earlier\nTagged.\n",
		"\nworking · 12m · 5 tools\n",
	}
	rest := out
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("output lacks (in order) %q:\n%s", w, out)
		}
		rest = rest[i+len(w):]
	}
	if rest != "" {
		t.Errorf("unexpected tail %q", rest)
	}
	// The running turn's reply is not shown before it ends.
	if strings.Count(out, "▌ claude ·") != 5 {
		t.Errorf("want 5 reply headers (3 replies, 2 earlier):\n%s", out)
	}
}

func TestRenderConvoColorAndLast(t *testing.T) {
	s := convoFixture()
	out := renderConvo(s, convoTarget{}, 1, true, time.Now())
	if !strings.Contains(out, "(6 earlier turns not shown)") || strings.Contains(out, "first question") {
		t.Fatalf("--last 1 shows:\n%s", out)
	}
	if !strings.Contains(out, sgrBoldCyan+"▌ you, while it worked"+sgrReset) {
		t.Errorf("no coloured header:\n%q", out)
	}
	if strings.Contains(out, osc133Prompt) {
		t.Errorf("a snapshot carries prompt marks")
	}
}

func TestConvoStart(t *testing.T) {
	turns := convoFixture().Turns
	cases := map[int]int{0: 0, 1: 6, 2: 5, 3: 2, 4: 0, 99: 0}
	for last, want := range cases {
		if got := convoStart(turns, last); got != want {
			t.Errorf("convoStart(last=%d) = %d, want %d", last, got, want)
		}
	}
}

// --follow prints each piece once as it turns up; at the end its output is
// the snapshot of the final transcript.
func TestConvoPrinterIsAppendOnly(t *testing.T) {
	final := convoFixture()
	final.Turns[6].Open = false
	final.Turns[6].Reply = "Shipped."
	final.Turns[6].Ended = final.Turns[6].Started.Add(time.Hour)

	grow := func(n int, edit func(last *transcript.Turn)) *transcript.Session {
		s := *final
		s.Turns = append([]transcript.Turn(nil), final.Turns[:n]...)
		if edit != nil {
			edit(&s.Turns[n-1])
		}
		return &s
	}
	steps := []*transcript.Session{
		grow(1, func(t *transcript.Turn) { t.Open, t.Reply = true, "" }),
		grow(1, nil),
		grow(2, func(t *transcript.Turn) { t.Open, t.Reply = true, "" }), // a notice still running prints nothing
		grow(3, func(t *transcript.Turn) { t.Open, t.Reply, t.Steers, t.Earlier = true, "", nil, nil }),
		grow(3, func(t *transcript.Turn) { t.Open, t.Reply, t.Earlier = true, "", nil }),
		grow(3, func(t *transcript.Turn) { t.Open, t.Reply = true, "" }),
		grow(5, nil),
		grow(7, func(t *transcript.Turn) { t.Open, t.Reply, t.Earlier = true, "", nil }),
		final,
	}
	p := newConvoPrinter("claude", true, true)
	var got strings.Builder
	for i, s := range steps {
		got.WriteString(p.emit(s))
		if again := p.emit(s); again != "" {
			t.Fatalf("step %d printed twice: %q", i, again)
		}
	}
	want := newConvoPrinter("claude", true, true).emit(final)
	if got.String() != want {
		t.Fatalf("follow output differs from the snapshot\nfollow:\n%s\nsnapshot:\n%s", got.String(), want)
	}
	if n := strings.Count(want, osc133Prompt); n != 6 {
		t.Errorf("want a prompt mark on each of 4 prompts and 2 steers, got %d", n)
	}
}

// A turn can end, go on when a background task or a peer message wakes the
// agent, and end again. The first answer stays where it was printed and
// the new reply follows; nothing is lost or printed twice.
func TestConvoPrinterFollowsATurnThatGoesOn(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	session := func(turns ...transcript.Turn) *transcript.Session {
		return &transcript.Session{Provider: "codex", Turns: turns}
	}
	human := transcript.Turn{Prompt: "deploy", Started: at}
	notice := transcript.Turn{Prompt: "[agent-nudge] CI failed", Source: transcript.Notice, Started: at.Add(time.Hour)}
	with := func(t transcript.Turn, open bool, reply string, earlier ...string) transcript.Turn {
		t.Open, t.Reply, t.Earlier = open, reply, earlier
		return t
	}
	steps := []*transcript.Session{
		session(with(human, false, "Deploy started.")),
		session(with(human, true, "", "Deploy started.")),
		session(with(human, false, "Deploy finished.", "Deploy started.")),
		session(with(human, false, "Deploy finished.", "Deploy started."), with(notice, false, "Looking at CI.")),
		session(with(human, false, "Deploy finished.", "Deploy started."), with(notice, true, "", "Looking at CI.")),
		session(with(human, false, "Deploy finished.", "Deploy started."), with(notice, false, "CI fixed.", "Looking at CI.")),
	}
	p := newConvoPrinter("codex", false, false)
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(p.emit(s))
	}
	out := b.String()
	for _, w := range []string{"\nDeploy started.\n", "\nDeploy finished.\n", "→ Looking at CI.\n", "  → CI fixed.\n"} {
		if strings.Count(out, w) != 1 {
			t.Errorf("%q printed %d times:\n%s", w, strings.Count(out, w), out)
		}
	}
	if strings.Contains(out, "(no reply)") || strings.Index(out, "Deploy started.") > strings.Index(out, "Deploy finished.") {
		t.Errorf("got:\n%s", out)
	}
}

func TestClaudeLiveInPane(t *testing.T) {
	parents := map[int]int{300: 200, 200: 100, 100: 1, 400: 1}
	pane := tmux.Pane{ID: "%5", Session: "aiq-ops-1", PID: 100}
	cases := []struct {
		c    claudeLive
		want bool
	}{
		{claudeLive{PID: 100, Tmux: "aiq-ops-1:@4.%5"}, true},
		{claudeLive{PID: 300, Tmux: "aiq-ops-1:@4.%5"}, true},  // under a launcher
		{claudeLive{PID: 400, Tmux: "aiq-ops-1:@4.%5"}, false}, // names the pane but runs elsewhere: a reused id
		{claudeLive{PID: 100, Tmux: "other:@4.%5"}, false},
		{claudeLive{PID: 100, Tmux: "aiq-ops-1:@4.%15"}, false},
		{claudeLive{PID: 100}, false},
	}
	for _, c := range cases {
		if got := c.c.inPane(pane, parents); got != c.want {
			t.Errorf("%+v in pane: %v, want %v", c.c, got, c.want)
		}
	}
	if descends(parents, 400, 100) || !descends(parents, 100, 100) || descends(nil, 5, 1) {
		t.Error("descends")
	}
}

func TestClaudeTranscript(t *testing.T) {
	real, overlay := t.TempDir(), t.TempDir()
	write := func(p string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("{}\n"), 0o644)
	}
	write(filepath.Join(overlay, "-w-my-app", "s1.jsonl"))
	write(filepath.Join(real, "-private-tmp-x", "s2.jsonl"))
	long := "/" + strings.Repeat("d", 230)
	write(filepath.Join(real, "-"+strings.Repeat("d", 199)+"-1a2b3c", "s3.jsonl"))
	roots := []string{real, overlay}
	if got := claudeTranscript(roots, "/w/my.app", "s1"); got != filepath.Join(overlay, "-w-my-app", "s1.jsonl") {
		t.Errorf("s1: %q", got)
	}
	// Recorded under /tmp, run under /private/tmp: found by id.
	if got := claudeTranscript(roots, "/tmp/x", "s2"); got != filepath.Join(real, "-private-tmp-x", "s2.jsonl") {
		t.Errorf("s2: %q", got)
	}
	if got := claudeTranscript(roots, long, "s3"); !strings.HasSuffix(got, "s3.jsonl") {
		t.Errorf("s3: %q", got)
	}
	if got := claudeTranscript(roots, "/w/my.app", "nope"); got != "" {
		t.Errorf("missing id: %q", got)
	}
}

func TestSessionIDOf(t *testing.T) {
	cases := map[string]string{
		"/p/-w/817eb00c-002c-41d7-87f7-3f6fdc54f90a.jsonl":                                     "817eb00c-002c-41d7-87f7-3f6fdc54f90a",
		"/s/2026/10/05/rollout-2026-10-05T10-51-31-01a10c8c-6b27-7e23-bcb3-2d5d06ccba17.jsonl": "01a10c8c-6b27-7e23-bcb3-2d5d06ccba17",
	}
	for path, want := range cases {
		if got := sessionIDOf(path); got != want {
			t.Errorf("sessionIDOf(%s) = %s, want %s", path, got, want)
		}
	}
}

// The status line is rewritten in place and erased before new text.
func TestFollowStatusLine(t *testing.T) {
	var b bytes.Buffer
	f := &convoFollow{w: &b, tty: true}
	f.setStatus("working · 1m · 2 tools")
	f.setStatus("working · 1m · 2 tools")
	f.print("\n▌ you\nhi\n")
	f.setStatus("")
	want := "\r\x1b[K" + sgrDim + "working · 1m · 2 tools" + sgrReset + "\r\x1b[K\n▌ you\nhi\n"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
	b.Reset()
	f = &convoFollow{w: &b}
	f.setStatus("working")
	f.print("x\n")
	if b.String() != "x\n" {
		t.Fatalf("not a terminal: got %q", b.String())
	}
}

// A real transcript that grows while --follow watches it.
func TestFollowPrintsWhatTheFileGains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	c := `"cwd":"/w","sessionId":"s1"`
	user := func(ts, text string) string {
		return `{"type":"user",` + c + `,"timestamp":"` + ts + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	reply := func(ts, id, text string) string {
		return `{"type":"assistant",` + c + `,"timestamp":"` + ts + `","message":{"id":"` + id + `","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(path, []byte(user("2026-09-15T10:00:00Z", "first")+reply("2026-09-15T10:01:00Z", "m1", "one")), 0o644)
	var b bytes.Buffer
	f := &convoFollow{tgt: convoTarget{provider: "claude", path: path}, w: &b,
		statEvery: 10 * time.Millisecond, parseGap: 30 * time.Millisecond, resolveEvery: time.Hour}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.run(stop); close(done) }()

	time.Sleep(50 * time.Millisecond)
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(user("2026-09-15T10:02:00Z", "second") + reply("2026-09-15T10:03:00Z", "m2", "two"))
	fh.Close()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-done

	out := b.String()
	for _, w := range []string{"first", "one", "second", "two"} {
		if strings.Count(out, "\n"+w+"\n") != 1 {
			t.Errorf("%q not printed exactly once:\n%s", w, out)
		}
	}
	if strings.Index(out, "\none\n") > strings.Index(out, "\nsecond\n") {
		t.Errorf("out of order:\n%s", out)
	}
}

func TestRenderTurnsPlainMarksNonHumanTurns(t *testing.T) {
	s := *convoFixture()
	out := renderTurnsPlain(s, nil)
	if !strings.Contains(out, "## Turn 2 (aiq) ·") || !strings.Contains(out, "## Turn 4 (peer agent) ·") ||
		!strings.Contains(out, "## Turn 5 (notice) ·") || !strings.Contains(out, "## Turn 3 ·") {
		t.Fatalf("got:\n%s", out)
	}
}
