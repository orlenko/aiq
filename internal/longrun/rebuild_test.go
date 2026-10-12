package longrun

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// claudeTranscript writes a Claude transcript that stops mid-turn, its
// last command still running.
func claudeTranscript(t *testing.T) string {
	t.Helper()
	min := 0
	ts := func() string { min++; return "2026-09-15T10:" + strconv.Itoa(10+min) + ":00Z" }
	rec := func(body string) string {
		return `{"isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":"` + ts() + `",` + body + `}`
	}
	q := strconv.Quote
	lines := []string{
		rec(`"type":"user","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":` + q("Move the billing jobs onto the new queue.") + `}`),
		rec(`"type":"assistant","message":{"id":"m1","stop_reason":"tool_use","role":"assistant","content":[{"type":"text","text":"Reading the job runner."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./jobs"}}]}`),
		rec(`"type":"user","promptSource":"system","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":"FAIL jobs 0.2s"}]}`),
		rec(`"type":"attachment","attachment":{"type":"queued_command","prompt":"keep the old queue as a fallback","commandMode":"prompt","origin":{"kind":"human"}}`),
		rec(`"type":"assistant","message":{"id":"m2","stop_reason":"tool_use","role":"assistant","content":[{"type":"text","text":"Keeping the old queue. Running the migration."},{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"make migrate"}}]}`),
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRebuildNote(t *testing.T) {
	l := state.Lease{ID: 7, Provider: "claude", AccountID: "claude/a", SessionID: "s", Transcript: claudeTranscript(t), Workspace: t.TempDir()}
	note, err := RebuildNote(l, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		rebuiltHeading + "\n",
		"## The task, as first typed",
		"Move the billing jobs onto the new queue.",
		"- Said on the way: Reading the job runner.",
		"- The person, while it worked: keep the old queue as a fallback",
		"- Said on the way: Keeping the old queue. Running the migration.",
		"The session stopped mid-turn, after 2 tool calls.",
		"```\nBash: make migrate\n```\n\nNo result recorded",
	} {
		if !strings.Contains(note, w) {
			t.Errorf("lacks %q:\n%s", w, note)
		}
	}
	if strings.Contains(note, "## The working tree") {
		t.Errorf("a git summary outside a repo:\n%s", note)
	}
	if _, err := RebuildNote(state.Lease{Provider: "claude"}, time.Now()); err == nil {
		t.Error("rebuilt a note with no transcript")
	}
}

// A note the session wrote is offered as is; one older than the session is
// an earlier takeover's and is archived; with none, a fresh successor gets
// a rebuilt note, and a resumed one the plain prompt.
func TestPrepareNote(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	s := &Supervisor{Logf: t.Logf}
	now := time.Now()
	l := state.Lease{ID: 7, Provider: "claude", AccountID: "claude/a", Transcript: claudeTranscript(t), Workspace: "/w", StartedAt: now.Add(-time.Hour).Unix()}
	note := HandoffPath(l.Workspace)
	os.MkdirAll(filepath.Dir(note), 0o700)

	os.WriteFile(note, []byte("# mine"), 0o600)
	s.prepareNote(l, false, now)
	if b, _ := os.ReadFile(note); string(b) != "# mine" {
		t.Fatalf("the session's own note was replaced: %q", b)
	}
	if p := TakeoverPrompt(l.Workspace, false, "claude"); !strings.Contains(p, "Read the handoff note at "+note) {
		t.Errorf("own note: %s", p)
	}

	old := now.Add(-2 * time.Hour)
	os.Chtimes(note, old, old)
	s.prepareNote(l, true, now)
	if _, err := os.Stat(note); err == nil {
		t.Fatal("a stale note stayed in place for a resumed successor")
	}
	if p := TakeoverPrompt(l.Workspace, true, "claude"); !strings.Contains(p, "No handoff note was written") || strings.Contains(p, "rebuilt") {
		t.Errorf("resumed: %s", p)
	}
	if archived, _ := filepath.Glob(strings.TrimSuffix(note, ".md") + ".*.md"); len(archived) != 1 {
		t.Errorf("archived %v", archived)
	}

	s.prepareNote(l, false, now)
	if !rebuilt(note) {
		t.Fatal("no rebuilt note for a fresh successor")
	}
	p := TakeoverPrompt(l.Workspace, false, "claude")
	if !strings.Contains(p, "aiq rebuilt one from its transcript: read "+note) || strings.Contains(p, "Read the handoff note at") {
		t.Errorf("rebuilt: %s", p)
	}
	// The resume that fell back to a fresh start finds the rebuilt note
	// current and keeps it.
	s.prepareNote(l, false, now)
	if !rebuilt(note) {
		t.Error("the rebuilt note did not survive a second launch")
	}
}

func TestCodexToolRecord(t *testing.T) {
	call, _, id, kind := codexToolRecord([]byte(`{"type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{\"cmd\":\"ls\"}"}}`))
	if kind != "call" || id != "c1" || call != `shell: {"cmd":"ls"}` {
		t.Errorf("call %q %q %q", call, id, kind)
	}
	_, out, id, kind := codexToolRecord([]byte(`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}]}}`))
	if kind != "result" || id != "c1" || out != "a\nb" {
		t.Errorf("output %q %q %q", out, id, kind)
	}
}

func TestCut(t *testing.T) {
	if got := cut("héllo world", 2); got != "h […11 more bytes]" {
		t.Errorf("%q", got)
	}
	if got := cut("  short ", 10); got != "short" {
		t.Errorf("%q", got)
	}
}
