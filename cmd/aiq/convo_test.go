package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
		"\n· aiq: aiq moved this session to claude/b\n  → Picking up where it left off.\n",
		"\n▌ you, while it worked\nalso run the tests\n",
		"\n▌ claude · earlier\nBuild fixed; waiting on CI.\n",
		"\n▌ claude · 12m · 47 tools\nCI is green.\n",
		"\n· peer agent: Another Claude session sent a message: hi\n  → Replied to the peer.\n",
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
	if !strings.Contains(out, sgrBand+sgrBoldCyan+"▌ you, while it worked ") {
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
		{claudeLive{PID: 100, Tmux: "renamed:@4.%5"}, true},    // the tmux session was renamed
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
		statEvery: 10 * time.Millisecond, parseGap: 30 * time.Millisecond, resolveEvery: time.Hour, quiet: 100 * time.Millisecond}
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

func TestConvoHidesIdleNotifications(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{
		{Prompt: `{"type":"idle_notification","from":"worker"}`, Source: transcript.Peer, Reply: "noted", Started: at, Ended: at},
		{Prompt: "worker finished the migration", Source: transcript.Peer, Reply: "merged it", Started: at.Add(time.Minute), Ended: at.Add(time.Minute)},
	}}
	out := renderConvo(s, convoTarget{}, 0, false, at)
	if strings.Contains(out, "idle_notification") || strings.Contains(out, "noted") {
		t.Errorf("idle notification shown:\n%s", out)
	}
	if !strings.Contains(out, "worker finished the migration") {
		t.Errorf("real peer message hidden:\n%s", out)
	}
}

