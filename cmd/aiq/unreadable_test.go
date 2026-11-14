package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

// listHome sets up temporary homes and a working directory holding sessions
// of every provider, a Codex worker among them, and returns that directory
// and the path of the Claude session s2.
func listHome(t *testing.T) (work, s2 string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	claudeHome, codexHome := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("PWD", work)
	write := func(path string, lines ...string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	project := filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"))
	claude := func(id, at, prompt string) {
		c := `"cwd":"` + work + `","sessionId":"` + id + `"`
		write(filepath.Join(project, id+".jsonl"),
			`{"type":"user",`+c+`,"timestamp":"2026-11-0`+at+`T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"`+prompt+`"}}`,
			`{"type":"assistant",`+c+`,"timestamp":"2026-11-0`+at+`T10:00:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
			`{"type":"assistant",`+c+`,"timestamp":"2026-11-0`+at+`T10:00:09Z","message":{"id":"m2","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Done."}]}}`)
	}
	claude("5e551011-0000-4000-8000-000000000001", "1", "fix the bug")
	claude("5e551011-0000-4000-8000-000000000002", "2", "write the tests")
	write(filepath.Join(project, "5e551011-0000-4000-8000-000000000003.jsonl"), `{"type":"permission-mode","permissionMode":"default"}`)
	codex := func(file, id, at, source string) {
		write(filepath.Join(codexHome, "sessions", "2026", "11", "0"+at, file),
			`{"timestamp":"2026-11-0`+at+`T11:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"`+work+`","source":"`+source+`"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:01Z","type":"turn_context","payload":{"model":"gpt-6","approval_policy":"never","sandbox_policy":{"type":"danger-full-access"}}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"ship it","turn_id":"a"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec"}}`,
			`{"timestamp":"2026-11-0`+at+`T11:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"a","last_agent_message":"Shipped."}}`)
	}
	codex("rollout-2026-11-03T11-00-00-a.jsonl", "019a0000-0000-7000-8000-00000000000a", "3", "cli")
	codex("rollout-2026-11-04T11-00-00-b.jsonl", "019a0000-0000-7000-8000-00000000000b", "4", "exec")
	write(filepath.Join(codexHome, "session_index.jsonl"), `{"id":"019a0000-0000-7000-8000-00000000000a","thread_name":"release"}`)
	appData := filepath.Join(home, ".gemini", "antigravity-cli")
	write(transcript.AgyTranscript(appData, "a1"),
		`{"type":"USER_INPUT","created_at":"2026-11-05T12:00:00Z","content":"<USER_REQUEST>\nlook around\n</USER_REQUEST>"}`,
		`{"type":"PLANNER_RESPONSE","created_at":"2026-11-05T12:00:10Z","content":"Looked.","tool_calls":[{}]}`)
	write(filepath.Join(appData, "history.jsonl"), `{"display":"look around","timestamp":1,"workspace":"`+work+`","conversationId":"a1"}`)
	return work, filepath.Join(project, "5e551011-0000-4000-8000-000000000002.jsonl")
}

// panicOn makes the parse of path panic for the rest of the test.
func panicOn(t *testing.T, path string) {
	restore := transcript.SetParserForTest(func(p string) (*transcript.Session, error) {
		switch {
		case p == path:
			panic("secret transcript text")
		case strings.HasPrefix(filepath.Base(p), "rollout-"):
			return transcript.ParseCodex(p)
		case filepath.Base(p) == "transcript.jsonl":
			return transcript.ParseAgy(p)
		}
		return transcript.ParseClaude(p)
	})
	t.Cleanup(restore)
}

// sessionOfRow is what a list row says of its session, as a Session: the
// resume list prints nothing a row does not carry.
func sessionOfRow(row transcript.Summary) transcript.Session {
	s := transcript.Session{Provider: row.Provider, ID: row.ID, Path: row.Path, Cwd: row.Cwd, Title: row.Label,
		Model: row.Model, Branch: row.Branch, Started: row.Started, Updated: row.Updated, Worker: row.Worker, Bypass: row.Bypass,
		Turns: make([]transcript.Turn, row.Turns)}
	s.Turns[0].Tools = row.Tools
	return s
}

