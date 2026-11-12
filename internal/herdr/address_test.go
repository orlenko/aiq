package herdr

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type addressRequest struct {
	Method string `json:"method"`
	Params struct {
		Terminal string `json:"terminal"`
	} `json:"params"`
}

func addressEndpoint(t *testing.T, generation int, id, address, state string, pid int) (string, func() []addressRequest) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "aiqh-address-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	socket := filepath.Join(root, "core")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []addressRequest
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(time.Second))
				var q addressRequest
				if json.NewDecoder(c).Decode(&q) != nil {
					return
				}
				mu.Lock()
				calls = append(calls, q)
				mu.Unlock()
				var result any
				switch q.Method {
				case "hello":
					result = map[string]any{"generation": generation, "core": map[string]any{"id": id}}
				case "terminal.get":
					result = map[string]any{"address": func() string {
						if address == "" {
							return q.Params.Terminal
						}
						return address
					}(), "state": state, "pid": pid}
				default:
					t.Errorf("unexpected fixture method %q", q.Method)
				}
				json.NewEncoder(c).Encode(map[string]any{"id": 1, "result": result})
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
	return socket, func() []addressRequest { mu.Lock(); defer mu.Unlock(); return append([]addressRequest(nil), calls...) }
}
func TestCoreTerminalAddressShape(t *testing.T) {
	for _, s := range []string{"t1", "t19", "t1b", "t2z", "t100", "t9bcdfghjkmnpqrstvwxyz"} {
		if !terminalID.MatchString(s) {
			t.Errorf("rejected core address %q", s)
		}
	}
	for _, s := range []string{"", "t", "t0", "t01", "tfoo", "t1a", "t1e", "t1i", "t1l", "t1o", "t1u", "T1", "t1B", " t1", "t1 ", "t1\n", "t1\x00", "t1é", "t1-b", "remote/t1", "t1/x", "t1:2", "t1@2"} {
		if terminalID.MatchString(s) {
			t.Errorf("accepted noncanonical shape %q", s)
		}
	}
}
func TestCoreVerifyPinsAlphanumericIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket identity fixture")
	}
	for _, tc := range []struct {
		name, id, address, state string
		generation, pid          int
		ok                       bool
	}{
		{"matching", "fixture-core", "t1b", "live", 1, 123, true},
		{"changed core", "other-core", "t1b", "live", 1, 123, false},
		{"changed address", "fixture-core", "t2z", "live", 1, 123, false},
		{"changed PID", "fixture-core", "t1b", "live", 1, 456, false},
		{"not live", "fixture-core", "t1b", "finished", 1, 123, false},
		{"protocol", "fixture-core", "t1b", "live", 2, 123, false},
		{"empty core", "", "t1b", "live", 1, 123, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket, calls := addressEndpoint(t, tc.generation, tc.id, tc.address, tc.state, tc.pid)
			c := Core{Socket: socket, ID: "fixture-core", Terminal: "t1b", PID: 123}
			if err := c.verify(); (err == nil) != tc.ok {
				t.Fatalf("verify success=%t want %t", err == nil, tc.ok)
			}
			got := calls()
			if len(got) == 0 || got[0].Method != "hello" {
				t.Fatal("missing hello")
			}
			if len(got) > 1 && (got[1].Method != "terminal.get" || got[1].Params.Terminal != "t1b") {
				t.Fatal("lost pinned terminal identity")
			}
		})
	}
}
func TestAlphanumericOwnershipDescriptorRoundTrip(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	ws := t.TempDir()
	ref, err := newRef(ws)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{Ref: ref, Workspace: ws, RunnerPID: os.Getpid(), PID: 123, Generation: 7, State: "running", Socket: filepath.Join(ws, "missing-control"), LeaseID: 17, LaunchID: 23, Fallback: "claude,codex", Core: Core{Socket: filepath.Join(ws, "missing-core"), ID: "invented-core", Terminal: "t1b", PID: 456}}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadKey(workspaceKey(ws))
	if err != nil || !reflect.DeepEqual(loaded, r) {
		t.Fatal("saved identity changed", err)
	}
	got, owned, err := Workspace(ws)
	if err == nil || !owned || !reflect.DeepEqual(got, r) {
		t.Fatal("unreachable alphanumeric ownership became vacancy", owned, err)
	}
	records, err := Records()
	if err != nil || len(records) != 1 || !reflect.DeepEqual(records[0], r) {
		t.Fatal("listing lost exact address", err)
	}
	r.State = "ended"
	r.PID = 0
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	got, owned, err = Workspace(ws)
	if err != nil || owned || !reflect.DeepEqual(got, r) {
		t.Fatal("ended tombstone did not retain exact identity", owned, err)
	}
	r.Core.Terminal = "remote/t1b"
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	if _, owned, err := Workspace(ws); err == nil || !owned {
		t.Fatal("malformed descriptor became vacancy")
	}
}