func TestLoadConvoClosesTheOpenTurnOfAStaleSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"user","timestamp":"2026-09-01T14:00:00Z","sessionId":"s","cwd":"/w","message":{"role":"user","content":"do it"}}`,
		`{"type":"assistant","timestamp":"2026-09-01T14:00:05Z","message":{"id":"m1","role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := loadConvo(convoTarget{provider: "claude", path: path})
	if err != nil || len(live.Turns) != 1 || !live.Turns[0].Open {
		t.Fatalf("live: want one open turn, got %+v, %v", live, err)
	}
	stale, err := loadConvo(convoTarget{provider: "claude", path: path, stale: true})
	if err != nil || len(stale.Turns) != 1 || stale.Turns[0].Open {
		t.Fatalf("stale: want the turn closed, got %+v, %v", stale, err)
	}
}

func TestTurnReplyFallsBackToTheLastEarlierAnswer(t *testing.T) {
	if got := turnReply(transcript.Turn{Reply: "final", Earlier: []string{"a"}}); got != "final" {
		t.Errorf("got %q", got)
	}
	if got := turnReply(transcript.Turn{Open: true, Earlier: []string{"a", "b"}}); got != "b" {
		t.Errorf("got %q", got)
	}
	if got := turnReply(transcript.Turn{Open: true}); got != "" {
		t.Errorf("got %q", got)
	}
}

// fixtureLines builds Claude Code transcript records, one timestamp each.
type fixtureLines struct {
	lines []string
	sec   int
}

func (c *fixtureLines) ts() string {
	c.sec++
	return fmt.Sprintf("2026-09-15T10:%02d:%02dZ", c.sec/60, c.sec%60)
}

func (c *fixtureLines) user(extra, content string) {
	c.lines = append(c.lines, `{"type":"user","isSidechain":false,`+extra+`"cwd":"/w","sessionId":"s","timestamp":"`+c.ts()+`","message":{"role":"user","content":`+content+`}}`)
}

func (c *fixtureLines) typed(text string) {
	c.user(`"origin":{"kind":"human"},"promptSource":"typed",`, strconv.Quote(text))
}

func (c *fixtureLines) notification() {
	c.user(`"origin":{"kind":"task-notification"},"promptSource":"system",`, strconv.Quote("<task-notification>done</task-notification>"))
}

func (c *fixtureLines) assistant(id, stop, part string) {
	c.lines = append(c.lines, `{"type":"assistant","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":"`+c.ts()+
		`","message":{"id":"`+id+`","model":"claude-opus-5","stop_reason":"`+stop+`","role":"assistant","content":[`+part+`]}}`)
}

// answer is a final message as Claude Code writes it: a thinking record and
// a text record, both carrying the stop reason.
func (c *fixtureLines) answer(id, text string) {
	c.assistant(id, "end_turn", `{"type":"thinking","thinking":"hm"}`)
	c.assistant(id, "end_turn", `{"type":"text","text":`+strconv.Quote(text)+`}`)
}

func (c *fixtureLines) tool(id string) {
	c.assistant(id, "tool_use", `{"type":"text","text":"Checking."}`)
	c.assistant(id, "tool_use", `{"type":"tool_use","id":"t`+id+`","name":"Bash","input":{}}`)
	c.user(`"promptSource":"system",`, `[{"type":"tool_result","tool_use_id":"t`+id+`","content":"ok"}]`)
}

func (c *fixtureLines) steer(text string) {
	c.lines = append(c.lines, `{"type":"attachment","isSidechain":false,"attachment":{"type":"queued_command","prompt":`+strconv.Quote(text)+
		`,"commandMode":"prompt","origin":{"kind":"human"}},"timestamp":"`+c.ts()+`","sessionId":"s"}`)
}

func claudeConvoFixtures() map[string][]string {
	out := map[string][]string{}
	var c fixtureLines
	// The answer, then a background task wakes the agent, which ends with
	// nothing to say: the parser moves the answer back to Reply.
	c.typed("check the deploy")
	c.answer("m1", "The deploy is fine.")
	c.notification()
	c.tool("m2")
	c.assistant("m3", "end_turn", `{"type":"thinking","thinking":"nothing to add"}`)
	out["answer moves back"] = c.lines

	c = fixtureLines{}
	c.typed("run the suite")
	c.tool("m1")
	c.answer("m2", "The suite runs in the background.")
	c.notification()
	c.tool("m3")
	c.answer("m4", "The suite passed.")
	out["wakeup with a new answer"] = c.lines

	c = fixtureLines{}
	c.typed("refactor the parser")
	c.tool("m1")
	c.steer("also rename the package")
	c.tool("m2")
	c.answer("m3", "Refactored and renamed.")
	out["steer"] = c.lines

	c = fixtureLines{}
	c.typed("[agent-nudge] 1 message waiting")
	c.tool("m1")
	c.steer("here is the context Jerry pasted")
	c.tool("m2")
	c.answer("m3", "Replied to Jerry with the context.")
	out["steer in a notice"] = c.lines

	c = fixtureLines{}
	c.user("", strconv.Quote("<command-name>/model</command-name>\n<command-args>opus</command-args>"))
	c.user("", strconv.Quote("<local-command-stdout>Set model to opus</local-command-stdout>"))
	c.typed("hello")
	c.answer("m1", "Hi.")
	out["local command"] = c.lines

	var all []string
	for _, name := range []string{"answer moves back", "wakeup with a new answer", "steer", "steer in a notice", "local command"} {
		all = append(all, out[name]...)
	}
	out["all of them"] = all
	return out
}

func codexConvoFixture() []string {
	sec := 0
	var lines []string
	add := func(typ, payload string) {
		sec++
		lines = append(lines, fmt.Sprintf(`{"timestamp":"2026-09-15T21:%02d:%02dZ","type":"%s","payload":%s}`, sec/60, sec%60, typ, payload))
	}
	user := func(turn, text string) {
		add("response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":`+strconv.Quote(text)+
			`}],"internal_chat_message_metadata_passthrough":{"turn_id":"`+turn+`","content_item_kinds":["user.text"]}}`)
	}
	say := func(phase, text string) {
		add("response_item", `{"type":"message","role":"assistant","phase":"`+phase+`","content":[{"type":"output_text","text":`+strconv.Quote(text)+`}]}`)
	}
	add("session_meta", `{"id":"c","cwd":"/w","source":"cli"}`)
	add("event_msg", `{"type":"task_started","turn_id":"a"}`)
	user("a", "deploy the site")
	say("commentary", "Building.")
	user("a", "use the staging bucket")
	add("response_item", `{"type":"custom_tool_call","name":"exec"}`)
	say("final_answer", "Deployed to staging.")
	add("event_msg", `{"type":"task_complete","turn_id":"a","last_agent_message":"Deployed to staging."}`)
	add("event_msg", `{"type":"task_started","turn_id":"b"}`)
	user("b", "Agent Orchestra local inbox notice. 1 message")
	say("final_answer", "Nothing for you.")
	add("event_msg", `{"type":"task_complete","turn_id":"b","last_agent_message":"Nothing for you."}`)
	add("event_msg", `{"type":"task_started","turn_id":"w"}`)
	add("response_item", `{"type":"function_call","name":"wait"}`)
	say("final_answer", "The build is green.")
	add("event_msg", `{"type":"task_complete","turn_id":"w","last_agent_message":"The build is green."}`)
	add("event_msg", `{"type":"task_started","turn_id":"c"}`)
	user("c", "thanks")
	say("final_answer", "Welcome.")
	add("event_msg", `{"type":"task_complete","turn_id":"c","last_agent_message":"Welcome."}`)
	return lines
}

// pieces collects what a printer prints, per turn: "kind\x00text".
func pieces(p *convoPrinter) map[string][]string {
	got := map[string][]string{}
	p.piece = func(key, kind, text string) { got[key] = append(got[key], kind+"\x00"+text) }
	return got
}

// checkFollowMatchesSnapshot parses every line-prefix of a growing
// transcript into one --follow printer, then holds what it printed against
// a snapshot of the whole file: per turn the same prompts, steers, said
// lines and answers, in the same order, and no "(no reply)" for a turn
// that has an answer. One difference is allowed: a line --follow printed
// as said on the way, which a later answer then repeats in full (the
// snapshot shows only the answer). And in the rendered text of both,
// every answer shows whole, never only as its one-line summary.
func checkFollowMatchesSnapshot(t *testing.T, name string, lines []string, parse func(string) (*transcript.Session, error)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	follow := newConvoPrinter("x", false, false)
	follow.live = true
	followed := pieces(follow)
	var s *transcript.Session
	var followText strings.Builder
	load := func() {
		var err error
		if s, err = parse(path); err != nil {
			t.Fatal(err)
		}
	}
	for n := 1; n <= len(lines); n++ {
		if err := os.WriteFile(path, []byte(strings.Join(lines[:n], "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		load()
		// As convoFollow.emit does: each line just written, the file is
		// not quiet yet.
		followText.WriteString(followEmit(follow, s, s.Unfinished))
	}
	load()
	if follow.pending {
		followText.WriteString(followEmit(follow, s, false)) // the file went quiet
		load()
	}
	snap := newConvoPrinter("x", false, false)
	snapped := pieces(snap)
	snapText := snap.emit(s)

	// dropRepeated leaves out a said line that the answer printed next
	// (steers aside) repeats.
	dropRepeated := func(list []string) []string {
		var out []string
		for i, p := range list {
			if text, ok := strings.CutPrefix(p, "said\x00"); ok {
				j := i + 1
				for j < len(list) && strings.HasPrefix(list[j], "steer\x00") {
					j++
				}
				if j < len(list) && list[j] == "answer\x00"+text {
					continue
				}
			}
			out = append(out, p)
		}
		return out
	}
	// lineOnly says which turns show answers as "→ first line": a
	// notifier's turn, before the person's first steer.
	lineOnly := map[string]map[string]bool{}
	for i, tr := range s.Turns {
		if tr.Source == transcript.Human {
			continue
		}
		before := map[string]bool{}
		for _, pc := range turnPieces(tr) {
			if pc.kind == transcript.ItemSteer {
				break
			}
			before[pc.text] = true
		}
		lineOnly[turnKey(i, tr)] = before
	}
	without := func(list []string, kind string, keepOrder ...string) []string {
		var out []string
		for _, p := range list {
			if !strings.HasPrefix(p, kind+"\x00") {
				out = append(out, p)
			}
		}
		if len(keepOrder) == 0 {
			sort.Strings(out)
		}
		return out
	}
	keys := map[string]bool{}
	for k := range followed {
		keys[k] = true
	}
	for k := range snapped {
		keys[k] = true
	}
	for k := range keys {
		fl := dropRepeated(followed[k])
		f, sn := without(fl, "none"), without(snapped[k], "none")
		if !reflect.DeepEqual(f, sn) {
			t.Errorf("%s, turn %s:\nfollow   %q\nsnapshot %q", name, k, f, sn)
		}
		// The same order, too: --follow prints each piece where it
		// belongs, never one it skipped later on.
		if fo, so := without(fl, "none", "keep order"), without(snapped[k], "none", "keep order"); !reflect.DeepEqual(fo, so) {
			t.Errorf("%s, turn %s: order differs:\nfollow   %q\nsnapshot %q", name, k, fo, so)
		}
		answered := slices.ContainsFunc(snapped[k], func(p string) bool { return strings.HasPrefix(p, "answer\x00") })
		if answered && (slices.ContainsFunc(followed[k], func(p string) bool { return strings.HasPrefix(p, "none\x00") }) ||
			slices.ContainsFunc(snapped[k], func(p string) bool { return strings.HasPrefix(p, "none\x00") })) {
			t.Errorf("%s, turn %s: (no reply) for a turn that has an answer", name, k)
		}
		// Every answer shows whole in both renderings.
		for _, p := range snapped[k] {
			if text, ok := strings.CutPrefix(p, "answer\x00"); ok {
				for out, which := range map[string]string{snapText: "snapshot", followText.String(): "follow"} {
					if !strings.Contains(out, "\n"+text+"\n") && !(lineOnly[k][text] && strings.Contains(out, "  → "+firstLine(text))) {
						t.Errorf("%s, turn %s: the %s shows %q only in part:\n%s", name, k, which, text, out)
					}
				}
			}
		}
	}
}

// The real --follow path on a Claude turn whose answer comes as a thinking
// record and then a text record: the narration before it is never shown as
// an answer, and the answer shows once, as the final, with its heading.
func TestFollowHoldsTheAnswerUntilItSettles(t *testing.T) {
	var c fixtureLines
	c.typed("check the pool")
	c.tool("m1")
	c.answer("m2", "Pool is fine.")
	path := filepath.Join(t.TempDir(), "s.jsonl")
	p := newConvoPrinter("claude", false, false)
	p.live = true
	var out strings.Builder
	var s *transcript.Session
	for n := 1; n <= len(c.lines); n++ {
		os.WriteFile(path, []byte(strings.Join(c.lines[:n], "\n")+"\n"), 0o600)
		s, _ = transcript.ParseClaude(path)
		out.WriteString(followEmit(p, s, s.Unfinished))
	}
	if p.pending {
		s, _ = transcript.ParseClaude(path)
		out.WriteString(followEmit(p, s, false))
	}
	got := out.String()
	if !regexp.MustCompile(`\n▌ claude · \d+s · 1 tool\nPool is fine\.\n`).MatchString(got) || strings.Count(got, "Pool is fine.") != 1 {
		t.Errorf("the final lacks its heading:\n%s", got)
	}
	if strings.Contains(got, "earlier") || strings.Count(got, "Checking.") != 1 {
		t.Errorf("the narration shows as an answer:\n%s", got)
	}
}

func TestFollowMatchesSnapshotAtEveryLine(t *testing.T) {
	for name, lines := range claudeConvoFixtures() {
		checkFollowMatchesSnapshot(t, name, lines, transcript.ParseClaude)
	}
	checkFollowMatchesSnapshot(t, "codex", codexConvoFixture(), transcript.ParseCodex)
}

// The fixtures say what they mean to: the steer shows, the moved answer
// is the reply, the local command leaves no turn.
func TestConvoFixturesRender(t *testing.T) {
	render := func(lines []string, parse func(string) (*transcript.Session, error)) string {
		path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
		os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		s, err := parse(path)
		if err != nil {
			t.Fatal(err)
		}
		return renderConvo(s, convoTarget{}, 0, false, time.Now())
	}
	f := claudeConvoFixtures()
	if out := render(f["answer moves back"], transcript.ParseClaude); strings.Contains(out, "(no reply)") || strings.Count(out, "The deploy is fine.") != 1 {
		t.Errorf("answer moves back:\n%s", out)
	}
	out := render(f["steer in a notice"], transcript.ParseClaude)
	for _, w := range []string{"· notice: [agent-nudge] 1 message waiting\n", "\n▌ you, while it worked\nhere is the context Jerry pasted\n", "\nReplied to Jerry with the context.\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("steer in a notice lacks %q:\n%s", w, out)
		}
	}
	if out := render(f["local command"], transcript.ParseClaude); strings.Contains(out, "/model") || !strings.Contains(out, "\nHi.\n") {
		t.Errorf("local command:\n%s", out)
	}
	out = render(codexConvoFixture(), transcript.ParseCodex)
	for _, w := range []string{"\nuse the staging bucket\n", "· notice: Agent Orchestra local inbox notice. 1 message\n  → Nothing for you.\n  → The build is green.\n", "\nWelcome.\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("codex lacks %q:\n%s", w, out)
		}
	}
}

// Transcript text cannot drive the terminal: no clipboard writes (OSC 52),
// no title, no fake prompt marks, no 8-bit controls.
func TestConvoStripsControlCharacters(t *testing.T) {
	evil := "ok\x1b]52;c;cm0gLXJmIH4=\x07\x1b]0;title\x07\x1b]133;A\x1b\\\x9b2J\x7f\rdone\tend\n\xc2\x9dline\xff"
	s := &transcript.Session{Provider: "claude", Model: "m\x1b[31m", Turns: []transcript.Turn{
		{Prompt: evil, Steers: []string{evil}, Earlier: []string{evil + " 1"}, Reply: evil + " 2", Started: time.Now(), Ended: time.Now()},
		{Prompt: evil, Source: transcript.Notice, Reply: evil},
	}}
	for _, color := range []bool{false, true} {
		out := renderConvo(s, convoTarget{}, 0, color, time.Now())
		out = strings.NewReplacer(sgrReset, "", sgrBold, "", sgrDim, "", sgrBoldCyan, "", sgrBoldGreen, "", sgrBand, "").Replace(out)
		for _, r := range out {
			if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r <= 0x9f {
				t.Fatalf("color=%v: control character %U in\n%q", color, r, out)
			}
		}
		if !strings.Contains(out, "ok]52;c;cm0gLXJmIH4=]0;title]133;A\\\ufffd2Jdone") || !strings.Contains(out, "\nline\ufffd") {
			t.Errorf("color=%v: text around the controls lost:\n%q", color, out)
		}
	}
	var b bytes.Buffer
	f := &convoFollow{w: &b, tty: true}
	f.setStatus("pane %1 runs \x1b]52;c;eA==\x07x")
	if strings.Contains(b.String(), "]52;c;eA==\x07") {
		t.Errorf("status line not cleaned: %q", b.String())
	}
}

func TestConvoBindingsQuoteOnlyPlainPaths(t *testing.T) {
	popup, split, note := convoBindings("/opt/homebrew/bin/aiq")
	if note != "" || !strings.Contains(popup, "-EE '/opt/homebrew/bin/aiq convo --pane #{pane_id}' || true") ||
		!strings.Contains(split, "'/opt/homebrew/bin/aiq convo --follow --pane #{pane_id}'") {
		t.Fatalf("plain path:\n%s\n%s\n%s", popup, split, note)
	}
	for _, exe := range []string{"/Users/a b/bin/aiq", "/x/it's/aiq", `/x/"q"/aiq`, "/x/$(id)/aiq"} {
		popup, split, note := convoBindings(exe)
		if note == "" || !strings.Contains(popup, "'aiq convo --pane #{pane_id}'") || !strings.Contains(split, "'aiq convo --follow") ||
			strings.Contains(popup+split, exe) {
			t.Errorf("%q:\n%s\n%s\n%s", exe, popup, split, note)
		}
	}
}

func TestParseConvoArgsRejectsAnEmptyPane(t *testing.T) {
	for _, args := range [][]string{{"--pane="}, {"--pane", ""}} {
		if _, err := parseConvoArgs(args); err == nil {
			t.Errorf("%q: want an error", args)
		}
	}
}

// A local command shows no header while it may still come to nothing: the
// parser drops it once the next prompt starts.
func TestConvoHoldsBackALocalCommand(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	p := newConvoPrinter("claude", false, false)
	out := p.emit(&transcript.Session{Turns: []transcript.Turn{{Prompt: "/model opus", Open: true, Started: at}}})
	out += p.emit(&transcript.Session{Turns: []transcript.Turn{{Prompt: "hello", Reply: "Hi.", Started: at.Add(time.Minute), Ended: at.Add(time.Minute)}}})
	if strings.Contains(out, "/model") || !strings.Contains(out, "\nhello\n") {
		t.Fatalf("got:\n%s", out)
	}
	// One that does work shows up.
	out = p.emit(&transcript.Session{Turns: []transcript.Turn{{Prompt: "/review", Open: true, Tools: 1, Started: at.Add(time.Hour)}}})
	if !strings.Contains(out, "/review") {
		t.Fatalf("got:\n%s", out)
	}
}

// A session nobody runs any more: its open turn is over, and the answer it
// gave last is its reply.
func TestLoadConvoGivesAStaleTurnItsLastAnswer(t *testing.T) {
	var c fixtureLines
	c.typed("check the deploy")
	c.answer("m1", "The deploy is fine.")
	c.notification()
	c.tool("m2")
	path := filepath.Join(t.TempDir(), "s.jsonl")
	os.WriteFile(path, []byte(strings.Join(c.lines, "\n")+"\n"), 0o600)
	live, err := loadConvo(convoTarget{provider: "claude", path: path})
	if err != nil || !live.Turns[0].Open {
		t.Fatalf("live: %+v, %v", live, err)
	}
	stale, err := loadConvo(convoTarget{provider: "claude", path: path, stale: true})
	if tr := stale.Turns[0]; err != nil || tr.Open || tr.Reply != "The deploy is fine." || len(tr.Earlier) != 0 {
		t.Fatalf("stale: %+v, %v", tr, err)
	}
}

func TestConvoPrintsAnswersTheAgentRepeats(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{
		{Prompt: "fix the build", Earlier: []string{"Done.", "Build still red."}, Reply: "Done.", Started: at, Ended: at.Add(9 * time.Minute), Tools: 12},
		{Prompt: "watch CI", Earlier: []string{"Waiting.", "Waiting."}, Reply: "Green.", Started: at.Add(10 * time.Minute), Ended: at.Add(20 * time.Minute)},
	}}
	out := renderConvo(s, convoTarget{}, 0, false, at)
	if n := strings.Count(out, "Done."); n != 2 {
		t.Errorf("Done. printed %d times, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "▌ claude · 9m · 12 tools\nDone.") {
		t.Errorf("final reply missing:\n%s", out)
	}
	if n := strings.Count(out, "Waiting."); n != 2 {
		t.Errorf("Waiting. printed %d times, want 2:\n%s", n, out)
	}

	// A follow run that saw the turn before its last stretch prints the
	// same: the reply that moves out of Earlier is not printed again.
	p := newConvoPrinter("claude", false, false)
	early := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{
		{Prompt: "fix the build", Earlier: []string{"Done."}, Open: true, Started: at},
	}}
	got := p.emit(early) + p.emit(&transcript.Session{Provider: "claude", Turns: s.Turns[:1]})
	if n := strings.Count(got, "Done."); n != 2 || !strings.Contains(got, "Build still red.") {
		t.Errorf("follow output:\n%s", got)
	}
}

func TestSanitizeKeepsTextAndDropsControls(t *testing.T) {
	in := "tab\there 日本語 👍🏽 é\x1b]52;c;Zm9v\x07‮evil⁦x⁩ next"
	want := "tab\there 日本語 👍🏽 é]52;c;Zm9vevilx\nnext"
	if got := sanitize(in); got != want {
		t.Errorf("sanitize = %q, want %q", got, want)
	}
}

func TestSlashCommand(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{{"/model", true}, {"/mcp list", true}, {"/Users/x/main.go panics on start, fix it", false}, {"/", false}, {"fix /model", false}} {
		if got := slashCommand(c.in); got != c.want {
			t.Errorf("slashCommand(%q) = %v", c.in, got)
		}
	}
}

func TestSanitizeDropsDirectionMarks(t *testing.T) {
	if got := sanitize("a‎b‏c؜d"); got != "abcd" {
		t.Errorf("sanitize = %q", got)
	}
}

func TestFirstLineStopsAtALineSeparator(t *testing.T) {
	if got := firstLine("ok ▌ you · fake"); got != "ok" {
		t.Errorf("firstLine = %q", got)
	}
}

func TestSessionIDOfAntigravity(t *testing.T) {
	if got := sessionIDOf("/h/.gemini/antigravity/brain/abc-123/.system_generated/logs/transcript.jsonl"); got != "abc-123" {
		t.Errorf("got %q", got)
	}
}

func TestDepth(t *testing.T) {
	parents := map[int]int{10: 1, 20: 10, 30: 20}
	if depth(parents, 30) != 3 || depth(parents, 10) != 1 {
		t.Errorf("depth: %d %d", depth(parents, 30), depth(parents, 10))
	}
}

// A stopped session writes nothing more, so an Unfinished file is final.
func TestFollowDoesNotHoldAStaleSession(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	for _, stale := range []bool{false, true} {
		s := &transcript.Session{Provider: "claude", Unfinished: true, Turns: []transcript.Turn{
			{Prompt: "go", Reply: "Done.", Started: at, Ended: at},
		}}
		f := &convoFollow{tgt: convoTarget{stale: stale}, quiet: time.Hour}
		p := newConvoPrinter("claude", false, false)
		p.live = true
		got := f.emit(p, s, time.Now())
		if held := !strings.Contains(got, "Done."); held == stale {
			t.Errorf("stale=%v: held=%v\n%s", stale, held, got)
		}
	}
}

// What the person typed is a grey band the width of the terminal: every
// line the same width, opened with the background, closed by one reset.
func TestHumanBlocksAreAFullWidthBand(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	prompt := "fix the build, then 東京都の天気を調べて and a verylongwordthatcannotfitinsideoneline" +
		"atallbecauseitgoesonandon\n\n  indented line\x1b]52;c;eA==\x07"
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{
		{Prompt: prompt, Steers: []string{"日本語日本語日本語日本語日本語日本語日本語"}, Reply: "**done**", Started: at, Ended: at},
	}}
	for _, width := range []int{10, 23, 40, 80} {
		p := newConvoPrinter("claude", true, true)
		p.width = width
		out := p.emit(s)
		var band []string
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimPrefix(l, osc133Prompt)
			if strings.Contains(l, sgrBand) {
				band = append(band, l)
			}
		}
		if len(band) < 8 {
			t.Fatalf("width %d: %d band lines:\n%q", width, len(band), out)
		}
		for _, l := range band {
			if !strings.HasPrefix(l, sgrBand) || !strings.HasSuffix(l, sgrReset) || strings.Count(l, sgrReset) != 1 {
				t.Errorf("width %d: band line not opened by the grey and closed by one reset: %q", width, l)
			}
			if w := cells(sgrPattern.ReplaceAllString(l, "")); w != width {
				t.Errorf("width %d: band line is %d cells: %q", width, w, l)
			}
			if strings.Contains(l, "\x1b]52") {
				t.Errorf("escape survived: %q", l)
			}
		}
		if strings.Count(out, osc133Prompt) != 2 || !strings.Contains(out, "\n"+osc133Prompt+sgrBand+sgrBoldCyan+"▌ you · ") {
			t.Errorf("width %d: prompt marks: %q", width, out)
		}
		// The answer is no band.
		if !strings.Contains(out, "\n"+sgrBold+"done"+sgrReset+"\n") {
			t.Errorf("width %d: answer: %q", width, out)
		}
	}
}

// While a held reply may still be coming, --follow says the agent works on.
func TestFollowShowsAHeldTurnAsWorking(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	s := &transcript.Session{Provider: "claude", Unfinished: true, Turns: []transcript.Turn{
		{Prompt: "go", Reply: "Let me check.", Started: at, Ended: at},
	}}
	f := &convoFollow{quiet: time.Hour}
	p := newConvoPrinter("claude", false, false)
	p.live = true
	out := f.emit(p, s, time.Now())
	if strings.Contains(out, "Let me check.") || !p.pending || workingLabel(s, time.Now()) == "" {
		t.Errorf("pending=%v working=%q\n%s", p.pending, workingLabel(s, time.Now()), out)
	}
}

func TestTurnKeyTellsTurnsWithTheSameStartApart(t *testing.T) {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	a, b := transcript.Turn{Prompt: "again", Started: at}, transcript.Turn{Prompt: "again", Started: at}
	if turnKey(0, a) == turnKey(1, b) {
		t.Error("same key for two turns")
	}
}

func TestAmbiguousErrIsTyped(t *testing.T) {
	var err error = ambiguousErr("2 live sessions")
	if !errors.As(err, new(ambiguousErr)) {
		t.Error("errors.As misses ambiguousErr")
	}
}

// codexLines builds a Codex rollout, one second a record.
type codexLines struct {
	lines []string
	sec   int
}

func (c *codexLines) add(typ, payload string) {
	c.sec++
	c.lines = append(c.lines, fmt.Sprintf(`{"timestamp":"2026-10-06T20:%02d:%02dZ","type":"%s","payload":%s}`, 31+c.sec/60, c.sec%60, typ, payload))
}

func (c *codexLines) started(turn string) {
	c.add("event_msg", `{"type":"task_started","turn_id":"`+turn+`"}`)
}

func (c *codexLines) user(turn, text string) {
	c.add("response_item", `{"type":"message","role":"user","content":[{"type":"input_text","text":`+strconv.Quote(text)+
		`}],"internal_chat_message_metadata_passthrough":{"turn_id":"`+turn+`","content_item_kinds":["user.text"]}}`)
}

func (c *codexLines) say(phase, text string) {
	c.add("response_item", `{"type":"message","role":"assistant","phase":"`+phase+`","content":[{"type":"output_text","text":`+strconv.Quote(text)+`}]}`)
}

func (c *codexLines) call() { c.add("response_item", `{"type":"custom_tool_call","name":"exec"}`) }

func (c *codexLines) rollout() []string {
	return append([]string{`{"timestamp":"2026-10-06T20:30:00Z","type":"session_meta","payload":{"id":"c","cwd":"/w","source":"cli"}}`}, c.lines...)
}

// opsSixRollout is the shape of the session the feature came from: the
// person steers a long Codex turn, and Codex answers each steer within
// seconds in commentary, between commentary that answers nothing.
func opsSixRollout() []string {
	var c codexLines
	c.started("a")
	c.user("a", "fix the review table")
	c.say("commentary", "Looking at the review page first.")
	c.call()
	c.say("commentary", "The second batch confirms three issues:\n\n- Finding detail hides the table")
	c.user("a", "QQ: why are you opening the video files in quicktime?")
	c.say("commentary", "QuickTime is only a permission bridge. macOS lets this session read the files through it.")
	c.call()
	c.user("a", "oh, it was the Findings that has the table.")
	c.user("a", "review still shows cards")
	c.say("commentary", "Correct. Findings already has a table; Review is still the cards.\n\nI'll move Review onto the table.")
	c.call()
	c.say("commentary", "One Mill maintenance incident to surface now: Accordion armed a watcher.")
	c.call()
	c.say("final_answer", "Review now uses the **issue table**:\n\n- rows keep their order\n- filters persist")
	c.add("event_msg", `{"type":"task_complete","turn_id":"a","last_agent_message":"Review now uses the **issue table**:\n\n- rows keep their order\n- filters persist"}`)
	return c.rollout()
}

func codexReplyFixtures() map[string][]string {
	out := map[string][]string{"ops6": opsSixRollout()}
	var c codexLines
	c.started("a")
	c.user("a", "tidy the logs")
	c.say("commentary", "Starting with the API logs.")
	c.call()
	c.say("commentary", "Halfway there.")
	c.add("event_msg", `{"type":"turn_aborted","turn_id":"a"}`)
	out["aborted, commentary only"] = c.rollout()

	c = codexLines{}
	c.started("a")
	c.user("a", "status?")
	c.say("final_answer", "All green.")
	c.add("event_msg", `{"type":"task_complete","turn_id":"a","last_agent_message":"All green."}`)
	c.started("w") // a wakeup: the same turn goes on
	c.call()
	c.say("commentary", "The nightly run finished.")
	c.say("final_answer", "Still green after the nightly run.")
	c.add("event_msg", `{"type":"task_complete","turn_id":"w","last_agent_message":"Still green after the nightly run."}`)
	out["wakeup"] = c.rollout()

	c = codexLines{}
	c.started("a")
	c.user("a", "go")
	c.say("commentary", "Starting.")
	c.call()
	c.say("commentary", "Done: line one\nline two")
	c.say("final_answer", "Done: line one\nline two")
	c.add("event_msg", `{"type":"task_complete","turn_id":"a","last_agent_message":"Done: line one\nline two"}`)
	out["final answer repeats the last commentary"] = c.rollout()
	return out
}

func claudeReplyFixtures() map[string][]string {
	out := map[string][]string{}
	text := func(s string) string { return `{"type":"text","text":` + strconv.Quote(s) + `}` }
	call := func(c *fixtureLines, id string) {
		c.assistant(id, "tool_use", `{"type":"tool_use","id":"t`+id+`","name":"Bash","input":{}}`)
		c.user(`"promptSource":"system",`, `[{"type":"tool_result","tool_use_id":"t`+id+`","content":"ok"}]`)
	}
	var c fixtureLines
	c.typed("make the plans persist")
	c.assistant("m1", "tool_use", text("Looking at how plans are stored."))
	call(&c, "m1")
	c.steer("will they replay after a day?")
	c.assistant("m2", "tool_use", text("Mostly yes. The plans persist; their results expire after a day."))
	call(&c, "m2")
	c.assistant("m3", "tool_use", text("Running the storage tests."))
	call(&c, "m3")
	c.answer("m4", "Plans persist now. Results still expire after **24 hours**.")
	out["steer answered mid-turn"] = c.lines

	c = fixtureLines{}
	c.typed("find the leak")
	c.assistant("m1", "tool_use", text("Checking the pool."))
	call(&c, "m1")
	c.assistant("m2", "tool_use", text("Found it: the pool never closes idle conns."))
	call(&c, "m2")
	c.assistant("m3", "end_turn", `{"type":"thinking","thinking":"done"}`)
	out["thinking-only final"] = c.lines

	// Records without a stop reason, as older CLIs wrote them.
	c = fixtureLines{}
	plain := func(id, part string) {
		c.lines = append(c.lines, `{"type":"assistant","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":"`+c.ts()+
			`","message":{"id":"`+id+`","model":"claude-opus-4","role":"assistant","content":[`+part+`]}}`)
	}
	c.typed("summarise the diff")
	plain("m1", text("Reading the diff."))
	plain("m1", `{"type":"tool_use","id":"t1","name":"Bash","input":{}}`)
	c.user(`"promptSource":"system",`, `[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)
	plain("m2", text("Two files changed."))
	out["legacy, no stop reasons"] = c.lines

	// Esc after narration: the narration is the turn's answer.
	interrupt := func(c *fixtureLines, id string) {
		c.user(`"origin":{"kind":"human"},"promptSource":"typed",`, `[{"type":"tool_result","tool_use_id":"t`+id+`","content":"x"},{"type":"text","text":"[Request interrupted by user for tool use]"}]`)
	}
	c = fixtureLines{}
	c.typed("run the suite")
	c.assistant("m0", "tool_use", text("Starting."))
	call(&c, "m0")
	c.assistant("m1", "tool_use", text("Suite running; 3 failures so far:\n- a\n- b"))
	c.assistant("m1", "tool_use", `{"type":"tool_use","id":"t1","name":"Bash","input":{}}`)
	interrupt(&c, "1")
	out["interrupted after a multi-line message"] = c.lines

	c = fixtureLines{}
	c.typed("[agent-nudge] PR #4 has a review")
	c.assistant("m1", "tool_use", text("Reading the review comments on PR 4."))
	c.assistant("m1", "tool_use", `{"type":"tool_use","id":"t1","name":"Bash","input":{}}`)
	interrupt(&c, "1")
	out["notice turn interrupted"] = c.lines
	return out
}

func TestFollowMatchesSnapshotWithReplies(t *testing.T) {
	for name, lines := range claudeReplyFixtures() {
		checkFollowMatchesSnapshot(t, name, lines, transcript.ParseClaude)
	}
	for name, lines := range codexReplyFixtures() {
		checkFollowMatchesSnapshot(t, name, lines, transcript.ParseCodex)
	}
}

func renderFixture(t *testing.T, lines []string, parse func(string) (*transcript.Session, error)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := parse(path)
	if err != nil {
		t.Fatal(err)
	}
	out := renderConvo(s, convoTarget{}, 0, false, time.Now())
	_, body, _ := strings.Cut(out, "\n") // the header names the run's time zone
	return body
}

// The reply to each steer shows in full, the rest of the commentary as one
// line each, all in the order it happened.
func TestOpsSixGolden(t *testing.T) {
	at := func(m, s int) string { return time.Date(2026, 10, 6, 20, m, s, 0, time.UTC).Local().Format("15:04:05") }
	want := `
▌ you · ` + whenLabel(time.Date(2026, 10, 6, 20, 31, 2, 0, time.UTC), time.Now()) + `
fix the review table

▌ codex · ` + at(31, 3) + `
Looking at the review page first.
  · The second batch confirms three issues:

▌ you, while it worked
QQ: why are you opening the video files in quicktime?

▌ codex · ` + at(31, 7) + `
QuickTime is only a permission bridge. macOS lets this session read the files through it.

▌ you, while it worked
oh, it was the Findings that has the table.

▌ you, while it worked
review still shows cards

▌ codex · ` + at(31, 11) + `
Correct. Findings already has a table; Review is still the cards.

I'll move Review onto the table.
  · One Mill maintenance incident to surface now: Accordion armed a watcher.

▌ codex · 14s · 4 tools
Review now uses the **issue table**:

- rows keep their order
- filters persist
`
	if got := renderFixture(t, opsSixRollout(), transcript.ParseCodex); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestReplyEdgeCases(t *testing.T) {
	// An aborted Codex turn: its last commentary is the reply, shown once.
	got := renderFixture(t, codexReplyFixtures()["aborted, commentary only"], transcript.ParseCodex)
	if strings.Count(got, "Halfway there.") != 1 || !strings.Contains(got, "\nStarting with the API logs.\n") {
		t.Errorf("aborted:\n%s", got)
	}
	// Claude's thinking-only final: the last text is the reply, once.
	got = renderFixture(t, claudeReplyFixtures()["thinking-only final"], transcript.ParseClaude)
	if strings.Count(got, "Found it") != 1 || !strings.Contains(got, "▌ claude · ") || strings.Contains(got, "(no reply)") {
		t.Errorf("thinking-only:\n%s", got)
	}
	// No stop reasons: nothing can be told apart, so it reads as before.
	got = renderFixture(t, claudeReplyFixtures()["legacy, no stop reasons"], transcript.ParseClaude)
	if strings.Contains(got, "Reading the diff.") || !strings.Contains(got, "\nTwo files changed.\n") {
		t.Errorf("legacy:\n%s", got)
	}
	// A steer answered mid-turn: the answer in full under it, then the
	// narration on one line, then the final.
	got = renderFixture(t, claudeReplyFixtures()["steer answered mid-turn"], transcript.ParseClaude)
	order := []string{"\nLooking at how plans are stored.\n", "will they replay after a day?", "\nMostly yes. The plans persist", "  · Running the storage tests.", "Plans persist now."}
	rest := got
	for _, w := range order {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("lacks %q (in order):\n%s", w, got)
		}
		rest = rest[i+len(w):]
	}
}

// An answer that was first said on the way shows whole, and a notifier's
// turn keeps the answer it ended on.
func TestPromotedAnswers(t *testing.T) {
	got := renderFixture(t, claudeReplyFixtures()["interrupted after a multi-line message"], transcript.ParseClaude)
	if !strings.Contains(got, "\nSuite running; 3 failures so far:\n- a\n- b\n") || strings.Contains(got, "  · Suite running") {
		t.Errorf("interrupted:\n%s", got)
	}
	got = renderFixture(t, claudeReplyFixtures()["notice turn interrupted"], transcript.ParseClaude)
	if !strings.Contains(got, "· notice: [agent-nudge] PR #4 has a review\n  → Reading the review comments on PR 4.\n") {
		t.Errorf("notice:\n%s", got)
	}
	got = renderFixture(t, codexReplyFixtures()["final answer repeats the last commentary"], transcript.ParseCodex)
	if strings.Count(got, "Done: line one") != 1 || !strings.Contains(got, "\nDone: line one\nline two\n") {
		t.Errorf("codex:\n%s", got)
	}
	got = renderFixture(t, codexReplyFixtures()["aborted, commentary only"], transcript.ParseCodex)
	if strings.Count(got, "Halfway there.") != 1 || strings.Contains(got, "  · Halfway") {
		t.Errorf("aborted:\n%s", got)
	}
	// Without a terminal a said line keeps its whole first line.
	long := strings.Repeat("word ", 40)
	p := newConvoPrinter("codex", false, false)
	at := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)
	out := p.emit(&transcript.Session{Turns: []transcript.Turn{{Prompt: "go", Open: true, Started: at, Items: []transcript.TurnItem{
		{Kind: transcript.ItemSaid, Text: "first", At: at}, {Kind: transcript.ItemSaid, Text: long + "\nmore", At: at},
	}}}})
	if !strings.Contains(out, "  · "+strings.TrimSpace(long)+"\n") {
		t.Errorf("said line cut without a terminal:\n%s", out)
	}
}
