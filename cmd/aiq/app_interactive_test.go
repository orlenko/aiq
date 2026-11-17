package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/state"
)

const (
	appInterClaudeID = "c3000000-1111-4222-8333-000000000003"
	appInterCodexID  = "d4000000-1111-4222-8333-000000000004"
)

// appInteractiveFixture adds to appTestEnv: an interactive Claude lease (8,
// pid 2001) and an interactive Codex lease (9, pid 3001), both recorded in
// terminal shell 1400; an interactive Claude lease with no record (10, pid
// 4001); and Claude's own registration for pid 2001.
type appInteractiveFixture struct {
	env   appEnv
	x     *navAppFixture
	procs appProcs
	terms map[int64]state.LeaseTerminal
	reg   string
}

func newAppInteractiveFixture(t *testing.T) *appInteractiveFixture {
	t.Helper()
	env, x := appTestEnv(t)
	now := time.Now().Truncate(time.Second)
	db, err := sql.Open("sqlite", filepath.Join(x.root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, l := range []struct {
		id      int64
		account string
		pid     int
	}{{8, "claude/invented", 2001}, {9, "codex/invented", 3001}, {10, "claude/invented", 4001}} {
		if _, err := db.Exec(`INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at) VALUES(?,?,?,'invented-host','interactive','/invented/elsewhere',0,?)`, l.id, l.account, l.pid, now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	f := &appInteractiveFixture{env: env, x: x}
	started := now.Add(-10 * time.Second)
	f.procs = appProcs{1400: {ppid: 1, start: started}, 1500: {ppid: 1400, start: started}, 2001: {ppid: 1500, start: started},
		3001: {ppid: 1400, start: started}, 4001: {ppid: 1400, start: started}}
	f.terms = map[int64]state.LeaseTerminal{
		8: {LeaseID: 8, PID: 2001, StartedAt: now.Unix(), Socket: "/invented/core.sock", Core: "invented-core", Address: "t3", TerminalPID: 1400},
		9: {LeaseID: 9, PID: 3001, StartedAt: now.Unix(), Socket: "/invented/core.sock", Core: "invented-core", Address: "t4", TerminalPID: 1400},
	}
	f.env.leaseTerminals = func(_ context.Context, host string) (map[int64]state.LeaseTerminal, error) {
		if host != "invented-host" {
			t.Errorf("terminals read for host %q", host)
		}
		return f.terms, nil
	}
	f.env.procs = func(context.Context) (appProcs, error) { return f.procs, nil }
	f.env.verifyTerminal = func(r state.LeaseTerminal) (herdr.Target, string) {
		if r.Socket != "/invented/core.sock" || r.TerminalPID != 1400 {
			return herdr.Target{}, herdr.TargetUnknown
		}
		return herdr.Target{CoreID: r.Core, Terminal: r.Address, PID: int(r.TerminalPID)}, ""
	}
	dir := filepath.Join(x.root, "real-claude", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.reg = filepath.Join(dir, "2001.json")
	f.register(t, `{"pid":2001,"sessionId":"`+appInterClaudeID+`","entrypoint":"cli"}`)
	return f
}

func (f *appInteractiveFixture) register(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(f.reg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *appInteractiveFixture) conversation(t *testing.T, provider, id string) (map[string]any, string) {
	t.Helper()
	got := appData(t, appCall(t, f.env, "target", "--provider", provider, "--conversation", id))
	return func() map[string]any {
		term, _ := got["terminal"].(map[string]any)
		return term
	}(), got["terminal_reason"].(string)
}

func TestAppInteractiveClaudeTarget(t *testing.T) {
	f := newAppInteractiveFixture(t)
	term, reason := f.conversation(t, "claude", appInterClaudeID)
	if reason != "" || term["address"] != "t3" || term["core_id"] != "invented-core" || term["relation"] != "origin" {
		t.Fatalf("interactive Claude target: %v %q", term, reason)
	}
	witness := term["witness"].(map[string]any)
	if witness["terminal_pid"].(float64) != 1400 || witness["label"] != "" {
		t.Fatal("interactive witness is the root pid only", witness)
	}
	// Leases are still matched as before.
	if term, reason := f.conversation(t, "claude", fullIDFixtureID); reason != "" || term["address"] != "t7" {
		t.Fatal("long target changed", term, reason)
	}
}

func TestAppInteractiveRefusals(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(*testing.T, *appInteractiveFixture)
		want   string
	}{
		{"sdk registration", func(t *testing.T, f *appInteractiveFixture) {
			f.register(t, `{"pid":2001,"sessionId":"`+appInterClaudeID+`","entrypoint":"sdk-ts"}`)
		}, appNoLease},
		{"registration for another pid", func(t *testing.T, f *appInteractiveFixture) {
			f.register(t, `{"pid":2002,"sessionId":"`+appInterClaudeID+`"}`)
		}, appNoLease},
		{"registration older than the process", func(t *testing.T, f *appInteractiveFixture) {
			old := time.Now().Add(-time.Hour)
			os.Chtimes(f.reg, old, old)
		}, appNoLease},
		{"pid reused after the lease", func(t *testing.T, f *appInteractiveFixture) {
			f.procs[2001] = appProc{ppid: 1500, start: time.Now().Add(time.Minute)}
			later := time.Now().Add(time.Hour) // the reused process's own registration
			os.Chtimes(f.reg, later, later)
		}, appNoLease},
		{"terminal root not an ancestor", func(t *testing.T, f *appInteractiveFixture) {
			f.procs[1500] = appProc{ppid: 1, start: f.procs[1500].start}
		}, appEnded},
		{"core says ended", func(t *testing.T, f *appInteractiveFixture) {
			f.env.verifyTerminal = func(state.LeaseTerminal) (herdr.Target, string) { return herdr.Target{}, herdr.TargetEnded }
		}, appEnded},
		{"terminals unreadable", func(t *testing.T, f *appInteractiveFixture) {
			f.env.leaseTerminals = func(context.Context, string) (map[int64]state.LeaseTerminal, error) {
				return nil, state.ErrLeaseTerminalUnavailable
			}
		}, appUnknownTerm},
		{"process list unreadable", func(t *testing.T, f *appInteractiveFixture) {
			f.env.procs = func(context.Context) (appProcs, error) { return nil, errors.New("invented") }
		}, appUnknownTerm},
		{"two leases claim it", func(t *testing.T, f *appInteractiveFixture) {
			f.terms[10] = state.LeaseTerminal{LeaseID: 10, PID: 4001, Socket: "/invented/core.sock", Core: "invented-core", Address: "t5", TerminalPID: 1400}
			os.WriteFile(filepath.Join(filepath.Dir(f.reg), "4001.json"), []byte(`{"pid":4001,"sessionId":"`+appInterClaudeID+`"}`), 0o600)
		}, appAmbiguous},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newAppInteractiveFixture(t)
			c.change(t, f)
			if term, reason := f.conversation(t, "claude", appInterClaudeID); term != nil || reason != c.want {
				t.Fatalf("got %v %q, want %q", term, reason, c.want)
			}
		})
	}
}

func TestAppInteractiveLeasesWithoutConversations(t *testing.T) {
	f := newAppInteractiveFixture(t)
	// Codex: the terminal is known, the conversation never guessed.
	if term, reason := f.conversation(t, "codex", appInterCodexID); term != nil || reason != appNoLease {
		t.Fatal("Codex interactive conversation was linked", term, reason)
	}
	targets := f.env.targets(context.Background())
	if term, reason := targets.forLease(9); reason != "" || term.Address != "t4" {
		t.Fatal("Codex interactive lease terminal", term, reason)
	}
	if term, reason := targets.forLease(10); term != nil || reason != appInteractiveUnmapped {
		t.Fatal("unrecorded interactive lease", term, reason)
	}
	if term, reason := targets.forLease(7); reason != "" || term.Address != "t7" {
		t.Fatal("long lease", term, reason)
	}
	if term, reason := targets.forLease(99); term != nil || reason != appNoLease {
		t.Fatal("missing lease", term, reason)
	}
}

func TestAppParseProcs(t *testing.T) {
	procs, err := appParseProcs(strings.NewReader(strings.Join([]string{
		"    1     0 Mon Nov  3 09:05:01 2031",
		"  200     1 Tue Nov 18 10:00:00 2031",
		"  300   200 Tue Nov 18 10:00:02 2031",
		"  bad   200 Tue Nov 18 10:00:02 2031",
		"  400   300 not a time",
	}, "\n")), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 3 || procs[300].ppid != 200 || procs[1].start != time.Date(2031, 11, 3, 9, 5, 1, 0, time.UTC) {
		t.Fatalf("%v", procs)
	}
	if !procs.ancestor(200, 300) || !procs.ancestor(300, 300) || procs.ancestor(300, 200) || procs.ancestor(1, 300) {
		t.Fatal("ancestry")
	}
	if !procs.sameLeaseProcess(300, procs[300].start.Unix()) || procs.sameLeaseProcess(300, procs[300].start.Unix()-5) || procs.sameLeaseProcess(999, 1) {
		t.Fatal("same lease process")
	}
	if got := procs.selfAndDescendants(200); len(got) != 2 || got[0] != 200 || got[1] != 300 {
		t.Fatal("descendants", got)
	}
}

// The real process listing parses on this OS: this test's own process is in
// it with its parent, started no later than now.
func TestAppProductionProcsListsThisProcess(t *testing.T) {
	if goos == "windows" {
		t.Skip("no ps")
	}
	procs, err := appProductionProcs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	me, ok := procs[os.Getpid()]
	if !ok || me.ppid != os.Getppid() || me.start.After(time.Now()) || time.Since(me.start) > time.Hour {
		t.Fatalf("this process: %+v %v (of %d)", me, ok, len(procs))
	}
	if !procs.sameLeaseProcess(os.Getpid(), time.Now().Unix()) || !procs.ancestor(os.Getppid(), os.Getpid()) {
		t.Fatal("own process checks")
	}
}
