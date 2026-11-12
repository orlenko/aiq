//go:build darwin || linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
)

type nativeAttempt struct{ workspace, socket, terminal, previousRef string }

// The polling window is bounded; one already-running Workspace observation
// may add its existing dial/I/O latency. Observations are never abandoned.
func waitNativeAttemptEnd(want nativeAttempt, observe func() (herdr.Record, bool, error), now func() time.Time, pause func(time.Duration), budget time.Duration, incomplete func()) (herdr.Record, error) {
	start := now()
	deadline := start.Add(budget)
	var family string
	var generation uint64
	var lastState string
	var lastOwned, lastError bool
	expired := func() error {
		return fmt.Errorf("native attempt completion not confirmed: state=%q owned=%t observation_error=%t elapsed=%s", lastState, lastOwned, lastError, now().Sub(start))
	}
	for {
		if !now().Before(deadline) {
			return herdr.Record{}, expired()
		}
		r, owned, err := observe()
		lastState, lastOwned, lastError = r.State, owned, err != nil
		if !now().Before(deadline) {
			return herdr.Record{}, expired()
		}
		if r.Ref != "" {
			ref, g, e := herdr.ParseRef(r.Ref)
			if e != nil || g != 0 || ref != r.Ref || r.Ref == want.previousRef || r.Workspace != want.workspace || r.Core.Socket != want.socket || r.Core.Terminal != want.terminal || r.Generation == 0 {
				return herdr.Record{}, errors.New("native attempt completion identity mismatch")
			}
			if family == "" {
				family, generation = r.Ref, r.Generation
			}
			if r.Ref != family || r.Generation != generation {
				return herdr.Record{}, errors.New("native attempt completion identity changed")
			}
			if r.State == "ended" && r.PID != 0 {
				return herdr.Record{}, errors.New("native attempt ended with a nonzero child PID")
			}
			if err == nil && !owned && r.State == "ended" && r.PID == 0 {
				return r, nil
			}
			if incomplete != nil {
				incomplete()
			}
		}
		left := deadline.Sub(now())
		if left <= 0 {
			return herdr.Record{}, expired()
		}
		pause(min(10*time.Millisecond, left))
	}
}
func awaitNativeAttempt(t *testing.T, want nativeAttempt) herdr.Record {
	t.Helper()
	r, err := waitNativeAttemptEnd(want, func() (herdr.Record, bool, error) { return herdr.Workspace(want.workspace) }, time.Now, time.Sleep, 8*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func fixtureHC(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "hc", args...)
	cmd.WaitDelay = time.Second
	return cmd
}
func stopFixtureCore(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := fixtureHC(ctx, "server", "stop", "--json").CombinedOutput(); err != nil {
		t.Errorf("private core cleanup failed: %v", err)
	}
}
func nativeSQLIDs(t *testing.T, db *sql.DB, table string) []int64 {
	t.Helper()
	if table != "leases" && table != "launches" {
		t.Fatal("invalid fixture table")
	}
	rows, err := db.Query("SELECT id FROM " + table + " ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}
func assertNoProviderEntry(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("provider entry marker is present or unreadable: %v", err)
	}
}

func TestNativeAttemptCompletionPredicates(t *testing.T) {
	ref := herdr.Prefix + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 32)
	other := herdr.Prefix + strings.Repeat("a", 32) + "-" + strings.Repeat("c", 32)
	want := nativeAttempt{"/invented/work", "/invented/core", "t7", ""}
	end := herdr.Record{Ref: ref, Workspace: want.workspace, Generation: 1, State: "ended", Core: herdr.Core{Socket: want.socket, Terminal: want.terminal}}
	type observation struct {
		r     herdr.Record
		owned bool
		err   error
	}
	live := end
	live.State = "running"
	live.PID = 123
	for _, tc := range []struct {
		name    string
		seq     []observation
		change  func(*nativeAttempt)
		success bool
	}{
		{name: "already ended", seq: []observation{{r: end}}, success: true},
		{name: "missing then running then ended", seq: []observation{{}, {live, true, nil}, {end, false, nil}}, success: true},
		{name: "control error then ended", seq: []observation{{live, true, errors.New("fixture unavailable")}, {end, false, nil}}, success: true},
		{name: "owned ended then persisted ended", seq: []observation{{end, true, nil}, {end, false, nil}}, success: true},
		{name: "missing times out", seq: []observation{{}}},
		{name: "malformed times out", seq: []observation{{err: errors.New("fixture malformed"), owned: true}}},
		{name: "ended save never confirms", seq: []observation{{live, true, errors.New("fixture save unavailable")}}},
		{name: "wrong terminal", seq: []observation{{r: end}}, change: func(w *nativeAttempt) { w.terminal = "t8" }},
		{name: "wrong workspace", seq: []observation{{r: end}}, change: func(w *nativeAttempt) { w.workspace = "/invented/other" }},
		{name: "wrong socket", seq: []observation{{r: end}}, change: func(w *nativeAttempt) { w.socket = "/invented/other" }},
		{name: "previous tombstone", seq: []observation{{r: end}}, change: func(w *nativeAttempt) { w.previousRef = ref }},
		{name: "changed family", seq: []observation{{live, true, nil}, {func() herdr.Record { r := end; r.Ref = other; return r }(), false, nil}}},
		{name: "changed generation", seq: []observation{{live, true, nil}, {func() herdr.Record { r := end; r.Generation++; return r }(), false, nil}}},
		{name: "ended nonzero PID", seq: []observation{{r: func() herdr.Record { r := end; r.PID = 123; return r }()}}},
		{name: "invalid family", seq: []observation{{r: func() herdr.Record { r := end; r.Ref = "invalid"; return r }()}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := want
			if tc.change != nil {
				tc.change(&expected)
			}
			now := time.Unix(100, 0)
			calls := 0
			r, err := waitNativeAttemptEnd(expected, func() (herdr.Record, bool, error) {
				i := min(calls, len(tc.seq)-1)
				calls++
				o := tc.seq[i]
				return o.r, o.owned, o.err
			}, func() time.Time { return now }, func(d time.Duration) { now = now.Add(d) }, 30*time.Millisecond, nil)
			if tc.success {
				if err != nil || !reflect.DeepEqual(r, end) {
					t.Fatal(r, err)
				}
			} else if err == nil {
				t.Fatal("unconfirmed ownership released the barrier")
			}
		})
	}
	t.Run("late success", func(t *testing.T) {
		now := time.Unix(100, 0)
		_, err := waitNativeAttemptEnd(want, func() (herdr.Record, bool, error) { now = now.Add(time.Second); return end, false, nil }, func() time.Time { return now }, func(time.Duration) { t.Fatal("paused after late success") }, time.Millisecond, nil)
		if err == nil {
			t.Fatal("late completion accepted")
		}
	})
	t.Run("expired before observation", func(t *testing.T) {
		now := time.Unix(100, 0)
		_, err := waitNativeAttemptEnd(want, func() (herdr.Record, bool, error) { t.Fatal("expired observer called"); return end, false, nil }, func() time.Time { return now }, func(time.Duration) {}, 0, nil)
		if err == nil {
			t.Fatal("expired wait succeeded")
		}
	})
}

// The helper is always provider-free. Its child deliberately stops after its
// diagnostic, so the test can prove printed text does not release the barrier.
func TestHerdrCompletionFixture(t *testing.T) {
	if os.Getenv("GO_WANT_HERDR_COMPLETION_FIXTURE") != "1" {
		return
	}
	root := os.Getenv("GO_HERDR_COMPLETION_ROOT")
	if os.Args[len(os.Args)-1] == "child" {
		if err := os.WriteFile(filepath.Join(root, "entered"), []byte("invented"), 0600); err != nil {
			os.Exit(2)
		}
		fmt.Println("FICTION REGISTRATION ERROR")
		until := time.Now().Add(20 * time.Second)
		for time.Now().Before(until) {
			if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(2)
	}
	core, err := herdr.CurrentCore()
	if err != nil {
		fmt.Println("COMPLETION FIXTURE ERROR", err)
		os.Exit(2)
	}
	code, err := herdr.Run(herdr.Options{Core: core, Workspace: root, Session: "completion-fixture", Dir: root, Args: []string{os.Args[0], "-test.run=^TestHerdrCompletionFixture$", "--", "child"}})
	if err != nil {
		fmt.Println("COMPLETION FIXTURE ERROR", err)
		os.Exit(2)
	}
	os.Exit(code)
}
func TestNativeCompletionWaitsForHeldChild(t *testing.T) {
	if _, err := exec.LookPath("hc"); err != nil {
		t.Skip("hc unavailable; native completion evidence not run")
	}
	root, err := os.MkdirTemp("/tmp", "aiqh-completion-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("fixture root cleanup: %v", err)
		}
	})
	t.Setenv("HC_SOCKET", filepath.Join(root, "core.sock"))
	t.Setenv("HC_SESSION", "completion-fixture")
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "aiq"))
	t.Setenv("GO_WANT_HERDR_COMPLETION_FIXTURE", "1")
	t.Setenv("GO_HERDR_COMPLETION_ROOT", root)
	t.Cleanup(func() { stopFixtureCore(t) })
	done := make(chan struct{})
	waiting := false
	t.Cleanup(func() {
		if err := os.WriteFile(filepath.Join(root, "release"), []byte("release"), 0600); err != nil {
			t.Errorf("release held fixture: %v", err)
		}
		if waiting {
			select {
			case <-done:
			case <-time.After(17 * time.Second):
				t.Error("fixture observer did not finish")
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := fixtureHC(ctx, "run", "--bg", "--json", "--no-parent", "--cwd", root, "--", os.Args[0], "-test.run=^TestHerdrCompletionFixture$", "--", "runner").CombinedOutput()
	if err != nil {
		t.Fatalf("completion fixture launch failed: %v", err)
	}
	var address struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(out, &address); err != nil {
		t.Fatal(err)
	}
	waitFakeText(t, address.Address, "FICTION REGISTRATION ERROR")
	observed := make(chan struct{})
	var once sync.Once
	result := make(chan error, 1)
	waiting = true
	go func() {
		defer close(done)
		_, err := waitNativeAttemptEnd(nativeAttempt{root, os.Getenv("HC_SOCKET"), address.Address, ""}, func() (herdr.Record, bool, error) { return herdr.Workspace(root) }, time.Now, time.Sleep, 8*time.Second, func() { once.Do(func() { close(observed) }) })
		result <- err
	}()
	select {
	case <-observed:
	case <-time.After(3 * time.Second):
		t.Fatal("incomplete ownership not acknowledged")
	}
	select {
	case err := <-result:
		t.Fatalf("diagnostic released next row before completion: %v", err)
	default:
	}
	if err := os.WriteFile(filepath.Join(root, "release"), []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(17 * time.Second):
		t.Fatal("completion never confirmed")
	}
}

func assertNativeSQLIDs(t *testing.T, db *sql.DB, leases, launches []int64) {
	t.Helper()
	if !reflect.DeepEqual(nativeSQLIDs(t, db, "leases"), leases) || !reflect.DeepEqual(nativeSQLIDs(t, db, "launches"), launches) {
		t.Fatal("failed registration changed fixture lease or launch rows")
	}
}
func TestNativeEntryMarkerPrecedesReadiness(t *testing.T) {
	root := t.TempDir()
	script, entry := filepath.Join(root, "provider"), filepath.Join(root, "entry")
	if err := os.WriteFile(script, []byte(fakeHerdrProviderScript), 0700); err != nil {
		t.Fatal(err)
	}
	assertNoProviderEntry(t, entry)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", script)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "AIQ_PROVIDER=fiction-unknown", "GO_HERDR_ENTRY_MARKER="+entry)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("invented provider did not exit at its early branch: %v", err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("early provider entry was not recorded", err)
	}
	if strings.Contains(string(out), "FAKE READY") {
		t.Fatal("fixture reached readiness unexpectedly")
	}
}
