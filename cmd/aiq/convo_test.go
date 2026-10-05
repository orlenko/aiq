package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
// a snapshot of the whole file: per turn the same prompts, steers and
// answers, nothing twice, and no "(no reply)" for a turn that has an answer.
func checkFollowMatchesSnapshot(t *testing.T, name string, lines []string, parse func(string) (*transcript.Session, error)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	follow := newConvoPrinter("x", false, false)
	follow.live = true
	followed := pieces(follow)
	var s *transcript.Session
	for n := 1; n <= len(lines); n++ {
		if err := os.WriteFile(path, []byte(strings.Join(lines[:n], "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if s, err = parse(path); err != nil {
			t.Fatal(err)
		}
		follow.hold = s.Unfinished // just written: not yet quiet
		follow.emit(s)
	}
	if follow.pending {
		follow.hold = false // the file went quiet
		follow.emit(s)
	}
	snap := newConvoPrinter("x", false, false)
	snapped := pieces(snap)
	snap.emit(s)

	without := func(list []string, kind string) []string {
		var out []string
		for _, p := range list {
			if !strings.HasPrefix(p, kind+"\x00") {
				out = append(out, p)
			}
		}
		sort.Strings(out)
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
		f, sn := without(followed[k], "none"), without(snapped[k], "none")
		if !reflect.DeepEqual(f, sn) {
			t.Errorf("%s, turn %s:\nfollow   %q\nsnapshot %q", name, k, f, sn)
		}
		for i := 1; i < len(f); i++ {
			if f[i] == f[i-1] {
				t.Errorf("%s, turn %s: printed twice: %q", name, k, f[i])
			}
		}
		answered := len(without(snapped[k], "none")) > len(without(snapped[k], "answer"))
		if answered && (len(followed[k]) != len(f) || len(snapped[k]) != len(sn)) {
			t.Errorf("%s, turn %s: (no reply) for a turn that has an answer", name, k)
		}
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
		out = strings.NewReplacer(sgrReset, "", sgrBold, "", sgrDim, "", sgrBoldCyan, "", sgrBoldGreen, "").Replace(out)
		for _, r := range out {
			if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r <= 0x9f {
				t.Fatalf("color=%v: control character %U in\n%q", color, r, out)
			}
		}
		if !strings.Contains(out, "ok]52;c;cm0gLXJmIH4=]0;title]133;A\\\ufffd2Jdone\tend\nline\ufffd") {
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
