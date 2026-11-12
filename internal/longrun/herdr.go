package longrun

import (
	"encoding/json"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// Native adoption uses the immutable launch id registered before provider exec,
// together with the runner's original fallback and current process generation.
// It never needs a transcript, credential or another process's environment.
func (s *Supervisor) adoptHerdr(leases []state.Lease, now time.Time) {
	records, err := herdr.Records()
	if err != nil {
		if !s.herdrDiscoveryFailed {
			s.Logf("native Herdr discovery unavailable: %v", err)
		}
		s.herdrDiscoveryFailed = true
		return
	}
	s.herdrDiscoveryFailed = false
	watch := map[string]bool{}
	held := map[int64]bool{}
	for _, l := range leases {
		held[l.ID] = true
		if l.Hostname == pool.Hostname() {
			watch[l.Pane] = true
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, saved := range records {
		if watch[saved.Ref] {
			continue
		}
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		r, err := herdr.InfoWithin(saved.Ref, left)
		if err != nil || r.State != "running" || r.PID <= 0 || r.LeaseID <= 0 || r.LaunchID <= 0 {
			continue
		}
		launch, err := s.Pool.St.GetLaunch(r.LaunchID)
		if err != nil || launch.Hostname != pool.Hostname() || launch.Mode != state.ModeLong || launch.LeaseID != r.LeaseID {
			continue
		}
		args, _ := json.Marshal(launch.Args)
		id := r.LeaseID
		if held[id] {
			id = 0
		}
		l := state.Lease{ID: id, AccountID: launch.AccountID, PID: r.PID, Hostname: launch.Hostname, Mode: state.ModeLong, Cwd: launch.Cwd,
			Args: string(args), StartedAt: launch.StartedAt.Unix(), Workspace: r.Workspace, Pane: r.Ref, SessionID: launch.SessionID,
			Provider: launch.Provider, Fallback: r.Fallback, Launcher: launch.Launcher, TurnStartedAt: now.Unix()}
		if id, err := s.Pool.St.AddLease(l); err == nil {
			held[id] = true
			s.Logf("native Herdr lease %d re-adopted", id)
		}
	}
}
