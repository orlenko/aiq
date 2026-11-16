package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// Invented values only.

func TestCollectKeepsThisMachinesLiveLongAndInteractiveSessions(t *testing.T) {
	snap := &state.QuotaSnapshot{
		Accounts: []state.Account{{ID: "claude/a", Provider: "claude", Identity: "a@invented.test", Home: "/invented/home"}},
		Leases: []state.Lease{
			{ID: 1, AccountID: "claude/a", PID: 10, Hostname: "here", Mode: "long", Pane: "%1", Workspace: "/invented/w", StartedAt: 5, Args: "invented secret prompt"},
			{ID: 2, AccountID: "claude/a", PID: 11, Hostname: "here", Mode: "interactive", Cwd: "/invented/c", StartedAt: 6},
			{ID: 3, AccountID: "claude/a", PID: 12, Hostname: "here", Mode: "worker", StartedAt: 7},
			{ID: 4, AccountID: "claude/a", PID: 13, Hostname: "here", Mode: "long", StartedAt: 8}, // process gone
			{ID: 5, AccountID: "claude/a", PID: 10, Hostname: "elsewhere", Mode: "long", StartedAt: 9},
			{ID: 6, AccountID: "codex/gone", PID: 10, Hostname: "here", Mode: "long", Provider: "codex", StartedAt: 10},
		},
		Whys: map[state.LeaseRef]string{{ID: 1, StartedAt: 5}: "invented why\nsecond line"},
	}
	doc := Collect(snap, "here", func(pid int) bool { return pid != 13 }, 100)
	want := []Lease{
		{Account: "claude/a", Provider: "claude", Identity: "a@invented.test", Mode: "long", Pane: "%1", PID: 10, Workspace: "/invented/w", Why: "invented why\nsecond line", StartedAt: 5},
		{Account: "claude/a", Provider: "claude", Identity: "a@invented.test", Mode: "interactive", PID: 11, Cwd: "/invented/c", StartedAt: 6},
		{Account: "codex/gone", Provider: "codex", Mode: "long", PID: 10, StartedAt: 10},
	}
	if doc.Schema != Schema || doc.GeneratedAt != 100 || !reflect.DeepEqual(doc.Leases, want) {
		t.Fatalf("%+v", doc)
	}
	raw, _ := json.Marshal(doc)
	for _, hidden := range []string{"secret", "/invented/home", "elsewhere"} {
		if strings.Contains(string(raw), hidden) {
			t.Fatalf("reply carries %q: %s", hidden, raw)
		}
	}
	if got := Collect(nil, "here", nil, 1); got.Leases == nil || len(got.Leases) != 0 {
		t.Fatalf("empty: %+v", got)
	}
	// A reply decodes back to what was collected.
	back, err := Decode(raw)
	if err != nil || !reflect.DeepEqual(back, doc) {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestDecodeRefusals(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{"schema": 1, "generated_at": 1, "leases": []any{map[string]any{
			"account": "claude/a", "provider": "claude", "identity": "", "mode": "long", "drain": "", "depth": 0,
			"pane": "%1", "pid": 1, "workspace": "", "cwd": "", "why": "line\nline", "started_at": 1}}}
	}
	lease := func(d map[string]any) map[string]any { return d["leases"].([]any)[0].(map[string]any) }
	cases := map[string]func(map[string]any){
		"newer schema":      func(d map[string]any) { d["schema"] = 2 },
		"worker":            func(d map[string]any) { lease(d)["mode"] = "worker" },
		"no account":        func(d map[string]any) { lease(d)["account"] = "" },
		"negative pid":      func(d map[string]any) { lease(d)["pid"] = -1 },
		"control in pane":   func(d map[string]any) { lease(d)["pane"] = "%1\x1b[2J" },
		"newline in cwd":    func(d map[string]any) { lease(d)["cwd"] = "/a\n/b" },
		"line separator":    func(d map[string]any) { lease(d)["why"] = "a b" },
		"long text":         func(d map[string]any) { lease(d)["workspace"] = strings.Repeat("w", MaxTextBytes+1) },
		"string number":     func(d map[string]any) { lease(d)["pid"] = "1" },
		"too many sessions": func(d map[string]any) { d["leases"] = make([]any, MaxLeases+1) },
		"leases not a list": func(d map[string]any) { d["leases"] = "none" },
	}
	raw, _ := json.Marshal(good())
	if _, err := Decode(raw); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range cases {
		d := good()
		mutate(d)
		raw, _ := json.Marshal(d)
		if _, err := Decode(raw); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	for name, raw := range map[string]string{"not json": "aiq: unknown command", "trailing": `{"schema":1,"generated_at":1,"leases":[]} {}`,
		"empty": "", "too large": `{"schema":1,"leases":[],"pad":"` + strings.Repeat("x", MaxReplyBytes) + `"}`} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("%s: admitted", name)
		}
	}
	if d, err := Decode([]byte(`{"schema":1,"generated_at":1,"leases":null,"newer_field":true}`)); err != nil || d.Leases == nil {
		t.Fatalf("null leases or a newer field: %+v %v", d, err)
	}
}

