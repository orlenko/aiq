package state

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A long session runs for days. Its lease must live as long as its process
// on this host; only a lease from a host aiq cannot probe ages out.
func TestPruneKeepsLiveLeasesOnThisHostWhateverTheirAge(t *testing.T) {
	st := openTemp(t)
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	live, _ := st.AddLease(Lease{AccountID: "claude/a", Hostname: "here", PID: 1, Mode: ModeLong, StartedAt: old})
	dead, _ := st.AddLease(Lease{AccountID: "claude/a", Hostname: "here", PID: 2, Mode: ModeLong, StartedAt: time.Now().Unix()})
	remote, _ := st.AddLease(Lease{AccountID: "claude/a", Hostname: "there", PID: 1, Mode: ModeLong, StartedAt: old})
	recent, _ := st.AddLease(Lease{AccountID: "claude/a", Hostname: "there", PID: 1, Mode: ModeLong, StartedAt: time.Now().Unix()})

	kept, err := st.PruneLeases("here", func(pid int) bool { return pid == 1 })
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, l := range kept {
		got[l.ID] = true
	}
	if !got[live] || !got[recent] || got[dead] || got[remote] {
		t.Fatalf("kept %v; want live %d and recent %d, not dead %d or remote %d", got, live, recent, dead, remote)
	}
}

func TestAddLeaseKeepsAnExplicitID(t *testing.T) {
	st := openTemp(t)
	id, err := st.AddLease(Lease{ID: 22, AccountID: "codex/c", Mode: ModeLong})
	if err != nil || id != 22 {
		t.Fatalf("got id %d, %v; want 22", id, err)
	}
	if _, err := st.AddLease(Lease{ID: 22, AccountID: "codex/d", Mode: ModeLong}); err == nil {
		t.Fatal("a second lease took id 22")
	}
	if l, err := st.GetLease(22); err != nil || l.AccountID != "codex/c" || l.RootID != 22 {
		t.Fatalf("got %+v, %v", l, err)
	}
	if next, err := st.AddLease(Lease{AccountID: "codex/e"}); err != nil || next == 22 {
		t.Fatalf("auto id %d, %v", next, err)
	}
}

func TestLongLaunchesNewestFirst(t *testing.T) {
	st := openTemp(t)
	t0 := time.Now().Add(-time.Hour)
	st.AddLaunch(Launch{StartedAt: t0, Hostname: "h", AccountID: "codex/a", Mode: ModeLong, LeaseID: 3, Cwd: "/old"})
	st.AddLaunch(Launch{StartedAt: t0.Add(time.Minute), Hostname: "h", AccountID: "codex/w", Mode: ModeWorker, LeaseID: 3})
	st.AddLaunch(Launch{StartedAt: t0.Add(2 * time.Minute), Hostname: "h", AccountID: "codex/b", Mode: ModeLong, LeaseID: 3, Cwd: "/new", Args: []string{"--yolo"}})
	st.AddLaunch(Launch{StartedAt: t0.Add(3 * time.Minute), Hostname: "other", AccountID: "codex/c", Mode: ModeLong, LeaseID: 3})
	got, err := st.LongLaunches("h", 3)
	if err != nil || len(got) != 2 || got[0].Cwd != "/new" || got[1].Cwd != "/old" || len(got[0].Args) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
