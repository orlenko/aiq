package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// aiq long list shows the daemon's line under its lease, one reason a line,
// and says when no daemon keeps the lines current.
func TestLongListShowsWhy(t *testing.T) {
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	add := func(pane string) state.Lease {
		l := state.Lease{AccountID: "codex/acct-a", Mode: state.ModeLong, Hostname: "h", Provider: "codex", Pane: pane,
			Workspace: "/home/me/proj", PID: 7, StartedAt: time.Now().Unix()}
		id, err := st.AddLease(l)
		if err != nil {
			t.Fatal(err)
		}
		l.ID = id
		return l
	}
	l := add("%1")
	add("%2")
	st.SetLeaseWhy(l.ID, l.StartedAt, l.PID, "no account to move to; it keeps going on the 4% left.\ncodex: no other account\x1b]0;x\x07")
	out, err := stdoutOf(t, (&app{cfg: config.Default(), st: st}).longList)
	if err != nil {
		t.Fatal(err)
	}
	want := "      no account to move to; it keeps going on the 4% left.\n        codex: no other account?]0;x?\n2 "
	if !strings.Contains(out, want) || !strings.HasSuffix(out, "\n(the daemon is not running; these may be out of date)\n") ||
		strings.Count(out, "\n") != 6 {
		t.Fatalf("out =\n%s", out)
	}
}

// aiq long warns when it starts on an account the supervisor would drain.
func TestFloorWarning(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	for _, id := range []string{"codex/acct-a", "codex/acct-b", "codex/acct-c", "codex/acct-d", "codex/acct-e"} {
		if err := st.AddAccount(state.Account{ID: id, Provider: "codex", Name: id[6:], Enabled: true, Native: true}); err != nil {
			t.Fatal(err)
		}
	}
	window := func(id string, used float64) {
		st.ReplaceWindows(id, "test", []state.Window{{Key: "weekly", Label: "Weekly", Kind: state.KindWeekly,
			UsedPct: used, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}})
	}
	window("codex/acct-a", 97)
	window("codex/acct-b", 50)
	window("codex/acct-d", 100)
	window("codex/acct-e", 92)
	for _, id := range []string{"codex/acct-d", "codex/acct-e"} {
		st.SetUsageMeta(id, "", 0, "", now)
		st.SetExtraUsage(id, &state.ExtraUsage{Enabled: true, Currency: "CAD", BalanceMinor: 500, ObservedAt: now.Unix(), BalanceAt: now.Unix()})
	}
	cfg := config.Default()
	cfg.Providers.Codex.ModelScope = ""
	cfg.Selection.ExtraUsage = true
	a := &app{cfg: cfg, st: st, pool: &pool.Pool{Cfg: cfg, St: st}}
	if got := a.floorWarning("codex", "codex/acct-a", now); got != "aiq: codex/acct-a has 3% left, at or below the 4% drain floor: aiq moves this session to an account with more once one is free, after a handoff note if it has started work" {
		t.Errorf("at the floor: %q", got)
	}
	// The supervisor moves a session off extra usage once plan quota is free.
	if got := a.floorWarning("codex", "codex/acct-d", now); got != "aiq: codex/acct-d has 0% plan quota left and runs on extra usage: aiq moves this session to an account with more than 4% plan quota once one is free, after a handoff note if it has started work" {
		t.Errorf("on extra usage: %q", got)
	}
	for _, id := range []string{"codex/acct-b", "codex/acct-c", "codex/acct-e"} {
		if got := a.floorWarning("codex", id, now); got != "" {
			t.Errorf("%s: %q, want nothing", id, got)
		}
	}
	// The long floor decides the rung, as it does for the supervisor.
	cfg.Long.DrainPct = 10
	if got := a.floorWarning("codex", "codex/acct-e", now); !strings.HasPrefix(got, "aiq: codex/acct-e has 8% plan quota left and runs on extra usage: aiq moves this session to an account with more than 10% plan quota") {
		t.Errorf("drain_pct 10: %q", got)
	}
}
