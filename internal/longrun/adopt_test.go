package longrun

import (
	"fmt"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

func fakeEnviron(envs map[int]map[string]string) func(int) (map[string]string, error) {
	return func(pid int) (map[string]string, error) {
		if env, ok := envs[pid]; ok {
			return env, nil
		}
		return nil, fmt.Errorf("no process %d", pid)
	}
}

func TestFindOrphans(t *testing.T) {
	long := func(lease string) map[string]string {
		return map[string]string{"AIQ_LONG": "1", "AIQ_LEASE": lease, "AIQ_PROVIDER": "codex", "AIQ_ACCOUNT": "codex3"}
	}
	panes := []tmux.Pane{
		{Session: "aiq-ops2-3bd1", ID: "%80", PID: 80}, // orphan
		{Session: "aiq-ops-9d58", ID: "%97", PID: 97},  // its lease is held
		{Session: "aiq-ops3-d787", ID: "%93", PID: 93}, // its pane is watched under another id
		{Session: "aiq-ops4-b53c", ID: "%98", PID: 98, Dead: true},
		{Session: "aiq-ops5-0000", ID: "%99", PID: 99}, // not a long session
		{Session: "fleet", ID: "%5", PID: 5},           // not aiq's
		{Session: "aiq-ops6-1111", ID: "%70", PID: 70}, // process gone
	}
	envs := map[int]map[string]string{
		80: long("22"), 97: long("28"), 93: long("25"), 98: long("26"), 99: {"AIQ_LEASE": "30"}, 5: long("31"),
	}
	leases := []state.Lease{
		{ID: 28, Hostname: pool.Hostname(), Mode: state.ModeWorker},
		{ID: 40, Hostname: pool.Hostname(), Mode: state.ModeLong, Pane: "%93"},
	}
	got := FindOrphans("aiq", panes, leases, fakeEnviron(envs))
	if len(got) != 1 || got[0].Pane.ID != "%80" || got[0].LeaseID != 22 || got[0].Provider != "codex" || got[0].Account != "codex3" {
		t.Fatalf("got %+v; want only pane %%80 under lease 22", got)
	}
}

func TestOrphanLeaseComesFromItsLaunch(t *testing.T) {
	s, st := newSupervisor(t, 10, false)
	ws := t.TempDir() // not a git repository: the workspace is the directory
	other := t.TempDir()
	host := pool.Hostname()
	t0 := time.Now().Add(-9 * 24 * time.Hour)
	add := func(l state.Launch) {
		t.Helper()
		if _, err := st.AddLaunch(l); err != nil {
			t.Fatal(err)
		}
	}
	// The id was used before, by another account and in another workspace.
	add(state.Launch{StartedAt: t0, Hostname: host, Provider: "claude", AccountID: "claude/other", Cwd: ws, Mode: state.ModeLong, LeaseID: 22})
	add(state.Launch{StartedAt: t0.Add(time.Hour), Hostname: host, Provider: "codex", AccountID: "codex/codex3", Cwd: other, Mode: state.ModeLong, LeaseID: 22})
	add(state.Launch{StartedAt: t0.Add(2 * time.Hour), Hostname: host, Provider: "codex", AccountID: "codex/codex3", Cwd: ws, Mode: state.ModeLong,
		LeaseID: 22, SessionID: "thread-1", Args: []string{"--model", "gpt-5.6-sol", "--yolo"}})

	now := time.Now()
	o := Orphan{
		Pane: tmux.Pane{Session: SessionName("aiq", ws), ID: "%80", PID: 455204,
			StartCommand: "/x/aiq run codex --long --account codex3 --takeover 3 --fallback codex,claude,codex --resume-session thread-1 --"},
		LeaseID: 22, Provider: "codex", Account: "codex3",
	}
	l, err := s.orphanLease(o, now)
	if err != nil {
		t.Fatal(err)
	}
	if l.ID != 22 || l.AccountID != "codex/codex3" || l.Workspace != ws || l.Pane != "%80" || l.PID != 455204 ||
		l.SessionID != "thread-1" || l.Provider != "codex" || l.Fallback != "codex,claude,codex" || l.Mode != state.ModeLong ||
		l.Args != `["--model","gpt-5.6-sol","--yolo"]` || !l.InTurn() {
		t.Fatalf("got %+v", l)
	}
	if _, err := st.AddLease(l); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetLease(22); err != nil || got.Pane != "%80" {
		t.Fatalf("lease 22 after adopt: %+v, %v", got, err)
	}

	o.Pane.Session = "aiq-elsewhere-0000"
	if _, err := s.orphanLease(o, now); err == nil {
		t.Fatal("adopted a pane whose session name matches no launch's workspace")
	}
}
