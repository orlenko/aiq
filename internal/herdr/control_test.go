package herdr

import (
	"encoding/json"
	"errors"
	"net"
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

func TestNativeGenerationGrammarAndStaleAuthority(t *testing.T) {
	family := Prefix + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 32)
	for _, suffix := range []string{"@", "@0", "@01", "@-1", "@+1", "@1@2", "@18446744073709551616", "@1/x"} {
		if _, _, err := ParseRef(family + suffix); err == nil {
			t.Errorf("accepted %q", suffix)
		}
	}
	if f, g, err := ParseRef(family + "@18446744073709551615"); err != nil || f != family || g != ^uint64(0) {
		t.Fatal("uint64 boundary", f, g, err)
	}
	for _, op := range []func() error{
		func() error { return Replace(family, 123, "/invented", []string{"/invented/aiq"}) },
		func() error { return Stop(family, 123) },
		func() error { return NameFor(family, 123, "fiction") },
	} {
		if err := op(); err == nil {
			t.Fatal("untagged target acquired mutation authority")
		}
	}
	root, err := os.MkdirTemp("/tmp", "aiqh-authority-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	ws := filepath.Join(root, "workspace")
	ref, err := newRef(ws)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Ref: ref, Workspace: ws, RunnerPID: os.Getpid(), PID: 123, Generation: 2, State: "running", Socket: filepath.Join(root, "control"), Core: Core{Socket: filepath.Join(root, "core"), ID: "fiction-core", Terminal: "t1", PID: 456}}
	if err := os.MkdirAll(directory(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", r.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan request, 8)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				var env struct {
					ID     int     `json:"id"`
					Method string  `json:"method"`
					Params request `json:"params"`
				}
				if json.NewDecoder(c).Decode(&env) != nil {
					return
				}
				out := response{Record: r}
				if env.Method != "info" {
					requests <- env.Params
					if env.Params.Generation != r.Generation {
						out.Error = ErrGenerationChanged.Error()
					}
				}
				json.NewEncoder(c).Encode(map[string]any{"id": env.ID, "result": out})
			}()
		}
	}()
	old := r
	old.Generation = 1
	for _, op := range []func() error{
		func() error { return Replace(old.Target(), r.PID, ws, []string{"/invented/aiq"}) },
		func() error { return Stop(old.Target(), r.PID) },
		func() error { return NameFor(old.Target(), r.PID, "fiction") },
	} {
		if err := op(); !errors.Is(err, ErrGenerationChanged) {
			t.Fatalf("stale aliased PID got %v", err)
		}
		q := <-requests
		if q.Generation != 1 || q.PID != 123 || q.Ref != ref {
			t.Fatalf("authority refreshed: %+v", q)
		}
	}
	if err := Attach(old.Target()); !errors.Is(err, ErrGenerationChanged) || errors.Is(err, ErrEnded) {
		t.Fatalf("stale attach became absence: %v", err)
	}
	if got, err := Info(old.Target()); err != nil || got.Generation != 2 {
		t.Fatalf("observation cannot follow family: %+v %v", got, err)
	}
}
