package pool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
)

// viewFixture is an invented store: two providers, windows, extra usage, a CLI
// login problem, and leases that pruning keeps and drops.
func viewFixture(t *testing.T) (*Pool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, a := range []state.Account{
		{ID: "claude/one", Provider: "claude", Name: "one", Enabled: true, Home: home, Identity: "one@invented.test", Priority: 100},
		{ID: "claude/two", Provider: "claude", Name: "two", Enabled: true, Home: t.TempDir(), Identity: "two@invented.test", Priority: 100},
		{ID: "codex/three", Provider: "codex", Name: "three", Enabled: false, Home: t.TempDir(), Priority: 100},
	} {
		if err := st.AddAccount(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetUsageMeta("claude/one", "max", 2, "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetExtraUsage("claude/one", &state.ExtraUsage{Enabled: true, Currency: "USD", BalanceMinor: -1, UsedMinor: 250, LimitMinor: 1000, ObservedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCLILogin("claude/two", true, "someone-else@invented.test", now); err != nil {
		t.Fatal(err)
	}
	for _, w := range []state.Window{
		{AccountID: "claude/one", Key: "five_hour", Label: "5h", Kind: state.KindShort, UsedPct: 42, ResetsAt: now.Add(time.Hour).Unix(), WindowSeconds: 18000, Source: "probe", ObservedAt: now.Unix()},
		{AccountID: "claude/one", Key: "seven_day", Label: "7d", Kind: state.KindWeekly, UsedPct: -1, Source: "probe", ObservedAt: now.Unix()},
		{AccountID: "claude/two", Key: "five_hour", Label: "5h", Kind: state.KindShort, UsedPct: 97, Source: "probe", ObservedAt: now.Unix()},
	} {
		if err := st.UpsertWindow(w); err != nil {
			t.Fatal(err)
		}
	}
	host := Hostname()
	dead := 1
	for proc.Alive(dead) || dead == os.Getpid() {
		dead += 7919
	}
	for _, l := range []state.Lease{
		{AccountID: "claude/one", PID: os.Getpid(), Hostname: host, Mode: state.ModeLong, Cwd: "/invented/live", Workspace: "/invented/live", Pane: "%7", StartedAt: now.Unix() - 60},
		{AccountID: "claude/one", PID: dead, Hostname: host, Mode: state.ModeWorker, Cwd: "/invented/dead", StartedAt: now.Unix() - 30},
		{AccountID: "claude/two", PID: 5, Hostname: "invented-other", Mode: state.ModeInteractive, Cwd: "/invented/recent", StartedAt: now.Unix() - 10},
		{AccountID: "claude/two", PID: 6, Hostname: "invented-other", Mode: state.ModeInteractive, Cwd: "/invented/old", StartedAt: now.Add(-state.MaxLeaseAge - time.Hour).Unix()},
	} {
		id, err := st.AddLease(l)
		if err != nil {
			t.Fatal(err)
		}
		if l.Cwd == "/invented/live" {
			if err := st.SetLeaseWhy(id, l.StartedAt, l.PID, "waiting for an invented reason"); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &Pool{Cfg: config.Default(), St: st}, path
}

// A view assembled from a read-only snapshot, with leases filtered by
// LeaseGone, is the view View builds after pruning; reading it releases nothing.
func TestViewFromSnapshotMatchesView(t *testing.T) {
	p, path := viewFixture(t)
	ro, err := state.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	snap, err := ro.ReadQuotaSnapshot(context.Background(), state.QuotaSnapshotLimits{Rows: 1000, TextBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Leases) != 4 {
		t.Fatalf("snapshot leases = %d, want all 4", len(snap.Leases))
	}
	now := time.Now()
	f := &Facts{Now: now, Hostname: Hostname(), Accounts: snap.Accounts, Usage: snap.Usage, Windows: snap.Windows, Whys: snap.Whys,
		Scope: map[string]string{}, HasCredential: HasCredential}
	cutoff := now.Add(-state.MaxLeaseAge).Unix()
	for _, l := range snap.Leases {
		if !state.LeaseGone(l, f.Hostname, proc.Alive, cutoff) {
			f.Leases = append(f.Leases, l)
		}
	}
	for _, provider := range Providers {
		f.Scope[provider] = p.ModelScope(provider)
	}
	got := p.ViewFrom(f)
	if left, _ := p.St.ListLeases(); len(left) != 4 {
		t.Fatalf("snapshot read released leases: %d left", len(left))
	}
	want, err := p.View(0)
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := p.St.ListLeases(); len(left) != 2 {
		t.Fatalf("View pruned to %d leases, want 2", len(left))
	}
	got.GeneratedAt, want.GeneratedAt = 0, 0
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(want)
	if string(a) != string(b) {
		t.Fatalf("ViewFrom differs from View:\n got %s\nwant %s", a, b)
	}
	var leases, whys int
	for _, acc := range got.Accounts {
		for _, l := range acc.Leases {
			leases++
			if l.Why != "" {
				whys++
			}
		}
	}
	if leases != 2 || whys != 1 {
		t.Fatalf("leases %d whys %d, want 2 and 1", leases, whys)
	}
}