func TestErrorLine(t *testing.T) {
	for in, want := range map[string]string{
		"\n  \nssh: connect to host invented port 22: Connection refused\r\nmore": "ssh: connect to host invented port 22: Connection refused",
		"\x1b[31mred\x1b[0m":     "[31mred[0m",
		strings.Repeat("é", 300): strings.Repeat("é", 200) + "…",
		"":                       "",
	} {
		if got := errorLine([]byte(in)); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func reply(leases ...Lease) []byte {
	if leases == nil {
		leases = []Lease{}
	}
	b, _ := json.Marshal(Doc{Schema: Schema, GeneratedAt: 1, Leases: leases})
	return b
}

func TestPollerStartsOnReadAndWaitsForTheFirstRound(t *testing.T) {
	var calls atomic.Int32
	run := func(ctx context.Context, host string) ([]byte, error) {
		calls.Add(1)
		switch host {
		case "invented-ok":
			return reply(Lease{Account: "claude/a", Provider: "claude", Mode: "long", PID: 1}), nil
		case "invented-slow":
			<-ctx.Done()
			return nil, errors.New("no answer in time")
		}
		return nil, errors.New("invented: Permission denied (publickey).")
	}
	p := NewPoller([]string{"invented-ok", "invented-bad", "invented-slow"}, run, time.Hour)
	p.timeout = 300 * time.Millisecond
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("asked before anyone read")
	}
	hosts := p.Hosts(context.Background(), 5*time.Second)
	got := [][2]string{}
	for _, h := range hosts {
		got = append(got, [2]string{h.Name, h.Status + ":" + h.Error})
	}
	want := [][2]string{{"invented-ok", "ok:"}, {"invented-bad", "error:invented: Permission denied (publickey)."}, {"invented-slow", "error:no answer in time"}}
	if !reflect.DeepEqual(got, want) || len(hosts[0].Leases) != 1 || hosts[0].SeenAt == 0 || hosts[1].SeenAt != 0 {
		t.Fatalf("%+v", hosts)
	}
	// The copy is the caller's.
	hosts[0].Leases[0].Account = "changed"
	if p.Hosts(context.Background(), 0)[0].Leases[0].Account != "claude/a" {
		t.Fatal("caller changed the poller's state")
	}
	if calls.Load() != 3 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestPollerWaitIsBoundedAndPendingShowsAsPending(t *testing.T) {
	release := make(chan struct{})
	p := NewPoller([]string{"invented-a"}, func(ctx context.Context, _ string) ([]byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return reply(), nil
	}, time.Hour)
	start := time.Now()
	hosts := p.Hosts(context.Background(), 50*time.Millisecond)
	if time.Since(start) > 2*time.Second || hosts[0].Status != Pending {
		t.Fatalf("%+v", hosts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if hosts := p.Hosts(ctx, time.Hour); hosts[0].Status != Pending {
		t.Fatalf("cancelled read: %+v", hosts)
	}
	close(release)
}

func TestPollerKeepsLeasesThroughAFailureAndStopsWhenIdle(t *testing.T) {
	var mu sync.Mutex
	fail, calls := false, 0
	p := NewPoller([]string{"invented-a"}, func(context.Context, string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if fail {
			return nil, errors.New("invented: Connection refused")
		}
		return reply(Lease{Account: "claude/a", Provider: "claude", Mode: "interactive", PID: 1}), nil
	}, time.Hour)
	p.interval = 10 * time.Millisecond
	p.idle = 200 * time.Millisecond
	first := p.Hosts(context.Background(), time.Second)[0]
	mu.Lock()
	fail = true
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	var h Host
	for time.Now().Before(deadline) {
		if h = p.Hosts(context.Background(), 0)[0]; h.Status == Failed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.Status != Failed || h.Error != "invented: Connection refused" || h.SeenAt != first.SeenAt || len(h.Leases) != 1 {
		t.Fatalf("after failure: %+v", h)
	}
	// No reads: the loop stops, and asks no more.
	time.Sleep(400 * time.Millisecond)
	p.mu.Lock()
	running := p.running
	p.mu.Unlock()
	mu.Lock()
	before := calls
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	after := calls
	mu.Unlock()
	if running || after != before {
		t.Fatalf("still polling: running=%v calls %d -> %d", running, before, after)
	}
	// The next read starts it again.
	p.Hosts(context.Background(), time.Second)
	mu.Lock()
	defer mu.Unlock()
	if calls == after {
		t.Fatal("did not restart")
	}
}

func TestPollerSurvivesAPanickingRunner(t *testing.T) {
	p := NewPoller([]string{"invented-a"}, func(context.Context, string) ([]byte, error) { panic("invented") }, time.Hour)
	h := p.Hosts(context.Background(), time.Second)[0]
	if h.Status != Failed || h.Error == "" {
		t.Fatalf("%+v", h)
	}
}

func TestSSHArgsEndOptionsBeforeTheHost(t *testing.T) {
	args := SSHArgs("invented-box", "~/.local/bin/aiq")
	n := len(args)
	if args[n-3] != "--" || args[n-2] != "invented-box" || args[n-1] != "~/.local/bin/aiq leases --json" {
		t.Fatal(args)
	}
	for _, want := range []string{"BatchMode=yes", "ClearAllForwardings=yes", "-n", "-T", "-a", "-x"} {
		found := false
		for _, a := range args {
			found = found || a == want
		}
		if !found {
			t.Errorf("missing %s", want)
		}
	}
}

// fakeSSH puts an ssh on PATH that runs body as a shell script.
func fakeSSH(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSSHRunner(t *testing.T) {
	t.Run("reply and arguments", func(t *testing.T) {
		fakeSSH(t, `for a in "$@"; do last="$a"; done; [ "$last" = "~/.local/bin/aiq leases --json" ] || exit 9; printf '%s' '{"schema":1,"generated_at":1,"leases":[]}'`)
		out, err := SSH("~/.local/bin/aiq")(context.Background(), "invented-box")
		if err != nil || string(out) != `{"schema":1,"generated_at":1,"leases":[]}` {
			t.Fatalf("%q %v", out, err)
		}
	})
	t.Run("stderr line", func(t *testing.T) {
		fakeSSH(t, `echo 'invented@invented-box: Permission denied (publickey).' >&2; exit 255`)
		_, err := SSH("aiq")(context.Background(), "invented-box")
		if err == nil || err.Error() != "invented@invented-box: Permission denied (publickey)." {
			t.Fatal(err)
		}
	})
	t.Run("silent failure", func(t *testing.T) {
		fakeSSH(t, `exit 3`)
		_, err := SSH("aiq")(context.Background(), "invented-box")
		if err == nil || err.Error() != "ssh exit status 3" {
			t.Fatal(err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		fakeSSH(t, `head -c 1100000 /dev/zero`)
		_, err := SSH("aiq")(context.Background(), "invented-box")
		if err == nil || err.Error() != "reply too large" {
			t.Fatal(err)
		}
	})
	t.Run("timeout kills it", func(t *testing.T) {
		fakeSSH(t, `exec sleep 30`)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := SSH("aiq")(ctx, "invented-box")
		if err == nil || err.Error() != "no answer in time" || time.Since(start) > 5*time.Second {
			t.Fatal(err, time.Since(start))
		}
	})
}