// aiq resume --print lists the same sessions, byte for byte, whether they
// come from List or from ListWith over the direct walk.
func TestResumePrintSameFromListWith(t *testing.T) {
	work, _ := listHome(t)
	list, unreadable, err := transcript.List(defaultRoots(), work)
	if err != nil || len(unreadable) != 0 {
		t.Fatal(err, unreadable)
	}
	res, err := transcript.ListWith(context.Background(), transcript.DirectWalk(defaultRoots()), work, transcript.ParseSummary)
	if err != nil || res.Partial {
		t.Fatal(err, res.Partial)
	}
	var rows []transcript.Session
	for _, row := range res.Rows {
		if !row.Empty {
			rows = append(rows, sessionOfRow(row))
		}
	}
	if len(list) != 5 {
		t.Fatalf("%d sessions listed, want 5", len(list))
	}
	for _, all := range []bool{false, true} {
		fromList, fromRows := list, rows
		if !all {
			fromList, fromRows = interactiveOnly(list), interactiveOnly(rows)
		}
		got, want := renderSessionsPlain(work, fromRows, nil, nil), renderSessionsPlain(work, fromList, nil, nil)
		if got != want {
			t.Errorf("--all %v:\nListWith:\n%s\nList:\n%s", all, got, want)
		}
		for _, w := range []string{"release", "look around", "write the tests", "fix the bug"} {
			if !strings.Contains(want, w) {
				t.Errorf("--all %v: %q missing:\n%s", all, w, want)
			}
		}
	}
}

// A transcript whose parse panics: aiq resume lists the rest and names the
// file on stderr before anything else, and the panic's text goes nowhere.
func TestResumeNamesAnUnreadableTranscriptFirst(t *testing.T) {
	_, bad := listHome(t)
	panicOn(t, bad)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	runErr := cmdResume([]string{"--print"})
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	lines := strings.Split(string(out), "\n")
	if lines[0] != fmt.Sprintf("aiq: could not read transcript %q", bad) || strings.Count(string(out), "could not read") != 1 {
		t.Fatalf("got:\n%s", out)
	}
	if !strings.HasPrefix(lines[1], "ID ") || !strings.Contains(string(out), "fix the bug") || strings.Contains(string(out), "write the tests") {
		t.Errorf("got:\n%s", out)
	}
	if strings.Contains(string(out), "secret") {
		t.Errorf("the panic's text was printed:\n%s", out)
	}
}

// The menu walks while its screen is up: the walk prints nothing, the status
// row counts what could not be read, and the paths print once the screen is
// gone, from what the walk has returned by then.
func TestMenuNamesUnreadableTranscriptsAfterItsScreen(t *testing.T) {
	work, bad := listHome(t)
	panicOn(t, bad)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	s := loadMenuSessions(work)
	os.Stdout, os.Stderr = stdout, stderr
	w.Close()
	if out, _ := io.ReadAll(r); len(out) != 0 {
		t.Errorf("the walk printed %q", out)
	}
	if len(s.unreadable) != 1 || s.unreadable[0] != bad || s.count != 3 {
		t.Fatalf("%+v", s)
	}

	env := testMenuEnv()
	env.sessions = s
	m := &menu{env: env, c: defaultChoice(), w: 100, h: 30}
	var shown strings.Builder
	for _, l := range m.render() {
		for _, sg := range l {
			shown.WriteString(sg.text)
		}
		shown.WriteString("\n")
	}
	if !strings.Contains(shown.String(), " 1 transcript could not be read\n") || strings.Contains(shown.String(), bad) {
		t.Errorf("status:\n%s", shown.String())
	}

	want := fmt.Sprintf("aiq: could not read transcript %q\n", bad)
	var b bytes.Buffer
	m.reportUnreadable(&b, nil)
	if b.String() != want {
		t.Errorf("after the screen: %q, want %q", b.String(), want)
	}
	// The walk returned but the screen never took its result.
	m.env.sessions = nil
	ch := make(chan *menuSessions, 1)
	ch <- s
	b.Reset()
	m.reportUnreadable(&b, ch)
	if b.String() != want {
		t.Errorf("walk done, not shown: %q, want %q", b.String(), want)
	}
	// The walk has not returned: nothing to say, and no wait.
	b.Reset()
	m.reportUnreadable(&b, make(chan *menuSessions, 1))
	if b.Len() != 0 {
		t.Errorf("walk still running: %q", b.String())
	}
}

