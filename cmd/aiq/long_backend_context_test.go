//go:build darwin || linux

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func clearLongContext(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HC_TERMINAL_ID", "HC_TERMINAL_SOCKET", "HC_SOCKET", "TMUX", "TMUX_PANE", "STY"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLongBackendContextAndExplicitOverrides(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"outside", nil, "tmux"},
		{"client selector only", map[string]string{"HC_SOCKET": "/invented/client"}, "tmux"},
		{"empty terminal ID", map[string]string{"HC_TERMINAL_ID": ""}, "tmux"},
		{"terminal", map[string]string{"HC_TERMINAL_ID": "t1", "HC_TERMINAL_SOCKET": "/invented/owner"}, "herdr"},
		{"legacy terminal", map[string]string{"HC_TERMINAL_ID": "t1", "HC_SOCKET": "/invented/client"}, "herdr"},
		{"ID only", map[string]string{"HC_TERMINAL_ID": "t1"}, "herdr"},
		{"owner only", map[string]string{"HC_TERMINAL_SOCKET": "/invented/owner"}, "herdr"},
		{"empty owner", map[string]string{"HC_TERMINAL_SOCKET": ""}, "herdr"},
		{"nested tmux", map[string]string{"TMUX": "/invented/tmux,123,0", "HC_TERMINAL_ID": "t1"}, "tmux"},
		{"nested pane", map[string]string{"TMUX_PANE": "%1", "HC_TERMINAL_SOCKET": "/invented/owner"}, "tmux"},
		{"nested screen", map[string]string{"STY": "fixture", "HC_TERMINAL_ID": "t1"}, "tmux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearLongContext(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			for _, explicit := range []string{"", "tmux", "herdr"} {
				want := tc.want
				if explicit != "" {
					want = explicit
				}
				if got := longBackendForContext(explicit); got != want {
					t.Fatalf("choice %q: got %q, want %q", explicit, got, want)
				}
			}
		})
	}
}

// Both endpoints and their replies are invented. No real core is contacted.
func autoCoreEndpoint(t *testing.T, fault string) (string, *atomic.Int32) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "aiq-auto-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "core.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(root)
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
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
					ID     int
					Method string
				}
				if json.NewDecoder(c).Decode(&q) != nil {
					return
				}
				calls.Add(1)
				generation, address, pid := 1, "t1", 0
				if fault == "protocol" {
					generation = 2
				}
				if fault == "address" {
					address = "t2"
				}
				if fault == "tty" {
					pid = 2147483647
				}
				var result any = map[string]any{"generation": generation, "core": map[string]any{"id": "fixture-core"}}
				if q.Method == "terminal.get" {
					result = map[string]any{"address": address, "state": "live", "pid": pid}
				}
				json.NewEncoder(c).Encode(map[string]any{"id": q.ID, "result": result})
			}()
		}
	}()
	t.Cleanup(func() { l.Close(); wg.Wait(); os.RemoveAll(root) })
	return socket, calls
}

