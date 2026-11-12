//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func nativeRecordFixture(t *testing.T, ws string) herdr.Record {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "aiqh-projection-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	hash := sha256.Sum256([]byte(ws))
	r := herdr.Record{Ref: herdr.Prefix + hex.EncodeToString(hash[:16]) + "-" + strings.Repeat("a", 32), Workspace: ws, Socket: filepath.Join(root, "control"), RunnerPID: os.Getpid(), PID: os.Getpid(), Generation: 1, State: "starting", Core: herdr.Core{Socket: filepath.Join(root, "core"), ID: "fiction-core", Terminal: "t1", PID: 456}}
	saveNativeFixture(t, r)
	return r
}
func saveNativeFixture(t *testing.T, r herdr.Record) {
	t.Helper()
	hash := sha256.Sum256([]byte(r.Workspace))
	dir := filepath.Join(paths.DataDir(), "herdr")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(filepath.Join(dir, hex.EncodeToString(hash[:16])+".json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func nativeInfoFixture(t *testing.T, r herdr.Record) (*atomic.Pointer[herdr.Record], *atomic.Int32) {
	t.Helper()
	l, err := net.Listen("unix", r.Socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	current := &atomic.Pointer[herdr.Record]{}
	current.Store(&r)
	mutations := &atomic.Int32{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				var env struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(c).Decode(&env) != nil {
					return
				}
				if env.Method != "info" {
					mutations.Add(1)
				}
				json.NewEncoder(c).Encode(map[string]any{"id": env.ID, "result": map[string]any{"Record": *current.Load()}})
			}()
		}
	}()
	return current, mutations
}
func TestNativeOwnershipListAndMenu(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	t.Setenv("PWD", ws)
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	a := &app{cfg: cfg, st: st, pool: &pool.Pool{Cfg: cfg, St: st}}
	empty, err := stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(empty, "no long sessions") {
		t.Fatal(empty, err)
	}
	r := nativeRecordFixture(t, ws)
	current, _ := nativeInfoFixture(t, r)
	out, err := stdoutOf(t, a.longList)
	if err != nil || strings.Contains(out, "no long sessions") || !strings.Contains(out, "starting") || !strings.Contains(out, ws) {
		t.Fatalf("descriptor-only startup hidden: %s %v", out, err)
	}
	id, err := st.AddLease(state.Lease{AccountID: "claude/fiction", Mode: state.ModeLong, Workspace: ws, Pane: r.Target(), PID: r.PID, StartedAt: time.Now().Unix(), Hostname: pool.Hostname()})
	if err != nil {
		t.Fatal(err)
	}
	out, err = stdoutOf(t, a.longList)
	if err != nil || strings.Count(out, ws) != 1 {
		t.Fatalf("current lease duplicated: %s %v", out, err)
	}
	if got := a.menuEnvironment(ws).longHere; got != "Herdr (starting)" {
		t.Fatalf("starting qualifier overwritten: %s", got)
	}
	next := r
	next.Generation++
	next.State = "running"
	current.Store(&next)
	out, err = stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(out, "stale") || strings.Count(out, ws) != 2 {
		t.Fatalf("stale lease hid current ownership: %s %v", out, err)
	}
	current.Store(&r)
	if err := os.Rename(r.Socket, r.Socket+".offline"); err != nil {
		t.Fatal(err)
	}
	out, err = stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(out, "unknown") {
		t.Fatalf("unknown lease hidden: %s %v", out, err)
	}
	if got := a.menuEnvironment(ws).longHere; got != "Herdr (unreachable)" {
		t.Fatalf("unreachable qualifier overwritten: %s", got)
	}
	st.ReleaseLease(id)
	out, err = stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(out, "unknown (starting)") {
		t.Fatalf("descriptor-only unknown hidden: %s %v", out, err)
	}
	r.State = "ended"
	saveNativeFixture(t, r)
	out, err = stdoutOf(t, a.longList)
	if err != nil || !strings.Contains(out, "no long sessions") {
		t.Fatalf("ended tombstone counted active: %s %v", out, err)
	}
}

func TestNativeSavedGenerationBlocksBothResumePaths(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	t.Setenv("PWD", ws)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	project := filepath.Join(home, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(ws, "-"))
	os.MkdirAll(project, 0700)
	sid := "11111111-1111-4111-8111-111111111111"
	text := fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"invented task"}}`+"\n", sid, ws)
	os.WriteFile(filepath.Join(project, sid+".jsonl"), []byte(text), 0600)
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app{cfg: config.Default(), st: st}
	r := nativeRecordFixture(t, ws)
	old := r.Target()
	r.Generation = 2
	r.State = "running"
	saveNativeFixture(t, r)
	_, mutations := nativeInfoFixture(t, r)
	st.AddLease(state.Lease{AccountID: "claude/fiction", Mode: state.ModeLong, Workspace: ws, Pane: old, PID: r.PID, SessionID: sid, StartedAt: time.Now().Unix(), Hostname: pool.Hostname()})
	if err := a.resumeSession(transcript.Session{ID: sid}, nil, liveNote{native: old}, resumeOpts{}); !errors.Is(err, herdr.ErrGenerationChanged) {
		t.Fatalf("resume fell through changed generation: %v", err)
	}
	_, done, err := a.pickResume(resumeRequest{o: resumeOpts{id: sid}})
	if !done || !errors.Is(err, herdr.ErrGenerationChanged) {
		t.Fatalf("long resume fell through changed generation: done=%v err=%v", done, err)
	}
	if mutations.Load() != 0 {
		t.Fatal("resume mutated a terminal")
	}
	launches, _ := st.ListLaunches("", nil, nil)
	if len(launches) != 0 {
		t.Fatal("resume launched provider")
	}
}

func TestNativeListingHasAggregateControlBudget(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	for i := 0; i < 8; i++ {
		r := nativeRecordFixture(t, filepath.Join(t.TempDir(), fmt.Sprint(i)))
		l, err := net.Listen("unix", r.Socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					c.SetDeadline(time.Now().Add(3 * time.Second))
					var b [4096]byte
					for {
						if _, err := c.Read(b[:]); err != nil {
							return
						}
					}
				}()
			}
		}()
	}
	start := time.Now()
	views, records, err := listNativeOwnership(nil)
	if err != nil || len(views) != 8 || len(records) != 8 || time.Since(start) > 3*time.Second {
		t.Fatalf("unbounded or missing ownership: %d %d %s %v", len(views), len(records), time.Since(start), err)
	}
}

func TestNativeTmuxRespawnKeepsStartupLock(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws := t.TempDir()
	root := t.TempDir()
	gate := filepath.Join(root, "gate")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	// The fake server acknowledges entering the actual mutation, then waits for
	// the test's pipe byte. Both backend entry paths must still see the lock.
	script := "#!/bin/sh\nprintf x > \"$AIQ_TEST_RESPAWN_GATE\"\nIFS= read -r line < \"$AIQ_TEST_RESPAWN_RELEASE\"\n"
	release := filepath.Join(root, "release")
	if err := syscall.Mkfifo(release, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIQ_TEST_RESPAWN_GATE", gate)
	t.Setenv("AIQ_TEST_RESPAWN_RELEASE", release)
	unlock, err := herdr.LockWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a := &app{cfg: config.Default()}
	done := make(chan error, 1)
	go func() {
		done <- a.longLaunchInPlace(longTarget{name: "fiction", pane: "%1", inside: true, release: unlock}, ws, ws, "codex", "", []string{"/invented/aiq", "run", "codex", "--long"})
	}()
	f, err := os.Open(gate)
	if err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	_, err = f.Read(b[:])
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"tmux", "herdr"} {
		a.longBackend = backend
		_, _, err := a.longExisting(ws)
		if err == nil || !strings.Contains(err.Error(), "start is in progress") {
			t.Fatalf("%s acquired early startup: %v", backend, err)
		}
	}
	f, err = os.OpenFile(release, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("continue\n")
	f.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	next, err := herdr.LockWorkspace(ws)
	if err != nil {
		t.Fatalf("completed respawn retained lock: %v", err)
	}
	next()
}