// Past ten unreadable transcripts the rest are counted, and a path prints
// quoted, so one cannot drive the terminal.
func TestReportUnreadableNamesTenThenCounts(t *testing.T) {
	var paths []string
	for i := range 12 {
		paths = append(paths, fmt.Sprintf("/t/%02d.jsonl", i))
	}
	paths[0] = "/t/a\x1b]0;x\x07\nb.jsonl"
	var b bytes.Buffer
	reportUnreadable(&b, paths)
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 11 || lines[10] != "aiq: …and 2 more could not be read" {
		t.Fatalf("got:\n%s", b.String())
	}
	if lines[0] != `aiq: could not read transcript "/t/a\x1b]0;x\a\nb.jsonl"` || lines[9] != `aiq: could not read transcript "/t/09.jsonl"` {
		t.Errorf("got:\n%s", b.String())
	}
	b.Reset()
	reportUnreadable(&b, paths[2:])
	if n := strings.Count(b.String(), "\n"); n != 10 || strings.Contains(b.String(), "more") {
		t.Errorf("ten: got:\n%s", b.String())
	}
}

// aiq long … resume and aiq convo <prefix> name an unreadable transcript the
// same way, before anything else.
func TestLongAndConvoNameAnUnreadableTranscript(t *testing.T) {
	work, bad := listHome(t)
	panicOn(t, bad)
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	want := fmt.Sprintf("aiq: could not read transcript %q\n", bad)
	for name, run := range map[string]func() error{
		"long resume": func() error {
			_, _, err := a.pickResume(resumeRequest{o: resumeOpts{id: "5e551011-0000-4000-8000-000000000001"}})
			return err
		},
		"convo": func() error {
			_, err := newConvoResolver(nil, defaultRoots()).resolveID(work, "5e551011-0000-4000-8000-000000000001")
			return err
		},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := os.Stderr
		os.Stderr = w
		runErr := run()
		os.Stderr = stderr
		w.Close()
		out, _ := io.ReadAll(r)
		if runErr != nil || string(out) != want {
			t.Errorf("%s: %v, printed %q, want %q", name, runErr, out, want)
		}
	}
}

// An id that matches no readable session says the transcripts named above
// could not be read, and gives no --all hint, which would not list them.
func TestNoReadableSessionGivesNoAllHint(t *testing.T) {
	_, bad := listHome(t)
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	line := fmt.Sprintf("aiq: could not read transcript %q\n", bad)
	for _, unreadable := range []bool{false, true} {
		if unreadable {
			panicOn(t, bad)
		}
		for _, full := range []bool{false, true} {
			id := "nonexistent-prefix"
			if full {
				id = "5e551011-0000-4000-8000-0000000000ee"
				if unreadable {
					id = "5e551011-0000-4000-8000-000000000002"
				}
			}
			want := fmt.Sprintf("no session %q in this directory (aiq resume --print --all lists them)", id)
			wantOut := ""
			if unreadable {
				want = fmt.Sprintf("no readable session %q in this directory; the transcripts named above could not be read", id)
				wantOut = line
			}
			var cause error
			if full {
				want = fmt.Sprintf("no readable session %q under configured roots", id)
				cause = transcript.ErrNoSuchSession
				if unreadable {
					want = fmt.Sprintf("could not read transcript %q", bad)
					cause = transcript.ErrUnreadable
					wantOut = ""
				}
			}
			for name, run := range map[string]func() error{
				"resume --print": func() error { return cmdResume([]string{"--print", id}) },
				"long resume":    func() error { _, _, err := a.pickResume(resumeRequest{o: resumeOpts{id: id}}); return err },
				"convo":          func() error { return cmdConvo([]string{id}) },
			} {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				stdout, stderr := os.Stdout, os.Stderr
				os.Stdout, os.Stderr = w, w
				runErr := run()
				os.Stdout, os.Stderr = stdout, stderr
				w.Close()
				out, _ := io.ReadAll(r)
				r.Close()
				if runErr == nil || runErr.Error() != want {
					t.Errorf("%s full=%v unreadable=%v: error %v, want %q", name, full, unreadable, runErr, want)
				}
				if full && !errors.Is(runErr, cause) {
					t.Errorf("%s lost global lookup/load cause: %v", name, runErr)
				}
				if full && unreadable {
					var load *convoLoadError
					if !errors.As(runErr, &load) {
						t.Errorf("%s unreadable full UUID did not return selected-load error", name)
					}
				}
				if string(out) != wantOut {
					t.Errorf("%s full=%v unreadable=%v: printed %q, want %q", name, full, unreadable, out, wantOut)
				}
			}
		}
	}
}

