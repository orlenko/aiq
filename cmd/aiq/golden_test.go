package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

// The goldens characterise what aiq prints today, on invented fixtures, so a refactor can
// show it changed nothing: the files under testdata/golden must come out byte for byte.
// -update-goldens rewrites them from the code as it is; a change to one is a change of
// behaviour, to be named as one.
var updateGoldens = flag.Bool("update-goldens", false, "rewrite testdata/golden from the code as it is")

// goldenDir is absolute: some goldens run in a working directory of their own.
var goldenDir, _ = filepath.Abs(filepath.Join("testdata", "golden"))

// visibleEscapes writes the control characters of s as text, so a golden reads as
// text and its escapes show in a diff: a backslash is \\, ESC \e, a carriage return
// \r, any other C0 control but a newline or a tab \xNN, a C1 control \u00NN, and a
// byte that is not UTF-8 \xNN (always 0x80 or above). Different strings stay different.
func visibleEscapes(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\x%02x`, s[0])
			s = s[1:]
			continue
		}
		s = s[size:]
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == 0x1b:
			b.WriteString(`\e`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestVisibleEscapesTellsBytesApart(t *testing.T) {
	for in, want := range map[string]string{
		"\xff": `\xff`, "\ufffd": "\ufffd", "\u0085": `\u0085`, `\xff`: `\\xff`, "\x1b[1m": `\e[1m`, "\x01": `\x01`,
	} {
		if got := visibleEscapes(in); got != want {
			t.Errorf("visibleEscapes(%q) = %q, want %q", in, got, want)
		}
	}
}

// checkGolden compares got with testdata/golden/name, escapes made visible.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	got = visibleEscapes(got)
	if *updateGoldens {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test -run %s -update-goldens writes it)", err, t.Name())
	}
	if want := string(data); got != want {
		g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
		for i := 0; i < len(g) || i < len(w); i++ {
			var gl, wl string
			if i < len(g) {
				gl = g[i]
			}
			if i < len(w) {
				wl = w[i]
			}
			if gl != wl || i >= len(g) || i >= len(w) {
				t.Fatalf("%s differs at line %d:\n got %q\nwant %q", name, i+1, gl, wl)
			}
		}
	}
}

// goldenEnv fixes what the output reads from the process: a UTF-8 locale, which picks
// the Unicode marks, the palette, the Markdown marks, and a stdout that is not a terminal,
// so every width aiq reads from it is its fallback. What fn printed on stdout comes
// back; stderr must stay empty unless a test captures it explicitly.
func goldenEnv(t *testing.T, fn func()) string {
	t.Helper()
	return goldenEnvIn(t, "C.UTF-8", fn)
}

// goldenEnvIn is goldenEnv with LANG set to lang: "C" picks the ASCII marks.
func goldenEnvIn(t *testing.T, lang string, fn func()) string {
	t.Helper()
	out, diagnostics := goldenStreamsIn(t, lang, fn)
	if diagnostics != "" {
		t.Fatalf("unexpected stderr: %q", diagnostics)
	}
	return out
}

// goldenStreamsIn preserves the command's two output streams independently.
func goldenStreamsIn(t *testing.T, lang string, fn func()) (string, string) {
	t.Helper()
	t.Setenv("LC_ALL", "")
	t.Setenv("LC_CTYPE", "")
	t.Setenv("LANG", lang)
	savedPal, savedMarkup, savedCmd := pal, convoMarkup, asksCmd
	pal, convoMarkup, asksCmd = darkPalette, true, "aiq asks"
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	diagnostics, err := os.Create(filepath.Join(t.TempDir(), "err"))
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, diagnostics
	func() {
		defer func() {
			os.Stdout, os.Stderr = stdout, stderr
			pal, convoMarkup, asksCmd = savedPal, savedMarkup, savedCmd
		}()
		fn()
	}()
	data, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := os.ReadFile(diagnostics.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), string(warnings)
}

// goldenAt is a fixture time on the local clock, a year before any run: its local
// rendering is the same in every time zone, and whenLabel shows it as a date.
func goldenAt(day, h, m, s int) time.Time { return time.Date(2025, 9, day, h, m, s, 0, time.Local) }

// goldenClaude builds Claude Code transcript records on the local clock.
type goldenClaude struct {
	cwd, id string
	at      time.Time
	lines   []string
}

func (c *goldenClaude) record(typ, rest string) {
	c.at = c.at.Add(7 * time.Second)
	c.lines = append(c.lines, `{"type":"`+typ+`","isSidechain":false,"cwd":`+strconv.Quote(c.cwd)+`,"sessionId":"`+c.id+
		`","timestamp":"`+c.at.Format(time.RFC3339)+`",`+rest+`}`)
}

func (c *goldenClaude) typed(text string) {
	c.record("user", `"origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":`+strconv.Quote(text)+`}`)
}

func (c *goldenClaude) assistant(id, stop, part string) {
	c.record("assistant", `"message":{"id":"`+id+`","model":"claude-opus-5","stop_reason":"`+stop+`","role":"assistant","content":[`+part+`]}`)
}

// tool is a message said on the way, its tool call and the call's result.
func (c *goldenClaude) tool(id, said string) {
	c.assistant(id, "tool_use", `{"type":"text","text":`+strconv.Quote(said)+`}`)
	c.assistant(id, "tool_use", `{"type":"tool_use","id":"t`+id+`","name":"Bash","input":{}}`)
	c.record("user", `"promptSource":"system","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t`+id+`","content":"ok"}]}`)
}

// answer is a final message as Claude Code writes it: a thinking record, then text.
func (c *goldenClaude) answer(id, text string) {
	c.assistant(id, "end_turn", `{"type":"thinking","thinking":"hm"}`)
	c.assistant(id, "end_turn", `{"type":"text","text":`+strconv.Quote(text)+`}`)
}

func goldenWrite(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// goldenCodex is a Codex rollout on the local clock: one turn for each prompt and last
// message, each with one call.
func goldenCodex(id, cwd, source string, at time.Time, turns ...[2]string) []string {
	sec := 0
	ts := func() string { sec++; return at.Add(time.Duration(sec) * time.Second).Format(time.RFC3339) }
	lines := []string{
		`{"timestamp":"` + ts() + `","type":"session_meta","payload":{"id":"` + id + `","cwd":` + strconv.Quote(cwd) + `,"source":"` + source + `"}}`,
		`{"timestamp":"` + ts() + `","type":"turn_context","payload":{"model":"gpt-6","approval_policy":"on-request","sandbox_policy":{"type":"workspace-write"}}}`,
	}
	for i, turn := range turns {
		n := strconv.Itoa(i)
		lines = append(lines,
			`{"timestamp":"`+ts()+`","type":"event_msg","payload":{"type":"task_started","turn_id":"`+n+`"}}`,
			`{"timestamp":"`+ts()+`","type":"event_msg","payload":{"type":"user_message","message":`+strconv.Quote(turn[0])+`,"turn_id":"`+n+`"}}`,
			`{"timestamp":"`+ts()+`","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
			`{"timestamp":"`+ts()+`","type":"event_msg","payload":{"type":"task_complete","turn_id":"`+n+`","last_agent_message":`+strconv.Quote(turn[1])+`}}`)
	}
	return lines
}

const (
	goldenC1 = "c0ffee01-0000-4000-8000-000000000001"
	goldenC2 = "c0ffee02-0000-4000-8000-000000000002"
	goldenX1 = "019a0000-0000-7000-8000-0000000000a1"
	goldenX2 = "019b0000-0000-7000-8000-0000000000b2"
)

// TestGoldenResumePrint is aiq resume --print end to end, on temporary homes: two Claude
// sessions, one open and one started through a launcher, a Codex session running in aiq
// long, and a Codex worker.
func TestGoldenResumePrint(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, claudeHome, codexHome := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("AIQ_CONFIG", filepath.Join(data, "config.toml"))
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("COPILOT_HOME", t.TempDir())
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	work := filepath.Join(home, "work", "demo")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("PWD", work)

	project := filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"))
	c1 := &goldenClaude{cwd: work, id: goldenC1, at: goldenAt(1, 9, 0, 0)}
	c1.typed("rename the config loader")
	c1.tool("m1", "Looking at the loader.")
	c1.answer("m2", "Renamed it to `loadConfig` and updated the callers.")
	c1.typed("[agent-nudge] PR #12 has a review")
	c1.answer("m3", "Read the review; nothing to change.")
	goldenWrite(t, filepath.Join(project, goldenC1+".jsonl"), c1.lines...)
	c2 := &goldenClaude{cwd: work, id: goldenC2, at: goldenAt(2, 10, 0, 0)}
	c2.typed("add a test for the parser\nwith two cases")
	c2.answer("m1", "Added `TestParse` with both cases.")
	goldenWrite(t, filepath.Join(project, goldenC2+".jsonl"), c2.lines...)
	// C2 is open in this process.
	goldenWrite(t, filepath.Join(claudeHome, "sessions", "1.json"), fmt.Sprintf(`{"pid":%d,"sessionId":"%s","cwd":%q}`, os.Getpid(), goldenC2, work))
	day := filepath.Join(codexHome, "sessions", "2025", "09", "03")
	goldenWrite(t, filepath.Join(day, "rollout-2025-09-03T11-00-00-"+goldenX1+".jsonl"),
		goldenCodex(goldenX1, work, "cli", goldenAt(3, 11, 0, 0), [2]string{"ship the release", "Shipped 1.4.0."})...)
	goldenWrite(t, filepath.Join(day, "rollout-2025-09-03T12-00-00-"+goldenX2+".jsonl"),
		goldenCodex(goldenX2, work, "exec", goldenAt(3, 12, 0, 0), [2]string{"summarise the diff", "Two files changed."})...)
	goldenWrite(t, filepath.Join(codexHome, "session_index.jsonl"), `{"id":"`+goldenX1+`","thread_name":"release 1.4"}`)

	st, err := state.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []state.Launch{
		{StartedAt: goldenAt(1, 8, 59, 0), Hostname: hostname(), Provider: "claude", AccountID: "claude/a", Launcher: "safe-claude",
			Cwd: work, Mode: state.ModeInteractive, SessionID: goldenC1, Args: []string{"--dangerously-skip-permissions"}},
		{StartedAt: goldenAt(2, 9, 59, 0), Hostname: hostname(), Provider: "claude", AccountID: "claude/a", Launcher: "box",
			Cwd: work, Mode: state.ModeInteractive},
	} {
		if _, err := st.AddLaunch(l); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddLease(state.Lease{AccountID: "codex/b", Provider: "codex", Mode: state.ModeLong, PID: os.Getpid(), Hostname: hostname(),
		Workspace: "/invented/ws/demo", Cwd: "/invented/ws/demo", SessionID: goldenX1, StartedAt: goldenAt(3, 10, 59, 0).Unix()}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	var b strings.Builder
	for _, args := range [][]string{{"--print"}, {"--print", "--all"}, {"--print", "c0ffee01"}, {"--print", goldenX1}, {"--print", "c0ffee"}} {
		fmt.Fprintf(&b, "=== aiq resume %s\n", strings.Join(args, " "))
		var runErr error
		b.WriteString(goldenEnv(t, func() { runErr = cmdResume(args) }))
		if runErr != nil {
			fmt.Fprintf(&b, "error: %v\n", runErr)
		}
	}
	// A shell that reached the directory through a link: $PWD spells it so, and a launch
	// recorded under that spelling explains a session. A launch on another host does not.
	link := filepath.Join(home, "demo-link")
	if err := os.Symlink(work, link); err != nil {
		t.Fatal(err)
	}
	c5 := &goldenClaude{cwd: work, id: "c0ffee05-0000-4000-8000-000000000005", at: goldenAt(5, 10, 0, 0)}
	c5.typed("check the link")
	c5.answer("m1", "Checked.")
	goldenWrite(t, filepath.Join(project, c5.id+".jsonl"), c5.lines...)
	if st, err = state.Open(filepath.Join(data, "state.db")); err != nil {
		t.Fatal(err)
	}
	for _, l := range []state.Launch{
		{StartedAt: goldenAt(5, 9, 59, 0), Hostname: hostname(), Provider: "claude", AccountID: "claude/a", Launcher: "linked",
			Cwd: link, Mode: state.ModeInteractive},
		{StartedAt: goldenAt(5, 9, 59, 30), Hostname: "invented-other-host", Provider: "claude", AccountID: "claude/a", Launcher: "elsewhere",
			Cwd: work, Mode: state.ModeInteractive, SessionID: c5.id},
	} {
		if _, err := st.AddLaunch(l); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	// os.Getwd answers $PWD as it is spelled, trailing slash and all; loadOrigins adds it
	// cleaned.
	t.Setenv("PWD", link+"/")
	b.WriteString("=== aiq resume --print, $PWD through a link\n")
	var runErr error
	b.WriteString(goldenEnv(t, func() { runErr = cmdResume([]string{"--print"}) }))
	if runErr != nil {
		fmt.Fprintf(&b, "error: %v\n", runErr)
	}
	checkGolden(t, "resume-print.golden", goldenPID(b.String(), os.Getpid()))
}

// goldenPID writes the test's own pid as <test> where a live note names it, and nowhere
// else: a pid can be spelled like a year or part of an id.
func goldenPID(s string, pid int) string {
	return strings.ReplaceAll(s, fmt.Sprintf("[open in pid %d]", pid), "[open in pid <test>]")
}

// A pid spelled like the fixtures' year or an id's group leaves their dates, ids and paths
// as they are.
func TestGoldenPIDTouchesOnlyTheLiveNote(t *testing.T) {
	for _, pid := range []int{2025, 4000, 7000, 8000} {
		fixture := "c0ffee02  2025-09-02  sessions/2025/09/03/rollout-2025-09-03T11-00-00-019a0000-0000-7000-8000-0000000000a1.jsonl"
		in := fmt.Sprintf("%s [open in pid %d]\n", fixture, pid)
		if got, want := goldenPID(in, pid), fixture+" [open in pid <test>]\n"; got != want {
			t.Errorf("pid %d: got %q, want %q", pid, got, want)
		}
	}
}

// goldenDeadPID is a pid that no longer runs: a child that has exited.
func goldenDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// goldenConvo is a session with something of every kind aiq convo shows.
func goldenConvo() *transcript.Session {
	at := func(m, s int) time.Time { return goldenAt(4, 9, m, s) }
	return &transcript.Session{Provider: "claude", ID: "c0ffee04-0000-4000-8000-000000000004", Cwd: "/invented/home/work/demo",
		Model: "claude-opus-5-20250901", Skipped: 1, Turns: []transcript.Turn{
			{Prompt: "summarise the failing tests", Started: at(0, 0), Ended: at(3, 0), Tools: 3,
				Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "Running the suite.", MsgID: "m1", At: at(0, 10)}},
				Reply: "Two tests fail:\n\n1. `TestLoad`: **timeout**\n2. `TestSave`: *wrong path*\n\n" +
					"| test | cause |\n| --- | --- |\n| TestLoad | timeout after 30 s |\n| TestSave | path |\n\n" +
					"```go\nt.Setenv(\"X\", \"1\")\n```\n\n> a quoted line\n\nSee [the guide](https://example.invalid/guide)."},
			{Prompt: "aiq moved this session to claude/b", Source: transcript.Aiq, Reply: "Picking up where it left off.\nmore", Started: at(4, 0), Ended: at(5, 0)},
			{Prompt: "fix the build", Started: at(10, 0), Ended: at(22, 0), Tools: 47, Items: []transcript.TurnItem{
				{Kind: transcript.ItemSaid, Text: "Starting on the fix.", MsgID: "m3", At: at(10, 5)},
				{Kind: transcript.ItemSteer, Text: "also update the changelog", At: at(11, 0)},
				{Kind: transcript.ItemSaid, Text: "Fixed both; updating the changelog now.\nIt has two entries.", MsgID: "m4", At: at(12, 30)},
				{Kind: transcript.ItemAnswer, Text: "Changelog updated.", At: at(15, 0)},
				{Kind: transcript.ItemSaid, Text: "Checking CI once more.", MsgID: "m6", At: at(16, 0)},
			}, Earlier: []string{"Changelog updated."}, Reply: "All done: the fix and the changelog."},
			{Prompt: "Another Claude session sent a message: hi", Source: transcript.Peer, Reply: "Answered the peer.", Started: at(23, 0), Ended: at(24, 0)},
			{Prompt: transcript.OrchestraNotice + " Read it with the inbox command.", Source: transcript.Notice, Started: at(25, 0), Ended: at(25, 0)},
			{Prompt: "[agent-nudge] PR #4 has a review", Source: transcript.Notice, Started: at(26, 0), Ended: at(26, 0)},
			{Prompt: "silent one", Started: at(30, 0), Ended: at(31, 0)},
			{Source: transcript.Human, Skipped: 1, PromptSkipped: true, Started: at(32, 0), Ended: at(33, 0), Reply: "Handled the long paste."},
			{Prompt: "a prompt that runs on for long enough that the band has to wrap it onto a second line at the narrow width",
				Started: at(34, 0), Ended: at(35, 0), Reply: "Wrapped.", Skipped: 1},
			{Prompt: "now ship it", Open: true, Started: at(40, 0), Tools: 5, Items: []transcript.TurnItem{
				{Kind: transcript.ItemSaid, Text: "Tagging v1.2.", MsgID: "m9", At: at(40, 30)},
				{Kind: transcript.ItemSteer, Text: "wait for me", At: at(41, 0)},
			}},
		}}
}

// TestGoldenRenderConvo is the aiq convo snapshot, plain and coloured, whole and with
// --last.
func TestGoldenRenderConvo(t *testing.T) {
	t.Setenv("HOME", "/invented/home")
	now := goldenAt(4, 9, 52, 0)
	var b strings.Builder
	for _, c := range []struct {
		name  string
		tgt   convoTarget
		last  int
		color bool
		lang  string
	}{
		{"plain, account and pane", convoTarget{account: "claude/a", pane: "%3"}, 0, false, "C.UTF-8"},
		{"colour, account and pane", convoTarget{account: "claude/a", pane: "%3"}, 0, true, "C.UTF-8"},
		{"plain, --last 2", convoTarget{}, 2, false, "C.UTF-8"},
		{"colour, --last 2", convoTarget{}, 2, true, "C.UTF-8"},
		{"colour, markup hidden", convoTarget{}, 0, true, "C.UTF-8"},
		{"colour, account and pane, LANG=C: ASCII marks", convoTarget{account: "claude/a", pane: "%3"}, 0, true, "C"},
	} {
		fmt.Fprintf(&b, "=== %s\n", c.name)
		b.WriteString(goldenEnvIn(t, c.lang, func() {
			if strings.Contains(c.name, "markup hidden") {
				convoMarkup = false
			}
			fmt.Print(renderConvo(goldenConvo(), c.tgt, c.last, c.color, now))
		}))
	}
	checkGolden(t, "render-convo.golden", b.String())
}

// TestGoldenAsksSections is aiq asks list in each state of the watcher, its sections
// plain and with --all, and the rules that sort and filter its items.
func TestGoldenAsksSections(t *testing.T) {
	now := goldenAt(5, 15, 30, 0)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	items := []state.Ask{
		{ID: 1, State: state.AskOpen, Workspace: "/invented/ws/alpha", Provider: "claude", RaisedAt: ago(5 * time.Minute), AsksReader: .9,
			FirstQuote: "Shall I merge the docs branch?\nIt has two commits.", Repeats: 1},
		{ID: 2, State: state.AskOpen, Workspace: "/invented/ws/beta", Provider: "codex", RaisedAt: ago(2 * time.Hour), Risk: .9, AsksReader: .2,
			FirstQuote: "The example token is printed in the log."},
		{ID: 3, State: state.AskOpen, Workspace: "/invented/ws/alpha", Provider: "claude", RaisedAt: ago(72 * time.Hour), AsksReader: .9,
			FirstQuote: "Which port should the stub use?"},
		{ID: 4, State: state.AskOpen, Workspace: "/invented/ws/gamma", Provider: "agy", RaisedAt: ago(30 * time.Second), AsksReader: .3,
			FirstQuote: "Noted for later."},
		{ID: 5, State: state.AskProbablyDone, Workspace: "/invented/ws/alpha", Provider: "claude", RaisedAt: ago(3 * time.Hour),
			FirstQuote: "Run the example check?", StateReason: "the agent ran it"},
		{ID: 6, State: state.AskClosed, Workspace: "/invented/ws/beta", Provider: "codex", RaisedAt: ago(26 * time.Hour), FirstQuote: "Bump the version?"},
		{ID: 7, State: state.AskDismissed, Workspace: "/invented/ws/beta", Provider: "codex", RaisedAt: ago(9 * 24 * time.Hour), FirstQuote: "Old question."},
	}
	moved := []state.Ask{
		{ID: 6, State: state.AskClosed, FirstQuote: "Bump the version?", StateText: "Bumped to 1.4.0 in the release commit."},
		{ID: 5, State: state.AskProbablyDone, FirstQuote: "Run the example check?", StateText: "The check passed."},
	}
	watching := state.AsksStatus{PassAt: ago(40 * time.Second), Watched: 3}
	var b strings.Builder
	b.WriteString(goldenEnv(t, func() {
		fmt.Println("=== list")
		fmt.Print(renderAsksList(items, moved, watching, true, false, now, asksCmd))
		fmt.Println("=== list --all, a skipped text, an unwatched workspace")
		st := watching
		st.Skipped, st.NotWatched = 2, []string{"/invented/ws/delta"}
		fmt.Print(renderAsksList(items, moved, st, true, true, now, asksCmd))
		fmt.Println("=== nothing open")
		fmt.Print(renderAsksList(nil, nil, watching, true, false, now, asksCmd))

		fmt.Println("=== status lines")
		for _, c := range []struct {
			st    state.AsksStatus
			keyOK bool
			open  int
		}{
			{watching, false, 1},
			{state.AsksStatus{}, true, 0},
			{state.AsksStatus{PassAt: ago(9 * time.Minute), Watched: 3}, true, 0},
			{state.AsksStatus{PassAt: ago(time.Minute), LastError: "example failure", ErrorAt: ago(3 * time.Minute)}, true, 2},
			{state.AsksStatus{PassAt: ago(9 * time.Minute), LastError: "example failure", ErrorAt: ago(9 * time.Minute), NextTryAt: now.Add(6 * time.Minute).Unix()}, true, 2},
			{state.AsksStatus{PassAt: ago(time.Minute), Waiting: 4, Watched: 3}, true, 2},
			{watching, true, 0},
			{state.AsksStatus{PassAt: ago(50 * time.Minute), Watched: 1}, true, 2},
		} {
			fmt.Println(asksStatusLine(c.st, c.keyOK, c.open, now, asksCmd))
		}

		fmt.Println("=== sections")
		groups := asksGroups(items, now)
		for _, name := range asksSections {
			var ids []string
			for _, a := range groups[name] {
				ids = append(ids, "#"+strconv.FormatInt(a.ID, 10))
			}
			fmt.Printf("%s: %s\n", name, strings.Join(ids, " "))
		}

		fmt.Println("=== in a directory")
		for _, c := range [][2]string{
			{"/invented/ws/alpha", "/invented/ws/alpha"},
			{"/invented/ws/alpha", "/invented/ws/alpha/sub"},
			{"/invented/ws/alpha/sub", "/invented/ws/alpha"},
			{"/invented/ws/alpha", "/invented/ws/alphabet"},
			{"/invented/ws/alpha", "/invented/ws/"},
			{"", "/invented/ws/alpha"},
			{"/invented/ws/alpha", ""},
		} {
			fmt.Printf("%q in %q: %v\n", c[0], c[1], asksInDir(c[0], c[1], asksRealPath))
		}
		for _, it := range asksFilter(items, "/invented/ws/beta", asksRealPath) {
			fmt.Printf("under beta: #%d\n", it.ID)
		}
	}))
	checkGolden(t, "asks-sections.golden", b.String())
}

// TestGoldenAsksList is aiq asks list as typed, with and without --here and --all, on a
// temporary data directory: a key, a watcher's report, and asks in two workspaces, two of
// them moved today. One clock drives both the fixture and each command invocation,
// including a transition across midnight.
func TestGoldenAsksList(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config", "config.toml"))
	alpha, beta := filepath.Join(root, "ws", "alpha"), filepath.Join(root, "ws", "beta")
	for _, d := range []string{filepath.Join(root, "data"), alpha, beta} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	goldenWrite(t, filepath.Join(root, "config", asksKeyName), "invented-key")
	st, err := state.Open(filepath.Join(root, "data", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	reg, err := state.Open(filepath.Join(root, "data", asksDBName))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if err := reg.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	now := goldenAt(11, 23, 59, 59)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	ask := func(ws string, s state.AskState, raised, moved int64, quote string) state.Ask {
		return state.Ask{Chain: "example", Workspace: ws, Provider: "claude", SessionID: "example-session",
			RaisedAt: raised, State: s, StateAt: moved, FirstQuote: quote, LastQuote: quote}
	}
	items := []state.Ask{
		ask(alpha, state.AskOpen, ago(150*time.Minute), ago(150*time.Minute), "Shall I merge the docs branch?"),
		ask(beta, state.AskOpen, ago(210*time.Minute), ago(210*time.Minute), "The example token is in the log."),
		ask(alpha, state.AskProbablyDone, ago(330*time.Minute), now.Unix(), "Run the example check?"),
		ask(beta, state.AskClosed, ago(390*time.Minute), now.Unix(), "Bump the version?"),
		ask(alpha, state.AskDismissed, ago(30*time.Hour+30*time.Minute), ago(29*time.Hour), "Old question?"),
	}
	items[0].AsksReader = .9
	items[1].Risk, items[1].AsksReader = .9, .2
	items[2].StateReason, items[2].StateText = "reply", "The check ran."
	items[3].StateReason, items[3].StateText = "reply", "Bumped to 1.4.0."
	asksSeed(t, reg, items...)
	// A pass a minute ago with texts waiting reads as "Reading", which names no age, for
	// four minutes.
	if err := reg.SetAsksStatus(state.AsksStatus{PassAt: ago(time.Minute), Watched: 2, Waiting: 3}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(alpha)

	var b strings.Builder
	for _, args := range [][]string{{}, {"--all"}, {"--here"}, {"--here", "--all"}} {
		fmt.Fprintf(&b, "=== %s\n", strings.TrimSpace("aiq asks list "+strings.Join(args, " ")))
		var runErr error
		b.WriteString(goldenEnv(t, func() {
			runErr = cmdAsksWithClock(append([]string{"list"}, args...), func() time.Time { return now })
		}))
		if runErr != nil {
			fmt.Fprintf(&b, "error: %v\n", runErr)
		}
	}
	// This move is exactly at the next day's inclusive lower bound. Yesterday's
	// two moves disappear, while this one remains at and after midnight.
	asksSeed(t, reg, ask(alpha, state.AskClosed, ago(time.Hour), now.Add(time.Second).Unix(), "Moved at midnight."))
	for _, after := range []time.Duration{time.Second, 2 * time.Second} {
		fmt.Fprintf(&b, "=== aiq asks list --all, %s after the fixture clock\n", after)
		b.WriteString(goldenEnv(t, func() {
			if err := cmdAsksWithClock([]string{"list", "--all"}, func() time.Time { return now.Add(after) }); err != nil {
				t.Fatal(err)
			}
		}))
	}
	// The asks app's live sessions: long leases on this host whose process runs.
	gone := goldenDeadPID(t)
	if st, err = state.Open(filepath.Join(root, "data", "state.db")); err != nil {
		t.Fatal(err)
	}
	for _, l := range []state.Lease{
		{AccountID: "claude/a", PID: os.Getpid(), Hostname: hostname(), Workspace: alpha},
		{AccountID: "claude/b", PID: gone, Hostname: hostname(), Workspace: beta},
		{AccountID: "claude/c", PID: os.Getpid(), Hostname: "invented-other-host", Workspace: beta},
	} {
		l.Provider, l.Mode, l.Cwd, l.StartedAt = "claude", state.ModeLong, l.Workspace, now.Unix()
		if _, err := st.AddLease(l); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	leases, err := state.OpenReadOnly(filepath.Join(root, "data", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer leases.Close()
	snap, err := asksLoad(reg, leases, false, now)
	if err != nil {
		t.Fatal(err)
	}
	b.WriteString("=== the asks app's live leases\n")
	for _, l := range snap.leases {
		fmt.Fprintf(&b, "%s in %s\n", l.AccountID, l.Workspace)
	}
	checkGolden(t, "asks-list.golden", strings.ReplaceAll(b.String(), root, "<root>"))
}

// goldenFollow is one --follow run, step by step, as convoFollow.loop composes it: the
// header and the first emit, then one emit for each later parse, with the printer's width
// what termWidth gave. The steps name what changed before each emit.
type goldenFollow struct {
	t     *testing.T
	width int
	color bool // a terminal: colour and marks both
	b     strings.Builder
	p     *convoPrinter
}

func (g *goldenFollow) step(name string) { fmt.Fprintf(&g.b, "--- %s\n", name) }

// begin starts on a conversation as loop does, --last applied.
func (g *goldenFollow) begin(s *transcript.Session, tgt convoTarget, last int, hold bool) {
	g.p = newConvoPrinter(s.Provider, g.color, g.color)
	g.p.live = true
	from := convoStart(s.Turns, last)
	g.p.skip(s.Turns[:from])
	g.p.width = g.width
	g.b.WriteString(convoHeader(s, tgt, from, g.color) + followEmit(g.p, s, hold))
}

// emit is a later parse of the same conversation.
func (g *goldenFollow) emit(s *transcript.Session, hold bool) {
	g.p.width = g.width
	g.b.WriteString(followEmit(g.p, s, hold))
	fmt.Fprintf(&g.b, "[pending %v]\n", g.p.pending)
}

// moved is the pane moving to another conversation, as loop prints it.
func (g *goldenFollow) moved(s *transcript.Session, tgt convoTarget, last int, now time.Time, hold bool) {
	g.p = newConvoPrinter(s.Provider, g.color, g.color)
	g.p.live = true
	from := convoStart(s.Turns, last)
	g.p.skip(s.Turns[:from])
	g.p.width = g.width
	g.b.WriteString("\n" + convoMovedLine(g.p, tgt, s, now, from, g.width) + "\n" + followEmit(g.p, s, hold))
}

func (g *goldenFollow) load(tgt convoTarget) *transcript.Session {
	g.t.Helper()
	s, err := loadConvo(tgt)
	if err != nil {
		g.t.Fatal(err)
	}
	return s
}

// runFollow runs seq at two widths, plain and on a terminal, into one golden. The width is
// the printer's only: the coloured header reads the terminal itself, as convoHeader does,
// and shows at its 80-column fallback in every section.
func runFollow(t *testing.T, name string, seq func(g *goldenFollow)) {
	runFollowIn(t, name, "C.UTF-8", seq)
}

// runFollowIn is runFollow with LANG set to lang.
func runFollowIn(t *testing.T, name, lang string, seq func(g *goldenFollow)) {
	t.Setenv("HOME", "/invented/home")
	var b strings.Builder
	for _, width := range []int{52, 110} {
		for _, color := range []bool{false, true} {
			fmt.Fprintf(&b, "=== width %d, colour and marks %v\n", width, color)
			g := &goldenFollow{t: t, width: width, color: color}
			b.WriteString(goldenEnvIn(t, lang, func() { seq(g) }))
			b.WriteString(g.b.String())
		}
	}
	checkGolden(t, name, b.String())
}

// The last reply is held while the file may still be writing it, then released once the
// file is quiet.
func TestGoldenFollowHoldThenRelease(t *testing.T) {
	runFollow(t, "follow-hold-release.golden", func(g *goldenFollow) {
		path := filepath.Join(g.t.TempDir(), "c0ffee05-0000-4000-8000-000000000005.jsonl")
		tgt := convoTarget{provider: "claude", path: path, account: "claude/a", pane: "%7"}
		c := &goldenClaude{cwd: "/invented/home/work/demo", id: "c0ffee05-0000-4000-8000-000000000005", at: goldenAt(6, 14, 0, 0)}
		c.typed("publish the release notes")
		c.tool("m1", "Checking the draft.")
		goldenWrite(g.t, path, c.lines...)
		s := g.load(tgt)
		g.step("prompt and a tool call")
		g.begin(s, tgt, 0, s.Unfinished)
		c.tool("m2", "Uploading the notes.\nThis takes a minute.")
		c.answer("m3", "Published. The notes are at **docs/release.md**:\n\n- what changed\n- how to upgrade")
		goldenWrite(g.t, path, c.lines...)
		s = g.load(tgt)
		g.step("the answer written, the file not quiet")
		g.emit(s, s.Unfinished)
		g.step("the file quiet")
		g.emit(g.load(tgt), false)
		c.typed("thanks")
		goldenWrite(g.t, path, c.lines...)
		s = g.load(tgt)
		g.step("a new prompt")
		g.emit(s, s.Unfinished)
	})
}

// A session read by id whose CLI stops: the open turn closes, its last answer the reply.
func TestGoldenFollowStaleFlip(t *testing.T) {
	runFollow(t, "follow-stale-flip.golden", func(g *goldenFollow) {
		id := "c0ffee06-0000-4000-8000-000000000006"
		path := filepath.Join(g.t.TempDir(), id+".jsonl")
		c := &goldenClaude{cwd: "/invented/home/work/demo", id: id, at: goldenAt(7, 16, 0, 0)}
		c.typed("rebuild the search index")
		c.tool("m1", "Rebuilding the index.")
		c.answer("m2", "The index is rebuilding in the background.")
		// The background task wakes the agent, which goes on working.
		c.record("user", `"origin":{"kind":"task-notification"},"promptSource":"system","message":{"role":"user","content":"<task-notification>done</task-notification>"}`)
		c.tool("m3", "Checking the links.")
		c.typed("is it done?")
		c.tool("m4", "Reading the log.")
		// The file ends on the answer: a running CLI may still be writing it.
		c.answer("m5", "Done: 1,204 pages, no broken links.")
		goldenWrite(g.t, path, c.lines...)
		tgt := convoTarget{provider: "claude", path: path, id: id}
		s := g.load(tgt)
		g.step("running: the last reply held")
		g.begin(s, tgt, 0, s.Unfinished)
		fmt.Fprintf(&g.b, "[pending %v]\n", g.p.pending)
		g.step("the CLI stops, nothing written: stale")
		tgt.stale = true
		g.emit(g.load(tgt), false)
		g.step("running again")
		tgt.stale = false
		s = g.load(tgt)
		g.emit(s, s.Unfinished)
	})
}

// A followed pane moves to another session, which shows from --last 1.
func TestGoldenFollowPaneMove(t *testing.T) {
	runFollow(t, "follow-pane-move.golden", goldenPaneMove)
}

// The same with LANG=C, whose marks are ASCII: the moved bar and the live mark.
func TestGoldenFollowPaneMoveASCII(t *testing.T) {
	runFollowIn(t, "follow-pane-move-ascii.golden", "C", goldenPaneMove)
}

func goldenPaneMove(g *goldenFollow) {
	dir := g.t.TempDir()
	first := filepath.Join(dir, "c0ffee07-0000-4000-8000-000000000007.jsonl")
	c := &goldenClaude{cwd: "/invented/home/work/demo", id: "c0ffee07-0000-4000-8000-000000000007", at: goldenAt(8, 11, 0, 0)}
	c.typed("tidy the imports")
	c.answer("m1", "Tidied.")
	goldenWrite(g.t, first, c.lines...)
	tgt := convoTarget{provider: "claude", path: first, account: "claude/a", pane: "%9"}
	g.step("the first session")
	g.begin(g.load(tgt), tgt, 1, false)
	second := filepath.Join(dir, "rollout-2025-09-08T11-30-00-019a0000-0000-7000-8000-0000000000c3.jsonl")
	turns := [][2]string{{"list the open issues", "Three are open."}, {"close the stale one", "Closed #41."}}
	write := func(n int) {
		goldenWrite(g.t, second, goldenCodex("019a0000-0000-7000-8000-0000000000c3", "/invented/home/work/demo", "cli", goldenAt(8, 11, 30, 0), turns[:n]...)...)
	}
	write(2)
	moved := convoTarget{provider: "codex", path: second, account: "codex/b", pane: "%9"}
	g.step("the pane moves")
	g.moved(g.load(moved), moved, 1, goldenAt(8, 11, 32, 0), false)
	turns = append(turns, [2]string{"and the duplicate", "Closed #42 as a duplicate of #40."})
	write(3)
	g.step("the new session goes on")
	g.emit(g.load(moved), false)
}

// TestGoldenResolve is what aiq convo resolves, by directory, id and pane, on temporary
// homes: a Claude session open in this process in tmux pane %5, a long Codex lease in pane
// %4 with its transcript, and a bare Codex lease no hook reported a transcript for. A fake
// tmux on PATH describes the panes.
func TestGoldenResolve(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake tmux is a shell script")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	data, claudeHome, codexHome := filepath.Join(root, "data"), filepath.Join(root, "claude"), filepath.Join(root, "codex")
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	bin := filepath.Join(root, "bin")
	pid := os.Getpid()
	goldenWrite(t, filepath.Join(bin, "tmux"), "#!/bin/sh", `case "$4" in`,
		fmt.Sprintf(`%%4) echo "%%4:%d:0:demo:codex" ;;`, pid), fmt.Sprintf(`%%5) echo "%%5:%d:0:demo:claude" ;;`, pid),
		`*) echo "can't find pane: $4" >&2; exit 1 ;;`, "esac")
	if err := os.Chmod(filepath.Join(bin, "tmux"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(filepath.Join(bin, "tmux"), "display-message", "-p", "-t", "%4", "x").Run(); err != nil {
		t.Skipf("the fake tmux cannot run, as on a noexec temporary directory: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(root, "work", "demo")
	for _, d := range []string{work, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	project := filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"))
	for _, id := range []string{goldenC1, goldenC2} {
		c := &goldenClaude{cwd: work, id: id, at: goldenAt(9, 9, 0, 0)}
		c.typed("look at " + id[:8])
		c.answer("m1", "Looked.")
		goldenWrite(t, filepath.Join(project, id+".jsonl"), c.lines...)
	}
	goldenWrite(t, filepath.Join(claudeHome, "sessions", "1.json"),
		fmt.Sprintf(`{"pid":%d,"sessionId":"%s","cwd":%q,"tmux":"demo:@1.%%5"}`, pid, goldenC2, work))
	day := filepath.Join(codexHome, "sessions", "2025", "09", "09")
	leased := filepath.Join(day, "rollout-2025-09-09T10-00-00-"+goldenX1+".jsonl")
	goldenWrite(t, leased, goldenCodex(goldenX1, work, "cli", goldenAt(9, 10, 0, 0), [2]string{"ship it", "Shipped."})...)
	goldenWrite(t, filepath.Join(day, "rollout-2025-09-09T11-00-00-"+goldenX2+".jsonl"),
		goldenCodex(goldenX2, work, "cli", goldenAt(9, 11, 0, 0), [2]string{"tag it", "Tagged."})...)

	st, err := state.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, l := range []state.Lease{
		{AccountID: "codex/b", Provider: "codex", Mode: state.ModeLong, PID: pid, Hostname: hostname(), Workspace: work, Cwd: work,
			Pane: "%4", SessionID: goldenX1, Transcript: leased, StartedAt: goldenAt(9, 9, 59, 0).Unix()},
		{AccountID: "codex/c", Provider: "codex", Mode: state.ModeInteractive, PID: pid, Hostname: hostname(), Cwd: work,
			StartedAt: goldenAt(9, 10, 59, 0).Unix()},
	} {
		if _, err := st.AddLease(l); err != nil {
			t.Fatal(err)
		}
	}

	show := func(call string, tgt convoTarget, err error) {
		if err != nil {
			fmt.Printf("%s: error: %v\n", call, err)
			return
		}
		fmt.Printf("%s: provider %s, path %s, account %q, pane %q, stale %v, id %q\n",
			call, tgt.provider, tgt.path, tgt.account, tgt.pane, tgt.stale, tgt.id)
	}
	t.Chdir(work)
	got, warnings := goldenStreamsIn(t, "C.UTF-8", func() {
		// Build after redirecting stderr so unreadable-transcript warnings retain their stream.
		r := newConvoResolver(st, defaultRoots())
		// resolveHere resolves from where aiq convo runs, as cmdConvo reads it.
		resolveHere := func(o convoOpts) (convoTarget, error) {
			dir, pane, err := convoHere()
			if err != nil {
				return convoTarget{}, err
			}
			return r.resolve(o, dir, pane)
		}
		for _, dir := range []string{work, filepath.Join(work, "sub"), filepath.Join(root, "work"), filepath.Join(root, "other")} {
			tgt, err := r.resolveDir(dir)
			show("dir "+dir, tgt, err)
		}
		for _, id := range []string{"c0ffee01", goldenC2, goldenX1, "c0ffee", "0000"} {
			tgt, err := r.resolveID(work, id)
			show("id "+id, tgt, err)
		}
		for _, pane := range []string{"%4", "%5", "%8"} {
			tgt, err := r.resolvePane(pane)
			show("pane "+pane, tgt, err)
		}
		for _, c := range []struct {
			name string
			o    convoOpts
		}{{"resolve here", convoOpts{}}, {"resolve --pane %5", convoOpts{pane: "%5"}}, {"resolve c0ffee01", convoOpts{id: "c0ffee01"}}} {
			tgt, err := resolveHere(c.o)
			show(c.name, tgt, err)
		}
		t.Setenv("TMUX_PANE", "%8")
		tgt, err := resolveHere(convoOpts{})
		show("resolve in pane %8", tgt, err)
		t.Setenv("TMUX_PANE", "%5")
		tgt, err = resolveHere(convoOpts{})
		show("resolve in pane %5", tgt, err)
		fmt.Printf("stale %s: %v\n", goldenX1, r.stale(convoTarget{provider: "codex", path: leased, id: goldenX1}))
		fmt.Printf("stale %s: %v\n", goldenX2, r.stale(convoTarget{provider: "codex", path: leased, id: goldenX2}))
		fmt.Printf("stale %s: %v\n", goldenC1, r.stale(convoTarget{provider: "claude", id: goldenC1}))

		// A transcript that cannot be read is named before the answer.
		bad := filepath.Join(project, "c0ffee03-0000-4000-8000-000000000003.jsonl")
		c := &goldenClaude{cwd: work, id: "c0ffee03-0000-4000-8000-000000000003", at: goldenAt(9, 9, 30, 0)}
		c.typed("look at c0ffee03")
		goldenWrite(t, bad, c.lines...)
		panicOn(t, bad)
		for _, id := range []string{"c0ffee01", "c0ffee03"} {
			tgt, err := r.resolveID(work, id)
			show("id "+id+", c0ffee03 unreadable", tgt, err)
		}

		// What the resolver reads of this machine by default: a Claude session and a lease
		// whose processes have ended, and a Codex transcript written two hours ago.
		gone := goldenDeadPID(t)
		goldenWrite(t, filepath.Join(claudeHome, "sessions", "2.json"),
			fmt.Sprintf(`{"pid":%d,"sessionId":"%s","cwd":%q,"tmux":"demo:@1.%%6"}`, gone, goldenC1, work))
		old := filepath.Join(day, "rollout-2025-09-09T12-00-00-019d0000-0000-7000-8000-0000000000d1.jsonl")
		goldenWrite(t, old, goldenCodex("019d0000-0000-7000-8000-0000000000d1", work, "cli", goldenAt(9, 12, 0, 0), [2]string{"old", "Old."})...)
		if err := os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AddLease(state.Lease{AccountID: "codex/d", Provider: "codex", Mode: state.ModeLong, PID: gone, Hostname: hostname(),
			Workspace: work, Cwd: work, Pane: "%9", SessionID: "019d0000-0000-7000-8000-0000000000d1", Transcript: old,
			StartedAt: goldenAt(9, 11, 59, 0).Unix()}); err != nil {
			t.Fatal(err)
		}
		fmt.Println("--- ended processes and an old transcript")
		r = newConvoResolver(st, defaultRoots())
		tgt, err = r.resolveDir(work)
		show("dir "+work, tgt, err)
		fmt.Printf("stale %s: %v\n", goldenC1, r.stale(convoTarget{provider: "claude", id: goldenC1}))
		fmt.Printf("stale 019d0000-0000-7000-8000-0000000000d1: %v\n",
			r.stale(convoTarget{provider: "codex", path: old, id: "019d0000-0000-7000-8000-0000000000d1"}))
	})
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(root, "-"), "<root as a project>")
		return strings.ReplaceAll(s, root, "<root>")
	}
	checkGolden(t, "resolve.golden", normalize(got))
	checkGolden(t, "resolve-stderr.golden", normalize(warnings))
}

// TestGoldenResolveMachine is what aiq convo resolves when the machine says no: through
// the resolver's own seams, a process that has ended, a lease on another host, a session
// in the home directory, and transcripts older and newer than staleAfter. The pids are
// invented; alive says which of them run.
func TestGoldenResolveMachine(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	data, claudeHome, codexHome := filepath.Join(root, "data"), filepath.Join(root, "claude"), filepath.Join(root, "codex")
	t.Setenv("AIQ_DATA_DIR", data)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	work, other := filepath.Join(root, "work", "demo"), filepath.Join(root, "work", "other")
	for _, d := range []string{work, other, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	const live, live2, dead = 900002, 900003, 900009
	alive := func(pid int) bool { return pid == live || pid == live2 }
	now := time.Now()

	const c4 = "c0ffee04-0000-4000-8000-000000000004"
	claudeIn := map[string]string{goldenC1: work, goldenC2: work, c4: root}
	for _, id := range []string{goldenC1, goldenC2, c4} {
		cwd := claudeIn[id]
		c := &goldenClaude{cwd: cwd, id: id, at: goldenAt(10, 9, 0, 0)}
		c.typed("look at " + id[:8])
		c.answer("m1", "Looked.")
		project := filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-"))
		goldenWrite(t, filepath.Join(project, id+".jsonl"), c.lines...)
	}
	for name, c := range map[string]claudeLive{
		"1.json": {PID: live, SessionID: goldenC2, Cwd: work, Tmux: "demo:@1.%5"},
		"2.json": {PID: dead, SessionID: goldenC1, Cwd: work, Tmux: "demo:@1.%6"},
		"3.json": {PID: live2, SessionID: c4, Cwd: root},
	} {
		goldenWrite(t, filepath.Join(claudeHome, "sessions", name),
			fmt.Sprintf(`{"pid":%d,"sessionId":%q,"cwd":%q,"tmux":%q}`, c.PID, c.SessionID, c.Cwd, c.Tmux))
	}
	// Codex transcripts, each written age before now.
	codex := map[string]string{}
	for i, x := range []struct {
		name, cwd string
		age       time.Duration
	}{
		{"x1", work, 10 * time.Minute}, {"x2", work, 2 * time.Hour}, {"x3", work, 2 * time.Hour},
		{"x4", work, 2 * time.Hour}, {"x5", work, 10 * time.Minute},
		{"x6", other, 5 * time.Minute}, {"x7", other, 20 * time.Minute}, {"x8", work, 20 * time.Minute},
	} {
		id := fmt.Sprintf("019c0000-0000-7000-8000-0000000000%02d", i+1)
		path := filepath.Join(codexHome, "sessions", "2025", "09", "10", "rollout-2025-09-10T10-00-00-"+id+".jsonl")
		goldenWrite(t, path, goldenCodex(id, x.cwd, "cli", goldenAt(10, 10, i, 0), [2]string{"task " + x.name, "Done " + x.name + "."})...)
		if err := os.Chtimes(path, now.Add(-x.age), now.Add(-x.age)); err != nil {
			t.Fatal(err)
		}
		codex[x.name] = path
	}
	idOf := func(name string) string { return sessionIDOf(codex[name]) }

	st, err := state.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	long := func(account string, pid int, host, cwd, pane, x string) state.Lease {
		return state.Lease{AccountID: account, Provider: "codex", Mode: state.ModeLong, PID: pid, Hostname: host, Workspace: cwd, Cwd: cwd,
			Pane: pane, SessionID: idOf(x), Transcript: codex[x], StartedAt: now.Add(-3 * time.Hour).Unix()}
	}
	for _, l := range []state.Lease{
		long("codex/b", live, hostname(), work, "%4", "x1"),
		long("codex/c", live, "invented-other-host", work, "%7", "x2"),
		long("codex/d", dead, hostname(), work, "%8", "x3"),
		// Two that hold x6 but do not count: on another host, and ended.
		long("codex/f", live, "invented-other-host", other, "", "x6"),
		long("codex/g", dead, hostname(), other, "", "x6"),
		// A bare lease: no hook reported its transcript.
		{AccountID: "codex/e", Provider: "codex", Mode: state.ModeInteractive, PID: live, Hostname: hostname(), Cwd: other,
			StartedAt: now.Add(-time.Hour).Unix()},
	} {
		if _, err := st.AddLease(l); err != nil {
			t.Fatal(err)
		}
	}

	show := func(call string, tgt convoTarget, err error) {
		if err != nil {
			fmt.Printf("%s: error: %v\n", call, err)
			return
		}
		fmt.Printf("%s: provider %s, path %s, account %q, pane %q, stale %v, id %q\n",
			call, tgt.provider, tgt.path, tgt.account, tgt.pane, tgt.stale, tgt.id)
	}
	got := goldenEnv(t, func() {
		r := newConvoResolver(st, defaultRoots())
		r.alive = alive
		r.claude = func() []claudeLive { return liveClaude(paths.RealClaudeHome(), paths.ClaudeHomesDir(), alive) }
		// Each pane's process is its lease's, so only the host and alive say no.
		r.paneInfo = func(pane string) (tmux.Pane, error) {
			pid := live
			if pane == "%8" {
				pid = dead
			}
			return tmux.Pane{ID: pane, PID: pid, StartCommand: "codex"}, nil
		}
		r.parents = func() map[int]int { return nil }
		// The resolver's clock is a quarter of an hour ahead of the files' one: x8, written
		// twenty minutes before the files' now, is stale by it and by no other.
		r.now = func() time.Time { return now.Add(15 * time.Minute) }
		for _, dir := range []string{work, other, root, filepath.Join(root, "elsewhere")} {
			tgt, err := r.resolveDir(dir)
			show("dir "+dir, tgt, err)
		}
		for _, pane := range []string{"%4", "%7", "%8"} {
			tgt, err := r.resolvePane(pane)
			show("pane "+pane, tgt, err)
		}
		for _, id := range []string{goldenC1, goldenC2} {
			tgt, err := r.resolveID(work, id)
			show("id "+id, tgt, err)
		}
		for _, x := range []string{"x1", "x2", "x3", "x4", "x5", "x8"} {
			fmt.Printf("stale %s: %v\n", x, r.stale(convoTarget{provider: "codex", path: codex[x], id: idOf(x)}))
		}
	})
	out := strings.ReplaceAll(got, regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(root, "-"), "<root as a project>")
	checkGolden(t, "resolve-machine.golden", strings.ReplaceAll(out, root, "<root>"))
}
