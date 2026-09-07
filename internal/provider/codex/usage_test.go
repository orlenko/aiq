package codex

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestWindowsFromRateLimits(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rl := RateLimits{
		PrimaryUsedPct: 100, PrimaryReset: now.Add(5 * 24 * time.Hour).Unix(), PrimaryWindowSecs: 604800,
		SecondaryUsedPct: 12, SecondaryReset: now.Add(3 * time.Hour).Unix(), SecondaryWindowSec: 18000,
	}
	ws := WindowsFromRateLimits(rl, now)
	if len(ws) != 2 {
		t.Fatalf("%+v", ws)
	}
	if ws[0].Key != "plan:primary_window" || ws[0].Kind != state.KindWeekly || ws[0].Label != "Weekly" || ws[0].Severity != "critical" {
		t.Fatalf("primary: %+v", ws[0])
	}
	if ws[1].Kind != state.KindShort || ws[1].Label != "5h" || ws[1].Severity != "" {
		t.Fatalf("secondary: %+v", ws[1])
	}
	// Unknown window length: the reset horizon decides.
	ws = WindowsFromRateLimits(RateLimits{PrimaryUsedPct: 5, PrimaryReset: now.Add(6 * 24 * time.Hour).Unix(), SecondaryUsedPct: -1}, now)
	if len(ws) != 1 || ws[0].Kind != state.KindWeekly {
		t.Fatalf("%+v", ws)
	}
}

func TestIdentity(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"u@example.com","https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`))
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	os.WriteFile(path, []byte(`{"tokens":{"id_token":"hdr.`+payload+`.sig"}}`), 0o600)
	email, plan := Identity(path)
	if email != "u@example.com" || plan != "pro" {
		t.Fatalf("%q %q", email, plan)
	}
	if e, _ := Identity(filepath.Join(dir, "missing")); e != "" {
		t.Fatal("missing file should yield nothing")
	}
}

// The argv an orchestrator uses for a review seat: `exec` first, prompt on
// stdin, result written to a file. It must classify as a worker regardless
// of the terminal.
func TestIsWorkerOnOrchestratorArgv(t *testing.T) {
	argv := []string{"exec", "--ephemeral", "-C", "/repo", "-s", "read-only", "-m", "gpt-6-astra",
		"-c", "model_reasoning_effort=high", "--color", "never", "-o", "/out/last.md", "-"}
	if !IsWorker(argv) {
		t.Fatal("codex exec … - must be a worker")
	}
	if !IsWorker([]string{"--yolo", "exec", "x"}) {
		t.Fatal("exec after a global flag is still a worker")
	}
}