func autoTmuxFixture(t *testing.T, panes string) string {
	t.Helper()
	root := t.TempDir()
	log := filepath.Join(root, "calls")
	script := `#!/bin/sh
printf '%s\n' "$1" >> "$AIQ_TEST_AUTO_TMUX_LOG"
case "$1" in
  has-session) test -n "$AIQ_TEST_AUTO_PANES" ;;
  list-panes) printf '%s\n' "$AIQ_TEST_AUTO_PANES" ;;
  attach-session|switch-client|kill-session) exit 0 ;;
  *) exit 93 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIQ_TEST_AUTO_TMUX_LOG", log)
	t.Setenv("AIQ_TEST_AUTO_PANES", panes)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func TestAutomaticNativeAdmissionNeverFallsBack(t *testing.T) {
	for _, fault := range []string{"outside", "client selector only", "empty owner", "missing ID", "ID only", "protocol", "address", "pid", "tty", "tmux override", "nested tmux", "nested pane", "nested screen", "explicit nested native"} {
		t.Run(fault, func(t *testing.T) {
			clearLongContext(t)
			t.Setenv("AIQ_DATA_DIR", t.TempDir())
			selector, selectorCalls := autoCoreEndpoint(t, "pid")
			owner, _ := autoCoreEndpoint(t, fault)
			t.Setenv("HC_SOCKET", selector)
			t.Setenv("HC_TERMINAL_ID", "t1")
			t.Setenv("HC_TERMINAL_SOCKET", owner)
			backend, wantError := "", true
			switch fault {
			case "outside", "client selector only":
				os.Unsetenv("HC_TERMINAL_ID")
				os.Unsetenv("HC_TERMINAL_SOCKET")
				if fault == "outside" {
					os.Unsetenv("HC_SOCKET")
				}
				wantError = false
			case "empty owner":
				t.Setenv("HC_TERMINAL_SOCKET", "")
			case "missing ID":
				t.Setenv("HC_TERMINAL_ID", "")
			case "ID only":
				os.Unsetenv("HC_TERMINAL_SOCKET")
				os.Unsetenv("HC_SOCKET")
			case "tmux override":
				backend, wantError = "tmux", false
			case "nested tmux":
				t.Setenv("TMUX", "/invented/socket,123,0")
				wantError = false
			case "nested pane":
				t.Setenv("TMUX_PANE", "%7")
				wantError = false
			case "nested screen":
				t.Setenv("STY", "fixture")
				wantError = false
			case "explicit nested native":
				t.Setenv("TMUX_PANE", "%7")
				backend = "herdr"
			}
			log := autoTmuxFixture(t, "")
			st, err := state.Open(paths.StateDB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			a := &app{cfg: config.Default(), st: st, longBackend: backend}
			target, attached, err := a.longExisting(t.TempDir())
			target.unlock()
			if attached || target.native || (err != nil) != wantError {
				t.Fatalf("wrong admission: attached=%v native=%v err=%v", attached, target.native, err)
			}
			if wantError && backend == "" && !strings.Contains(err.Error(), "aiq long --backend tmux auto") {
				t.Fatal("automatic refusal omitted recovery command")
			}
			if selectorCalls.Load() != 0 {
				t.Fatal("owner failure retried the client selector")
			}
			b, _ := os.ReadFile(log)
			for _, call := range strings.Fields(string(b)) {
				if call != "has-session" {
					t.Fatalf("refusal/selection mutated tmux: %q", call)
				}
			}
			launches, e := st.ListLaunches("", nil, nil)
			if e != nil || len(launches) != 0 {
				t.Fatal("selection launched a provider", e)
			}
			if a.longBackend != backend {
				t.Fatal("detected backend became persistent app state")
			}
		})
	}
}

func TestAutomaticNativePreferenceAttachesExistingTmux(t *testing.T) {
	clearLongContext(t)
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	owner, calls := autoCoreEndpoint(t, "protocol")
	t.Setenv("HC_TERMINAL_ID", "t1")
	t.Setenv("HC_TERMINAL_SOCKET", owner)
	log := autoTmuxFixture(t, "%7:123:0:fixture:/invented/aiq run codex --long")
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws := t.TempDir()
	if err := st.AddAccount(state.Account{ID: "codex/fixture", Provider: "codex", Name: "fixture", Enabled: true, Native: true, Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	id, err := st.AddLease(state.Lease{AccountID: "codex/fixture", Mode: state.ModeLong, Workspace: ws, Pane: "%7", PID: os.Getpid(), Hostname: pool.Hostname(), StartedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: config.Default(), st: st}
	target, attached, err := a.longExisting(ws)
	target.unlock()
	if err != nil || !attached || target.native {
		t.Fatalf("existing tmux lost precedence: %v %v", attached, err)
	}
	if calls.Load() != 0 {
		t.Fatal("existing owner triggered unrelated native probe")
	}
	b, err := os.ReadFile(log)
	if err != nil || !strings.Contains(string(b), "attach-session") || strings.Contains(string(b), "kill-session") {
		t.Fatal("existing tmux was not attached safely", err)
	}
	l, err := st.GetLease(id)
	if err != nil || l.Pane != "%7" || l.PID != os.Getpid() {
		t.Fatal("existing lease changed", err)
	}
}

func TestAutomaticBackendRespectsUncertainNativeOwner(t *testing.T) {
	clearLongContext(t)
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	t.Setenv("HC_TERMINAL_ID", "t1")
	t.Setenv("HC_TERMINAL_SOCKET", "/invented/owner")
	log := autoTmuxFixture(t, "")
	ws := t.TempDir()
	r := nativeRecordFixture(t, ws)
	st, err := state.Open(paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &app{cfg: config.Default(), st: st}
	target, attached, err := a.longExisting(ws)
	target.unlock()
	if err == nil || attached || !strings.Contains(err.Error(), "ownership is uncertain") {
		t.Fatalf("uncertain owner ignored: %v %v", attached, err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("uncertain owner reached tmux")
	}
	if got, _, _ := herdr.Workspace(ws); got.Ref != r.Ref || got.Generation != r.Generation {
		t.Fatal("native generation changed")
	}
}

func TestAutomaticNativeReservesUndiscoverableTmuxLease(t *testing.T) {
	for _, fixture := range []string{"missing server", "missing executable", "foreign host", "unknown host", "unknown PID", "negative PID", "proven dead"} {
		t.Run(fixture, func(t *testing.T) {
			clearLongContext(t)
			t.Setenv("AIQ_DATA_DIR", t.TempDir())
			owner, calls := autoCoreEndpoint(t, "protocol")
			t.Setenv("HC_TERMINAL_ID", "t1")
			t.Setenv("HC_TERMINAL_SOCKET", owner)
			log := autoTmuxFixture(t, "")
			host, pid := pool.Hostname(), os.Getpid()
			switch fixture {
			case "foreign host":
				host = "fixture-other-host"
			case "unknown host":
				host = ""
			case "unknown PID":
				pid = 0
			case "negative PID":
				pid = -1
			case "proven dead":
				pid = 2147483647
			}
			if fixture == "missing executable" {
				t.Setenv("PATH", t.TempDir())
			}
			st, err := state.Open(paths.StateDB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			ws := t.TempDir()
			if err := st.AddAccount(state.Account{ID: "codex/fiction", Provider: "codex", Name: "fiction", Enabled: true, Native: true, Home: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			id, err := st.AddLease(state.Lease{AccountID: "codex/fiction", Mode: state.ModeLong, Workspace: ws, Pane: "%7", PID: pid, Hostname: host, StartedAt: time.Now().Unix()})
			if err != nil {
				t.Fatal(err)
			}
			before, err := st.GetLease(id)
			if err != nil {
				t.Fatal(err)
			}
			a := &app{cfg: config.Default(), st: st}
			target, attached, err := a.longExisting(ws)
			target.unlock()
			if err == nil || attached || target.native {
				t.Fatalf("lease admitted a second target: %v %v", attached, err)
			}
			if fixture == "proven dead" {
				if strings.Contains(err.Error(), "tmux ownership is uncertain") || calls.Load() != 1 {
					t.Fatal("proven-dead lease blocked native verification")
				}
			} else if !strings.Contains(err.Error(), "tmux ownership is uncertain") || calls.Load() != 0 {
				t.Fatal("uncertain lease reached native admission")
			}
			after, err := st.GetLease(id)
			if err != nil || before != after {
				t.Fatal("lease changed during refusal", err)
			}
			launches, err := st.ListLaunches("", nil, nil)
			if err != nil || len(launches) != 0 {
				t.Fatal("refusal started a provider", err)
			}
			if _, owned, err := herdr.Workspace(ws); owned || err != nil {
				t.Fatal("refusal published native ownership", err)
			}
			b, _ := os.ReadFile(log)
			for _, call := range strings.Fields(string(b)) {
				if call != "has-session" {
					t.Fatal("refusal mutated tmux")
				}
			}
		})
	}
}

func TestAutomaticNativeInvalidContextPreservesTmuxRecovery(t *testing.T) {
	for _, user := range []bool{false, true} {
		t.Run(map[bool]string{false: "dead only", true: "dead and user"}[user], func(t *testing.T) {
			clearLongContext(t)
			t.Setenv("AIQ_DATA_DIR", t.TempDir())
			t.Setenv("HC_TERMINAL_ID", "t1")
			t.Setenv("HC_TERMINAL_SOCKET", "")
			panes := "%7:123:1:fixture:/invented/aiq run codex --long"
			if user {
				panes += "\n%8:456:0:fixture:sleep 30"
			}
			log := autoTmuxFixture(t, panes)
			st, err := state.Open(paths.StateDB())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			a := &app{cfg: config.Default(), st: st}
			target, attached, err := a.longExisting(t.TempDir())
			target.unlock()
			if err == nil || attached {
				t.Fatal("invalid context admitted a new session")
			}
			b, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			kills := strings.Count(string(b), "kill-session\n")
			if (user && kills != 0) || (!user && kills != 1) {
				t.Fatalf("wrong stale cleanup with user=%v: %s", user, b)
			}
			if strings.Contains(string(b), "new-session") || strings.Contains(string(b), "respawn-pane") {
				t.Fatal("invalid context started a provider")
			}
		})
	}
}

func TestMenuLongUsesContextWithoutBackendFlag(t *testing.T) {
	clearLongContext(t)
	t.Setenv("HC_TERMINAL_ID", "t1")
	for _, session := range []string{"new", "resume"} {
		c := defaultChoice()
		c.Length, c.Session = "long", session
		args := menuArgs(c, testMenuEnv())
		backend, _, err := longBackendArgs(args[1:])
		if err != nil || args[0] != "long" || backend != "" || longBackendForContext(backend) != "herdr" {
			t.Fatalf("menu pinned backend: %v %v", args, err)
		}
	}
	c := defaultChoice()
	c.Length = "long"
	m := &menu{c: c, env: testMenuEnv(), focus: "length", w: 80, h: 24}
	var text strings.Builder
	for _, line := range m.render() {
		for _, part := range line {
			text.WriteString(part.text)
		}
		text.WriteByte('\n')
	}
	if !strings.Contains(text.String(), "Herdr") || !strings.Contains(text.String(), "Killing") || strings.Contains(text.String(), "outlives") {
		t.Fatal("menu misstates long lifetime")
	}
}