// Every way out of the menu names what the list could not read once the
// screen is gone, before the command it runs; a list not yet made says
// nothing.
func TestMenuNamesUnreadableOnEveryExit(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	line := fmt.Sprintf("aiq: could not read transcript %q\n", "/t/s2.jsonl")
	quit, _ := (&menu{env: testMenuEnv(), c: defaultChoice(), focus: "session"}).handle(keypress{r: 'q'})
	interrupt, done := (&menu{env: testMenuEnv(), c: defaultChoice(), focus: "session"}).handle(keypress{code: keyQuit})
	if !done || interrupt != nil {
		t.Fatalf("interrupt: %v %v", interrupt, done)
	}
	for _, c := range []struct {
		name string
		args []string
		err  error
		then string // what follows the line on stdout
	}{
		{"quit", quit, nil, ""},
		{"interrupt", interrupt, nil, ""},
		{"error", nil, errors.New("no terminal"), ""},
		{"help", []string{"help"}, nil, ""},
		{"start", []string{"run", "auto"}, nil, "$ aiq run auto\n"},
	} {
		for _, returned := range []bool{true, false} {
			m := &menu{env: testMenuEnv(), c: defaultChoice()}
			ch := make(chan *menuSessions, 1)
			if returned {
				ch <- &menuSessions{unreadable: []string{"/t/s2.jsonl"}}
			}
			var out bytes.Buffer // stdout and stderr, in the order written
			args, err := m.finish(&out, &out, ch, c.args, c.err)
			if err != c.err || (c.name == "start") != (args != nil) {
				t.Errorf("%s: %v %v", c.name, args, err)
			}
			got, rest := out.String(), out.String()
			if c.name == "help" {
				// Help's usage comes first, so the line is not lost above it.
				help := fmt.Sprintf(usage, version)
				if !strings.HasPrefix(got, help) || returned != strings.HasSuffix(got, line) ||
					!returned && strings.Contains(got, "could not read") || returned && got != help+line {
					t.Errorf("help, list made %v: got %.80q … %q", returned, got, got[max(0, len(got)-80):])
				}
				continue
			}
			if returned {
				if !strings.HasPrefix(got, line) {
					t.Errorf("%s: got %q", c.name, got)
					continue
				}
				rest = strings.TrimPrefix(got, line)
			} else if strings.Contains(got, "could not read") {
				t.Errorf("%s, list not made: got %q", c.name, got)
			}
			if c.then == "" && rest != "" || !strings.Contains(rest, c.then) {
				t.Errorf("%s: after the line, %.80q", c.name, rest)
			}
		}
	}
}

// A record too long to read shows as one dim line where it came, once, also
// when the conversation is followed.
func TestConvoShowsASkippedRecord(t *testing.T) {
	s := &transcript.Session{Skipped: 1, Turns: []transcript.Turn{{Prompt: "fix it", Reply: "Fixed.", Skipped: 1}}}
	p := newConvoPrinter("claude", false, false)
	got := pieces(p)
	out := p.emit(s)
	line := "  · a record over 32 MB was skipped\n"
	if strings.Count(out, line) != 2 || !strings.HasPrefix(out, line) || strings.Index(out, "Fixed.") > strings.LastIndex(out, line) {
		t.Errorf("got:\n%s", out)
	}
	if k := turnKey(0, s.Turns[0]); len(got[k]) != 3 || got[k][2] != "skipped\x00a record over 32 MB was skipped" {
		t.Errorf("pieces %q", got)
	}
	if again := p.emit(s); again != "" {
		t.Errorf("printed again: %q", again)
	}
	s.Turns[0].Skipped = 2
	if more := p.emit(s); more != line {
		t.Errorf("a second record: %q", more)
	}
	plain := renderTurnsPlain(*s, nil)
	if strings.Count(plain, "(a record over 32 MB was skipped)\n") != 3 {
		t.Errorf("resume --print:\n%s", plain)
	}
}

