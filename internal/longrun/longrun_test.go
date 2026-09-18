package longrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestSessionNameAndHandoffPathAreStable(t *testing.T) {
	ws := "/home/me/code/my.project"
	a := SessionName("aiq", ws)
	b := SessionName("aiq", ws)
	if a != b || !strings.HasPrefix(a, "aiq-my-project-") {
		t.Fatalf("%s %s", a, b)
	}
	if SessionName("aiq", "/home/me/code/other") == a {
		t.Fatal("different workspaces must not collide")
	}
	if HandoffPath(ws) != HandoffPath(ws) || !strings.HasSuffix(HandoffPath(ws), ".md") {
		t.Fatal("handoff path must be deterministic")
	}
}

func TestPromptsMentionTheNote(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws := "/w"
	d := DrainInstruction(ws, 3, ", resets 14:00")
	if !strings.Contains(d, HandoffPath(ws)) || !strings.Contains(d, "3% left, resets 14:00") {
		t.Fatalf("%s", d)
	}
	p := TakeoverPrompt(ws, false, "claude")
	if !strings.Contains(p, "No handoff note") || !strings.Contains(p, "from a claude session") {
		t.Fatalf("%s", p)
	}
	os.MkdirAll(filepath.Dir(HandoffPath(ws)), 0o700)
	os.WriteFile(HandoffPath(ws), []byte("# note"), 0o600)
	p = TakeoverPrompt(ws, true, "claude")
	if !strings.Contains(p, "Read the handoff note") || !strings.Contains(p, "conversation above is yours") {
		t.Fatalf("%s", p)
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`["--model","x y","-p"]`)
	if len(got) != 3 || got[1] != "x y" {
		t.Fatalf("%v", got)
	}
	if got := splitArgs("a b"); len(got) != 2 {
		t.Fatalf("fallback: %v", got)
	}
}

func TestTranslateArgsCarriesThePermissionBypass(t *testing.T) {
	cases := []struct {
		from, to string
		in, want []string
	}{
		{"claude", "codex", []string{"--dangerously-skip-permissions", "--model", "opus"}, []string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-5.6-sol"}},
		{"claude", "codex", []string{"--permission-mode", "bypassPermissions"}, []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"claude", "codex", []string{"--permission-mode=bypassPermissions"}, []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"codex", "claude", []string{"--yolo", "-m", "gpt-5"}, []string{"--dangerously-skip-permissions"}},
		{"codex", "claude", []string{"--dangerously-bypass-approvals-and-sandbox"}, []string{"--dangerously-skip-permissions"}},
		{"claude", "codex", []string{"--permission-mode", "acceptEdits"}, nil},
		{"claude", "codex", []string{"--model", "claude-opus-5"}, nil},
		{"claude", "codex", []string{"--model", "opus"}, []string{"--model", "gpt-5.6-sol"}},
		{"claude", "codex", []string{"--model=fable", "--effort", "ultracode", "--dangerously-skip-permissions"},
			[]string{"--dangerously-bypass-approvals-and-sandbox", "--model", "gpt-6-astra", "-c", "model_reasoning_effort=ultra"}},
		{"codex", "claude", []string{"--model", "gpt-5.6-luna", "-c", "model_reasoning_effort=high", "--yolo"},
			[]string{"--dangerously-skip-permissions", "--model", "haiku", "--effort", "high"}},
		{"codex", "claude", []string{"-m", "gpt-5.6-terra", "--config=model_reasoning_effort=\"low\""}, []string{"--model", "sonnet", "--effort", "low"}},
		{"claude", "codex", []string{"--", "--model", "opus"}, nil},
		{"claude", "claude", []string{"--model", "opus"}, []string{"--model", "opus"}},
	}
	for _, c := range cases {
		got := TranslateArgs(c.from, c.to, c.in)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s→%s %v: got %v, want %v", c.from, c.to, c.in, got, c.want)
		}
	}
}

