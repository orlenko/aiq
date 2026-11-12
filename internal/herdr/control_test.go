package herdr

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPersistentCoreEnvironmentCannotBypassRouting(t *testing.T) {
	env := ChildEnv([]string{"HOME=/invented/home", "PATH=/invented/bin", "AIQ_DATA_DIR=/invented/data", "AIQ_CONFIG=/invented/config",
		"AIQ_BYPASS=1", "AIQ_CHAIN=codex\x1f123\x1f/invented/wrapper", "AIQ_LONG=1", "AIQ_LEASE=12", "AIQ_DEPTH=8", "AIQ_ROOT=5",
		"ANTHROPIC_AUTH_TOKEN=fiction", "OPENAI_API_KEY=fiction", "CODEX_API_KEY=fiction", "TMUX=fiction", "HC_SOCKET=/invented/core.sock"}, "herdr:fixture", 3)
	got := strings.Join(env, "\n")
	for _, key := range []string{"AIQ_BYPASS=", "AIQ_CHAIN=", "AIQ_LONG=", "AIQ_LEASE=", "AIQ_DEPTH=", "AIQ_ROOT=", "AUTH_TOKEN=", "API_KEY=", "TMUX="} {
		if strings.Contains(got, key) {
			t.Fatalf("inherited routing/auth override %s survived", key)
		}
	}
	for _, kv := range []string{"AIQ_DATA_DIR=/invented/data", "AIQ_CONFIG=/invented/config", "HC_SOCKET=/invented/core.sock", "AIQ_TERMINAL_GENERATION=3"} {
		if !strings.Contains(got, kv) {
			t.Fatalf("lost launch context %s", kv)
		}
	}
}

func TestUnknownOwnershipIsNotVacancy(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ref, err := newRef(ws)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Ref: ref, Workspace: ws, RunnerPID: os.Getpid(), PID: os.Getpid(), Socket: filepath.Join(ws, "missing.sock"), State: "running", Core: Core{Socket: filepath.Join(ws, "fiction-core.sock"), ID: "fiction-core", Terminal: "t1", PID: os.Getpid()}}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	got, owned, err := Workspace(ws)
	if err == nil || !owned || got.Ref != ref {
		t.Fatalf("unknown control became vacancy: owned=%v ref=%q err=%v", owned, got.Ref, err)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if reclaimed, err := Reclaim(r); err != nil || reclaimed {
			t.Fatalf("live owner reclaimed: %v %v", reclaimed, err)
		}
	}
	unlock, err := LockWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if second, err := LockWorkspace(ws); err == nil {
		second()
		t.Fatal("simultaneous workspace start acquired ownership")
	}
	if _, err := load("herdr:../../invented"); err == nil {
		t.Fatal("invalid ref accepted")
	}
}
