package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quotaFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	if err := st.AddAccount(Account{ID: "claude/one", Provider: "claude", Name: "one", Enabled: true, Home: "/invented/home", QuotaID: "invented-quota", Identity: "one@invented.test", Priority: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetUsageMeta("claude/one", "max", 1, "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.SetExtraUsage("claude/one", &ExtraUsage{Enabled: true, Currency: "USD", BalanceMinor: 500, ObservedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWindow(Window{AccountID: "claude/one", Key: "five_hour", Kind: KindShort, UsedPct: 12.5, Source: "probe", ObservedAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	id, err := st.AddLease(Lease{AccountID: "claude/one", Hostname: "here", PID: 2, Mode: ModeLong, Cwd: "/invented/cwd", Args: "invented secret prompt", Transcript: "/invented/transcript.jsonl", Fallback: "codex", StartedAt: now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLeaseWhy(id, now.Unix(), 2, "invented why"); err != nil {
		t.Fatal(err)
	}
	return path
}

func readQuota(t *testing.T, path string, ctx context.Context, l QuotaSnapshotLimits) (*QuotaSnapshot, error) {
	t.Helper()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	return ro.ReadQuotaSnapshot(ctx, l)
}

func TestQuotaSnapshotReadsWithoutHiddenLeaseText(t *testing.T) {
	path := quotaFixture(t)
	s, err := readQuota(t, path, context.Background(), QuotaSnapshotLimits{Rows: 100, TextBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Accounts) != 1 || s.Accounts[0].Home != "/invented/home" || len(s.Windows["claude/one"]) != 1 || s.Windows["claude/one"][0].UsedPct != 12.5 {
		t.Fatalf("accounts/windows: %+v %+v", s.Accounts, s.Windows)
	}
	u, ok := s.Usage["claude/one"]
	if !ok || u.Plan != "max" || u.ResetCredits != 1 || u.Extra == nil || u.Extra.BalanceMinor != 500 {
		t.Fatalf("usage: %+v", u)
	}
	if len(s.Leases) != 1 {
		t.Fatal("lease count")
	}
	l := s.Leases[0]
	if l.Args != "" || l.Transcript != "" || l.Fallback != "" || l.Cwd != "/invented/cwd" {
		t.Fatalf("lease carries hidden text: %+v", l)
	}
	if s.Whys[LeaseRef{ID: l.ID, StartedAt: l.StartedAt}] != "invented why" {
		t.Fatal("why")
	}
}

// Admission refuses before selecting: too many rows, or too much stored text,
// even when the excess is in a column the snapshot never returns to callers.
func TestQuotaSnapshotAdmission(t *testing.T) {
	path := quotaFixture(t)
	// 1 account + 1 usage + 1 window + 1 lease.
	if _, err := readQuota(t, path, context.Background(), QuotaSnapshotLimits{Rows: 4, TextBytes: 1 << 16}); err != nil {
		t.Fatalf("exact row budget: %v", err)
	}
	if _, err := readQuota(t, path, context.Background(), QuotaSnapshotLimits{Rows: 3, TextBytes: 1 << 16}); !errors.Is(err, ErrQuotaSnapshotTooLarge) {
		t.Fatalf("rows over budget: %v", err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUsageMeta("claude/one", "max", 1, strings.Repeat("e", 8192), time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := readQuota(t, path, context.Background(), QuotaSnapshotLimits{Rows: 100, TextBytes: 4096}); !errors.Is(err, ErrQuotaSnapshotTooLarge) {
		t.Fatalf("text over budget: %v", err)
	}
	for _, l := range []QuotaSnapshotLimits{{0, 1}, {1, 0}, {-1, 1}} {
		if _, err := readQuota(t, path, context.Background(), l); !errors.Is(err, ErrQuotaSnapshotInvalid) {
			t.Fatalf("limits %+v: %v", l, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, err := readQuota(t, path, ctx, QuotaSnapshotLimits{Rows: 100, TextBytes: 1 << 16}); s != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v %v", s, err)
	}
}

// LeaseGone is the test PruneLeases applies, so a read-only caller sees what
// pruning would keep.
func TestLeaseGoneMatchesPrune(t *testing.T) {
	st := openTemp(t)
	old := time.Now().Add(-MaxLeaseAge - time.Hour).Unix()
	st.AddLease(Lease{AccountID: "claude/a", Hostname: "here", PID: 1, Mode: ModeLong, StartedAt: old})
	st.AddLease(Lease{AccountID: "claude/a", Hostname: "here", PID: 2, Mode: ModeLong, StartedAt: time.Now().Unix()})
	st.AddLease(Lease{AccountID: "claude/a", Hostname: "there", PID: 1, Mode: ModeLong, StartedAt: old})
	st.AddLease(Lease{AccountID: "claude/a", Hostname: "there", PID: 1, Mode: ModeLong, StartedAt: time.Now().Unix()})
	alive := func(pid int) bool { return pid == 1 }
	all, err := st.ListLeases()
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-MaxLeaseAge).Unix()
	var keep []int64
	for _, l := range all {
		if !LeaseGone(l, "here", alive, cutoff) {
			keep = append(keep, l.ID)
		}
	}
	kept, err := st.PruneLeases("here", alive)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != len(keep) || len(keep) != 2 {
		t.Fatalf("LeaseGone keeps %v, PruneLeases kept %d", keep, len(kept))
	}
	for i, l := range kept {
		if l.ID != keep[i] {
			t.Fatalf("LeaseGone keeps %v, PruneLeases kept %+v", keep, kept)
		}
	}
}