// A turn whose prompt was a record too long to read shows the skipped line
// once, in the prompt's place under the turn's header, in aiq convo and in
// aiq resume --print.
func TestASkippedPromptShowsInItsPlace(t *testing.T) {
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	s := &transcript.Session{Turns: []transcript.Turn{{Source: transcript.Human, PromptSkipped: true, Skipped: 1, Reply: "Fixed.", Started: at, Ended: at}}}
	line := "  · a record over 32 MB was skipped\n"
	out := newConvoPrinter("claude", false, false).emit(s)
	if strings.Count(out, "a record over 32 MB was skipped") != 1 || !strings.Contains(out, "▌ you · ") ||
		!strings.Contains(out, "\n"+line) || strings.Index(out, line) > strings.Index(out, "Fixed.") {
		t.Errorf("convo:\n%s", out)
	}
	if i := strings.Index(out, line); i < 0 || !strings.HasPrefix(out[strings.LastIndex(out[:i-1], "\n")+1:], "▌ you · ") {
		t.Errorf("the line is not under the prompt's header:\n%s", out)
	}
	// The session's header line carries its label; the turn starts after it.
	plain := renderTurnsPlain(*s, nil)
	plain = plain[strings.Index(plain, "## Turn"):]
	if strings.Count(plain, "skipped") != 1 || !strings.Contains(plain, "· 0 tools\n\n(a record over 32 MB was skipped)\n\nFixed.\n") {
		t.Errorf("resume --print:\n%s", plain)
	}
	// A second record skipped in the turn has a line of its own.
	s.Turns[0].Skipped = 2
	if out := newConvoPrinter("claude", false, false).emit(s); strings.Count(out, "a record over 32 MB was skipped") != 2 {
		t.Errorf("convo, two records:\n%s", out)
	}
	if plain := renderTurnsPlain(*s, nil); strings.Count(plain[strings.Index(plain, "## Turn"):], "skipped") != 2 {
		t.Errorf("resume --print, two records:\n%s", plain)
	}
}

// aiq resume's browser shows the skipped line in a skipped prompt's place too,
// on the turns screen and on the turn's own screen, not a blank prompt.
func TestBrowserShowsASkippedPrompt(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	s := transcript.Session{Provider: "claude", ID: "abc", Updated: at, Turns: []transcript.Turn{
		{Source: transcript.Human, PromptSkipped: true, Skipped: 1, Reply: "Fixed.", Started: at, Ended: at}}}
	b := newBrowser("/w", []transcript.Session{s}, nil, false)
	b.w, b.h = 120, 30
	note := "(a record over 32 MB was skipped)"
	show := func(rows []line) string {
		var out strings.Builder
		for _, l := range rows {
			out.WriteString(l.text + "\n")
		}
		return out.String()
	}
	// The turns screen: the turn's second line is its prompt.
	b.view = viewTurns
	rows := b.render()
	i := slices.IndexFunc(rows, func(l line) bool { return strings.HasPrefix(l.text, "     › ") })
	if i < 0 || rows[i].text != "     › "+note {
		t.Errorf("turns screen:\n%s", show(rows))
	}
	// The turn's screen: the prompt comes under YOU.
	b.view = viewTurn
	rows = b.render()
	i = slices.IndexFunc(rows, func(l line) bool { return l.text == " YOU" })
	if i < 0 || i+1 >= len(rows) || rows[i+1].text != "   "+note {
		t.Errorf("turn screen:\n%s", show(rows))
	}
}

