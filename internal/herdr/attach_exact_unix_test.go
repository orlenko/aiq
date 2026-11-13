//go:build unix

package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func attachExactFixture(t *testing.T) (Record, func(Record), *atomic.Int32) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "aiqh-exact-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("AIQ_DATA_DIR", root)
	workspace := filepath.Join(root, "workspace")
	ref, err := newRef(workspace)
	if err != nil {
		t.Fatal(err)
	}
	coreSocket, _ := addressEndpoint(t, 1, "fixture-core", "t1", "live", 456)
	r := Record{Ref: ref, Workspace: workspace, Socket: filepath.Join(root, "control"), RunnerPID: 123, PID: 2468, Generation: 7, State: "running", LeaseID: 999, LaunchID: 888, Core: Core{Socket: coreSocket, ID: "fixture-core", Terminal: "t1", PID: 456}}
	if err := os.MkdirAll(directory(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", r.Socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	current := r
	calls := &atomic.Int32{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(time.Second))
				var q struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(c).Decode(&q) != nil {
					return
				}
				calls.Add(1)
				mu.Lock()
				snapshot := current
				mu.Unlock()
				if q.Method != "info" {
					t.Error("exact attachment attempted state mutation")
				}
				json.NewEncoder(c).Encode(map[string]any{"id": q.ID, "result": response{Record: snapshot}})
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); wg.Wait() })
	return r, func(next Record) { mu.Lock(); current = next; mu.Unlock() }, calls
}

func TestAttachExactNativeIdentityAndDrift(t *testing.T) {
	for _, mode := range []string{"generation", "state", "child", "workspace", "registration", "launch", "core-id", "core-terminal", "core-pid", "vanished"} {
		t.Run(mode, func(t *testing.T) {
			r, update, calls := attachExactFixture(t)
			action, err := PrepareAttachExact(r.Target(), r.Workspace, r.PID)
			if err != nil || action == nil {
				t.Fatal("positive adopted registration rejected", err)
			}
			next := r
			switch mode {
			case "generation":
				next.Generation++
			case "state":
				next.State = "ended"
			case "child":
				next.PID++
			case "workspace":
				next.Workspace = "/another-workspace"
			case "registration":
				next.LeaseID = 0
			case "launch":
				next.LaunchID = 0
			case "core-id":
				next.Core.ID = "replacement"
			case "core-terminal":
				next.Core.Terminal = "t2"
			case "core-pid":
				next.Core.PID++
			case "vanished":
				next.Ref = "invalid"
			}
			update(next)
			bin := t.TempDir()
			marker := filepath.Join(bin, "viewer-ran")
			if err := os.WriteFile(filepath.Join(bin, "hc"), []byte("#!/bin/sh\nprintf ran > \"$GO_EXACT_MARKER\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("GO_EXACT_MARKER", marker)
			if err := action(); err == nil || strings.Contains(err.Error(), "replacement") {
				t.Fatal("changed child accepted or detail leaked", err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("changed target reached viewer")
			}
			if calls.Load() != 2 {
				t.Fatal("exact attach retried/fell back instead of one recheck")
			}
		})
	}
	for _, mode := range []string{"generation", "state", "child", "workspace", "registration", "launch"} {
		t.Run("preflight-"+mode, func(t *testing.T) {
			r, update, _ := attachExactFixture(t)
			next := r
			switch mode {
			case "generation":
				next.Generation++
			case "state":
				next.State = "starting"
			case "child":
				next.PID++
			case "workspace":
				next.Workspace = "/other"
			case "registration":
				next.LeaseID = -1
			case "launch":
				next.LaunchID = 0
			}
			update(next)
			if a, err := PrepareAttachExact(r.Target(), r.Workspace, r.PID); err == nil || a != nil {
				t.Fatal("invalid native preflight ready")
			}
		})
	}
}

func TestAttachExactViewerChild(t *testing.T) {
	if os.Getenv("GO_EXACT_CHILD") != "1" {
		return
	}
	action, err := PrepareAttachExact(os.Getenv("GO_EXACT_REF"), os.Getenv("GO_EXACT_WORKSPACE"), 2468)
	if err != nil {
		t.Fatal(err)
	}
	if err := action(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachExactSavedCoreAndDiagnostics(t *testing.T) {
	r, update, _ := attachExactFixture(t)
	// A later registry endpoint spelling must not replace the saved endpoint.
	next := r
	next.Core.Socket = "/invented/unreachable-replacement.sock"
	// The prepared closure must keep its first endpoint even after Info
	// reports a different spelling with the same core identity.
	action, err := PrepareAttachExact(r.Target(), r.Workspace, r.PID)
	if err != nil {
		t.Fatal(err)
	}
	update(next)
	bin := t.TempDir()
	marker := filepath.Join(bin, "viewer")
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" \"$2\" \"$HC_SOCKET\" \"${HC_TERMINAL_SOCKET-unset}\" \"${HC_TERMINAL_ID-unset}\" \"${HC_SESSION-unset}\" > \"$GO_EXACT_MARKER\"\nprintf 'PRIVATE backend diagnostic' >&2\nprintf 'fixture-view\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "hc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GO_EXACT_MARKER", marker)
	t.Setenv("HC_TERMINAL_ID", "t2")
	t.Setenv("HC_TERMINAL_SOCKET", "/invented/caller")
	t.Setenv("HC_SOCKET", "/invented/selector")
	t.Setenv("HC_SESSION", "fixture-session")
	if err := action(); err != nil {
		t.Fatal("prepared endpoint not retained", err)
	}
	b, err := os.ReadFile(marker)
	if err != nil || string(b) != "attach\nt1\n"+r.Core.Socket+"\nunset\nunset\nunset\n" {
		t.Fatal("viewer endpoint/context not pinned", err)
	}
	update(r)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestAttachExactViewerChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GO_EXACT_CHILD=1", "GO_EXACT_REF="+r.Target(), "GO_EXACT_WORKSPACE="+r.Workspace)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("invented viewer child: %v %s", err, stdout.Bytes())
	}
	if stderr.Len() != 0 {
		t.Fatal("exact viewer leaked backend diagnostic")
	}
	if !strings.HasPrefix(stdout.String(), "fixture-view\n") {
		t.Fatal("interactive stdout not preserved")
	}
}