func TestReplayArgsDropsSessionSelection(t *testing.T) {
	cases := []struct {
		provider string
		in, want []string
	}{
		{"codex", []string{"--yolo", "resume"}, []string{"--yolo"}},
		{"codex", []string{"resume", "--last", "--yolo"}, []string{"--yolo"}},
		{"codex", []string{"--yolo", "resume", "01a0", "do it"}, []string{"--yolo"}},
		{"codex", []string{"fork", "01a0", "-m", "gpt-5"}, []string{"-m", "gpt-5"}},
		{"codex", []string{"--yolo", "-c", "model=\"x\""}, []string{"--yolo", "-c", "model=\"x\""}},
		{"claude", []string{"--dangerously-skip-permissions", "--resume", "abc"}, []string{"--dangerously-skip-permissions"}},
		{"claude", []string{"--resume"}, nil},
		{"claude", []string{"-c", "--model", "opus"}, []string{"--model", "opus"}},
		{"claude", []string{"--session-id=abc", "-r"}, nil},
		{"claude", []string{"--model", "opus"}, []string{"--model", "opus"}},
	}
	for _, c := range cases {
		got := replayArgs(c.provider, c.in)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s %v: got %v, want %v", c.provider, c.in, got, c.want)
		}
	}
}

func TestDrainInstructionKeepsTheAgentWorking(t *testing.T) {
	d := DrainInstruction("/w", 3, "")
	for _, want := range []string{"Keep working", "do not stop", "Keep the note current"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in %s", want, d)
		}
	}
	if strings.Contains(d, "end your turn") || strings.Contains(d, "do not start new work") {
		t.Errorf("drain must not tell the agent to stop: %s", d)
	}
}

func TestIdleRotateDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	idle := 15 * time.Minute
	ended := now.Add(-20 * time.Minute)
	quiet := endedAt(ended)
	cases := []struct {
		name      string
		lease     state.Lease
		remaining float64
		pct       float64
		lastWrite time.Time
		want      bool
	}{
		{"quiet and low", quiet, 8, 15, ended, true},
		{"plenty left", quiet, 40, 15, ended, false},
		{"turned off", quiet, 8, 0, ended, false},
		{"in a turn", func() state.Lease { x := quiet; x.TurnStartedAt = now.Unix(); return x }(), 8, 15, ended, false},
		{"idle too briefly", endedAt(now.Add(-5 * time.Minute)), 8, 15, now.Add(-5 * time.Minute), false},
		{"background work still writing", quiet, 8, 15, now.Add(-time.Minute), false},
		{"transcript unknown", quiet, 8, 15, time.Time{}, false},
		{"hooks never reported", func() state.Lease { x := quiet; x.SessionID = ""; return x }(), 8, 15, ended, false},
		{"no turn yet", func() state.Lease { x := quiet; x.TurnStartedAt, x.TurnEndedAt = 0, 0; return x }(), 8, 15, ended, false},
	}
	for _, c := range cases {
		if got := IdleRotateDue(c.lease, c.remaining, c.pct, idle, c.lastWrite, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func endedAt(ended time.Time) state.Lease {
	return state.Lease{SessionID: "s", TurnStartedAt: ended.Add(-time.Minute).Unix(), TurnEndedAt: ended.Unix()}
}

func TestLastWriteSeesSubagents(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "abc.jsonl")
	os.WriteFile(tr, []byte("{}\n"), 0o600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(tr, old, old)
	if got := LastWrite(tr); !got.Equal(old) {
		t.Fatalf("transcript only: got %v, want %v", got, old)
	}
	sub := filepath.Join(dir, "abc", "subagents", "workflows", "wf1")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "agent.jsonl"), []byte("{}\n"), 0o600)
	if got := LastWrite(tr); time.Since(got) > time.Minute {
		t.Fatalf("a fresh subagent write must count: %v", got)
	}
	if !LastWrite("").IsZero() || !LastWrite(filepath.Join(dir, "missing.jsonl")).IsZero() {
		t.Fatal("unknown transcript must be zero")
	}
}