// A turn whose answer or tool result was over the cap says so in the browser,
// in place of "(no reply)" when nothing else came back, and the record that is
// the prompt is not counted again.
func TestBrowserShowsASkippedReply(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	note := "(a record over 32 MB was skipped)"
	for _, c := range []struct {
		name  string
		turn  transcript.Turn
		list  string   // the turns screen's reply line
		reply []string // the turn screen's lines under the provider
		notes int      // the notes on the turn screen
	}{
		{"no reply", transcript.Turn{Prompt: "Fix it.", Skipped: 1},
			"     ‹ " + note, []string{"   " + note}, 1},
		{"a reply", transcript.Turn{Prompt: "Fix it.", Reply: "Fixed.", Skipped: 1},
			"     ‹ Fixed.", []string{"   Fixed.", "   " + note}, 1},
		{"a skipped prompt", transcript.Turn{PromptSkipped: true, Skipped: 2},
			"     ‹ " + note, []string{"   " + note}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			turn := c.turn
			turn.Source, turn.Started, turn.Ended = transcript.Human, at, at
			s := transcript.Session{Provider: "claude", ID: "abc", Updated: at, Turns: []transcript.Turn{turn}}
			b := newBrowser("/w", []transcript.Session{s}, nil, false)
			b.w, b.h = 120, 30
			show := func(rows []line) string {
				var out strings.Builder
				for _, l := range rows {
					out.WriteString(l.text + "\n")
				}
				return out.String()
			}
			b.view = viewTurns
			rows := b.render()
			i := slices.IndexFunc(rows, func(l line) bool { return strings.HasPrefix(l.text, "     ‹ ") })
			if i < 0 || rows[i].text != c.list || strings.Contains(show(rows), "(no reply)") {
				t.Errorf("turns screen:\n%s", show(rows))
			}
			b.view = viewTurn
			rows = b.render()
			i = slices.IndexFunc(rows, func(l line) bool { return l.text == " CLAUDE" })
			var under []string
			for _, l := range rows[i+1:] {
				if len(under) < len(c.reply) {
					under = append(under, l.text)
				}
			}
			if i < 0 || !slices.Equal(under, c.reply) ||
				strings.Count(show(rows), note) != c.notes || strings.Contains(show(rows), "(no reply)") {
				t.Errorf("turn screen:\n%s", show(rows))
			}
		})
	}
}