// A prefix or selector can match the lexical envelope but must not acquire
// another terminal's canonical identity.
func TestAlphanumericCurrentCoreStages(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("direct native terminal requires Unix")
	}
	for _, tc := range []struct {
		name, requested, address, id, state, marker string
		generation, pid, calls                      int
		progress                                    bool
	}{
		{name: "alphanumeric progression", requested: "t1b", address: "t1b", id: "fiction-core", state: "live", generation: 1, pid: os.Getpid(), calls: 2, progress: true},
		{name: "prefix canonical mismatch", requested: "t1", address: "t1b", id: "fiction-core", state: "live", generation: 1, pid: os.Getpid(), calls: 2},
		{name: "not live", requested: "t1b", address: "t1b", id: "fiction-core", state: "finished", generation: 1, pid: os.Getpid(), calls: 2},
		{name: "nonpositive PID", requested: "t1b", address: "t1b", id: "fiction-core", state: "live", generation: 1, pid: 0, calls: 2},
		{name: "negative PID", requested: "t1b", address: "t1b", id: "fiction-core", state: "live", generation: 1, pid: -1, calls: 2},
		{name: "wrong protocol", requested: "t1b", address: "t1b", id: "fiction-core", state: "live", generation: 2, pid: os.Getpid(), calls: 1},
		{name: "empty core", requested: "t1b", address: "t1b", state: "live", generation: 1, pid: os.Getpid(), calls: 1},
		{name: "invalid shape", requested: "tfoo", calls: 0},
		{name: "TMUX", requested: "t1b", marker: "TMUX", calls: 0},
		{name: "TMUX_PANE", requested: "t1b", marker: "TMUX_PANE", calls: 0},
		{name: "STY", requested: "t1b", marker: "STY", calls: 0},
		{name: "relative endpoint", requested: "t1b", calls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket, calls := addressEndpoint(t, tc.generation, tc.id, tc.address, tc.state, tc.pid)
			for _, key := range []string{"TMUX", "TMUX_PANE", "STY"} {
				t.Setenv(key, "")
			}
			if tc.marker != "" {
				t.Setenv(tc.marker, "fixture")
			}
			if tc.name == "relative endpoint" {
				socket = "relative-socket"
			}
			t.Setenv("HC_SOCKET", socket)
			t.Setenv("HC_TERMINAL_ID", tc.requested)
			c, err := CurrentCore()
			got := calls()
			if len(got) != tc.calls {
				t.Fatalf("RPC count=%d want%d", len(got), tc.calls)
			}
			if len(got) > 1 && (got[1].Method != "terminal.get" || got[1].Params.Terminal != tc.requested) {
				t.Fatal("request did not preserve exact terminal")
			}
			if !tc.progress {
				expected := "Herdr terminal is not live"
				if tc.calls == 0 {
					expected = "native Herdr sessions need a direct Herdr-core terminal; open one, or use hc run --cwd <workspace> -- aiq long --backend herdr <provider>"
				}
				if tc.calls == 1 {
					expected = "unsupported Herdr core protocol (tested with hc 0.1.20)"
				}
				if err == nil || err.Error() != expected {
					t.Fatalf("wrong discovery failure stage: got %v, want %q", err, expected)
				}
				return
			}
			// Piped tests have no controlling TTY: this asserts progression, not
			// positive admission. Native PTY acceptance is tested separately.
			tty, ttyErr := processTTY(os.Getpid())
			if ttyErr != nil || tty == "" || tty == "?" || tty == "??" {
				if err == nil || !(strings.Contains(err.Error(), "TTY") || strings.Contains(err.Error(), "terminal ownership")) {
					t.Fatal("did not reach the expected TTY guard", err)
				}
			} else if err != nil || c.Terminal != tc.requested || c.PID != os.Getpid() {
				t.Fatal("matching controlled identity rejected", err)
			}
		})
	}
}
