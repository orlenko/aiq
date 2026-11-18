//go:build unix

package herdr

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// targetCore is an invented core answering hello and terminal.get only.
type targetCore struct {
	id, address, state string
	pid                int
	labels             map[string]string
	createdMS          int64
	onGet              func()
}

func serveTargetCore(t *testing.T, socket string, core *targetCore) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
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
				var q struct {
					Method string `json:"method"`
				}
				if json.NewDecoder(c).Decode(&q) != nil {
					return
				}
				var result any
				switch q.Method {
				case "hello":
					result = map[string]any{"generation": 1, "core": map[string]any{"id": core.id}}
				case "terminal.get":
					if core.onGet != nil {
						core.onGet()
					}
					result = map[string]any{"address": core.address, "state": core.state, "pid": core.pid, "labels": core.labels, "created_ms": core.createdMS}
				default:
					t.Errorf("target check sent %q; it must stay read-only", q.Method)
				}
				json.NewEncoder(c).Encode(map[string]any{"id": 1, "result": result})
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); wg.Wait() })
}

func TestWitnessLabelPerGeneration(t *testing.T) {
	ref := Prefix + "0123456789abcdef0123456789abcdef-0123456789abcdef0123456789abcdef"
	a, b := WitnessLabel(ref, 1), WitnessLabel(ref, 2)
	if len(a) != 32 || a == b || a != WitnessLabel(ref, 1) {
		t.Fatalf("labels %q %q", a, b)
	}
	if a == WitnessLabel(Prefix+"1123456789abcdef0123456789abcdef-0123456789abcdef0123456789abcdef", 1) {
		t.Fatal("two terminals share a witness")
	}
}

func TestResolveTarget(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "aiqh-target-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	if err := os.MkdirAll(directory(), 0700); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		name       string
		record     func(*Record)
		core       func(*targetCore)
		generation uint64
		reason     string
		legacy     bool
		replaced   bool
	}{
		{name: "verified", generation: 2},
		{name: "kept replacement shell keeps the label, not the pid", core: func(c *targetCore) { c.pid = 999 }, generation: 2, reason: TargetEnded},
		{name: "label missing", core: func(c *targetCore) { c.labels = map[string]string{"aiq.status": "idle"} }, generation: 2, reason: TargetLabelMissing},
		{name: "legacy terminal", generation: 2, legacy: true},
		{name: "legacy terminal rolls over during verification", generation: 2, legacy: true, replaced: true, reason: TargetUnknown},
		{name: "other generation's label", core: func(c *targetCore) {
			c.labels[TargetLabel] = "0123456789abcdef0123456789abcdef"
			c.createdMS = 123456
		}, generation: 2, reason: TargetLabelMissing},
		{name: "another core at the socket", core: func(c *targetCore) { c.id = "other-core" }, generation: 2, reason: TargetUnknown},
		{name: "terminal finished", core: func(c *targetCore) { c.state = "finished" }, generation: 2, reason: TargetEnded},
		{name: "child replaced since", generation: 1, reason: TargetEnded},
		{name: "not registered", record: func(r *Record) { r.State = "starting" }, generation: 2, reason: TargetUnknown},
		{name: "ended", record: func(r *Record) { r.State = "ended" }, generation: 2, reason: TargetEnded},
		{name: "bare family", generation: 0, reason: TargetUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := filepath.Join(root, "workspace-"+strconv.Itoa(i))
			ref, err := newRef(ws)
			if err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(root, "core-"+strconv.Itoa(i))
			r := Record{Ref: ref, Workspace: ws, Socket: filepath.Join(root, "control-"+strconv.Itoa(i)), RunnerPID: os.Getpid(), PID: 123, Generation: 2, State: "running",
				Core: Core{Socket: socket, ID: "fixture-core", Terminal: "t7", PID: 456}}
			if tc.record != nil {
				tc.record(&r)
			}
			if err := save(r); err != nil {
				t.Fatal(err)
			}
			core := &targetCore{id: "fixture-core", address: "t7", state: "live", pid: 456,
				labels: map[string]string{"aiq.status": "idle", TargetLabel: WitnessLabel(ref, 2)}}
			if tc.core != nil {
				tc.core(core)
			}
			want := Target{CoreID: core.id, Terminal: "t7", PID: 456, Label: WitnessLabel(ref, 2)}
			if tc.legacy {
				delete(core.labels, TargetLabel)
				core.createdMS = 123456
				want.Label, want.CreatedMS = "", core.createdMS
			}
			if tc.replaced {
				core.onGet = func() {
					replacement := r
					replacement.Generation++
					if err := save(replacement); err != nil {
						t.Error(err)
					}
				}
			}
			serveTargetCore(t, socket, core)
			target := ref
			if tc.generation > 0 {
				target += "@" + strconv.FormatUint(tc.generation, 10)
			}
			got, reason := ResolveTarget(target)
			if reason != tc.reason {
				t.Fatalf("reason %q, want %q (%+v)", reason, tc.reason, got)
			}
			if reason == "" && got != want {
				t.Fatalf("target %+v, want %+v", got, want)
			}
			if reason != "" && got != (Target{}) {
				t.Fatalf("unverified target leaked %+v", got)
			}
		})
	}
}

// The runner's label write is the only Herdr mutation here: one
// terminal.label call, on the child's own terminal, merging the witness that
// verifyTarget then accepts.
func TestLabelTargetWritesTheVerifiedWitness(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "aiqh-label-")
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
	var methods []string
	labels := map[string]string{"user": "kept"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			var q struct {
				Method string `json:"method"`
				Params struct {
					Terminal string            `json:"terminal"`
					Labels   map[string]string `json:"labels"`
				} `json:"params"`
			}
			c.SetDeadline(time.Now().Add(time.Second))
			if json.NewDecoder(c).Decode(&q) == nil {
				mu.Lock()
				methods = append(methods, q.Method)
				var result any = map[string]any{}
				switch q.Method {
				case "hello":
					result = map[string]any{"generation": 1, "core": map[string]any{"id": "fixture-core"}}
				case "terminal.label":
					if q.Params.Terminal != "t7" {
						t.Errorf("labelled terminal %q", q.Params.Terminal)
					}
					for k, v := range q.Params.Labels {
						labels[k] = v
					}
				case "terminal.get":
					result = map[string]any{"address": "t7", "state": "live", "pid": 456, "labels": labels}
				default:
					t.Errorf("unexpected %q", q.Method)
				}
				mu.Unlock()
				json.NewEncoder(c).Encode(map[string]any{"id": 1, "result": result})
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	ref := Prefix + "0123456789abcdef0123456789abcdef-0123456789abcdef0123456789abcdef"
	c := Core{Socket: socket, ID: "fixture-core", Terminal: "t7", PID: 456}
	if err := c.labelTarget(ref, 3); err != nil {
		t.Fatal(err)
	}
	got, reason := verifyTarget(c, WitnessLabel(ref, 3))
	if reason != "" || got.Label != WitnessLabel(ref, 3) || got.PID != 456 || got.Terminal != "t7" {
		t.Fatalf("labelled target did not verify: %+v %q", got, reason)
	}
	mu.Lock()
	defer mu.Unlock()
	if labels["user"] != "kept" {
		t.Fatal("label write dropped an existing label")
	}
	for _, m := range methods {
		if m != "hello" && m != "terminal.label" && m != "terminal.get" {
			t.Fatalf("label write sent %q", m)
		}
	}
}