// A session whose only prompt is over the cap is listed by aiq resume --print,
// under the skipped note, and aiq convo's printer shows the line once.
func TestASessionOfOneSkippedPromptIsListed(t *testing.T) {
	work, _ := listHome(t)
	old := transcript.MaxLine
	transcript.MaxLine = 1 << 20
	t.Cleanup(func() { transcript.MaxLine = old })
	id := "5e551011-0000-4000-8000-0000000000ff"
	c := `"cwd":"` + work + `","sessionId":"` + id + `"`
	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"), id+".jsonl")
	body := `{"type":"user",` + c + `,"timestamp":"2026-11-08T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + strings.Repeat("z", 2<<20) + `"}}` + "\n" +
		`{"type":"assistant",` + c + `,"timestamp":"2026-11-08T10:00:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"The dump looks fine."}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	runErr := cmdResume([]string{"--print"})
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !strings.Contains(string(out), "5e551011  ") || !strings.Contains(string(out), "a record over 1 MB was skipped") {
		t.Errorf("resume --print:\n%s", out)
	}
	s, err := transcript.ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	if shown := newConvoPrinter("claude", false, false).emit(s); strings.Count(shown, "a record over 1 MB was skipped") != 1 || !strings.Contains(shown, "The dump looks fine.") {
		t.Errorf("convo:\n%s", shown)
	}
}

// A session whose only records were too long to read is listed with no turns;
// the browser shows it and does not open a turn it does not have.
func TestBrowserOpensNoTurnOfASessionWithNone(t *testing.T) {
	s := transcript.Session{Provider: "claude", ID: "abc", Skipped: 1, Updated: time.Now()}
	b := newBrowser("/w", []transcript.Session{s}, nil, false)
	b.w, b.h = 120, 30
	b.handle(keypress{code: keyEnter})
	b.render()
	b.handle(keypress{code: keyEnter})
	rows := b.render()
	if b.view != viewTurns {
		t.Errorf("view %v, want the turns screen", b.view)
	}
	if !slices.ContainsFunc(rows, func(l line) bool { return l.text == "   (no turns)" }) {
		t.Errorf("no '(no turns)' row")
	}
	b.handle(keypress{code: keyRight})
	b.render()
	if b.view != viewTurns {
		t.Errorf("right moved to view %v", b.view)
	}
}

// A turn whose prompt was too long to read, with nothing timed after it yet,
// shows no time rather than the year 0, in each view.
func TestATimelessTurnShowsNoTime(t *testing.T) {
	old := transcript.MaxLine
	transcript.MaxLine = 100 << 10
	t.Cleanup(func() { transcript.MaxLine = old })
	dir := t.TempDir()
	path := filepath.Join(dir, "11111111-2222-3333-4444-555555555555.jsonl")
	c := `"isSidechain":false,"cwd":"/w","sessionId":"11111111-2222-3333-4444-555555555555"`
	// Claude Code writes the timestamp after the message.
	body := `{` + c + `,"type":"user","message":{"role":"user","content":"first ask"},"uuid":"u1","timestamp":"2026-11-02T10:00:00Z"}` + "\n" +
		`{` + c + `,"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"First done."}]},"uuid":"a1","timestamp":"2026-11-02T10:00:05Z"}` + "\n" +
		`{` + c + `,"type":"user","message":{"role":"user","content":"` + strings.Repeat("z", 200<<10) + `"},"uuid":"u2","timestamp":"2026-11-02T10:01:00Z"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := transcript.ParseClaude(path)
	if err != nil || s == nil || len(s.Turns) != 2 || !s.Turns[1].PromptSkipped || !s.Turns[1].Started.IsZero() {
		t.Fatalf("parse: %+v %v", s, err)
	}
	bad := []string{"0000-", "0001-", "Dec 31", "Jan 1 00:00"}
	check := func(view, out string) {
		t.Helper()
		for _, b := range bad {
			if strings.Contains(out, b) {
				t.Errorf("%s shows %q:\n%s", view, b, out)
			}
		}
	}
	convo := newConvoPrinter("claude", false, false).emit(s)
	check("convo", convo)
	if !strings.Contains(convo, "▌ you\n") {
		t.Errorf("convo's header for the skipped prompt:\n%s", convo)
	}
	plain := renderTurnsPlain(*s, nil)
	check("resume --print", plain)
	if !strings.Contains(plain, "## Turn 2 · -- · ") {
		t.Errorf("resume --print:\n%s", plain)
	}
	b := newBrowser("/w", []transcript.Session{*s}, nil, false)
	b.w, b.h = 120, 30
	b.view, b.tsel = viewTurns, 1
	var out strings.Builder
	for _, l := range b.render() {
		out.WriteString(l.text + "\n")
	}
	b.view = viewTurn
	for _, l := range b.render() {
		out.WriteString(l.text + "\n")
	}
	check("browser", out.String())
	if strings.Count(out.String(), " -- · ") != 2 {
		t.Errorf("browser:\n%s", out.String())
	}

	// A session whose only record is that prompt has no start either; the
	// browser's session header says so.
	only := filepath.Join(dir, "66666666-7777-8888-9999-000000000000.jsonl")
	if err := os.WriteFile(only, []byte(`{`+c+`,"type":"user","message":{"role":"user","content":"`+strings.Repeat("z", 200<<10)+`"},"uuid":"u3","timestamp":"2026-11-02T10:01:00Z"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = transcript.ParseClaude(only)
	if err != nil || s == nil || !s.Started.IsZero() {
		t.Fatalf("parse: %+v %v", s, err)
	}
	s.Updated = time.Now()
	b = newBrowser("/w", []transcript.Session{*s}, nil, false)
	b.w, b.h = 120, 30
	b.view = viewTurns
	out.Reset()
	for _, l := range b.render() {
		out.WriteString(l.text + "\n")
	}
	check("browser header", out.String())
	if !strings.Contains(out.String(), " -- → ") {
		t.Errorf("browser header:\n%s", out.String())
	}
}
