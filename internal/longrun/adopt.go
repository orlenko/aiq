package longrun

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

// Orphan is a live long-session pane with no lease: what the supervisor
// needs to take it back.
type Orphan struct {
	Pane    tmux.Pane
	LeaseID int64 // AIQ_LEASE, which the session's hooks report under
	// IDTaken: another lease holds LeaseID (it was reused while this
	// session's lease was gone). The session is adopted under a new id and
	// its hooks find it by pane.
	IDTaken  bool
	Provider string // AIQ_PROVIDER
	Account  string // AIQ_ACCOUNT, the account's name
}

// FindOrphans lists the panes of this host's long sessions (tmux sessions
// named prefix-…) that no lease watches, with the AIQ_LEASE their CLI runs
// under.
// Before leases on this host lived as long as their process, every long
// session lost its lease 36 hours after it started and ran unsupervised.
func FindOrphans(prefix string, panes []tmux.Pane, leases []state.Lease, environ func(pid int) (map[string]string, error)) []Orphan {
	held := map[int64]bool{}
	watched := map[string]bool{}
	for _, l := range leases {
		held[l.ID] = true
		if l.Hostname == pool.Hostname() && l.Pane != "" {
			watched[l.Pane] = true
		}
	}
	var out []Orphan
	for _, p := range panes {
		if p.Dead || p.PID <= 0 || watched[p.ID] || !strings.HasPrefix(p.Session, prefix+"-") {
			continue
		}
		env, err := environ(p.PID)
		if err != nil || env["AIQ_LONG"] != "1" {
			continue
		}
		id, _ := strconv.ParseInt(env["AIQ_LEASE"], 10, 64)
		if id <= 0 {
			continue
		}
		out = append(out, Orphan{Pane: p, LeaseID: id, IDTaken: held[id], Provider: env["AIQ_PROVIDER"], Account: env["AIQ_ACCOUNT"]})
	}
	return out
}

var fallbackFlag = regexp.MustCompile(`--fallback '?([a-z,]+)'?`)

// adoptOrphans gives every orphaned long session its lease back, under the
// id its hooks already carry, so drains and takeovers resume. The launch
// that started the pane's CLI supplies the rest; a pane without one is
// reported once and left alone.
func (s *Supervisor) adoptOrphans(leases []state.Lease, now time.Time) {
	if !tmux.Available() {
		return
	}
	panes, err := tmux.Panes()
	if err != nil {
		return
	}
	for _, o := range FindOrphans(s.Pool.Cfg.Long.TmuxPrefix, panes, leases, proc.Environ) {
		l, err := s.orphanLease(o, now)
		if err != nil {
			if s.unadoptable == nil {
				s.unadoptable = map[string]bool{}
			}
			if key := fmt.Sprintf("%s/%d", o.Pane.ID, o.LeaseID); !s.unadoptable[key] {
				s.unadoptable[key] = true
				s.Logf("pane %s (%s) runs a long session under lease %d, which is gone, and cannot be re-adopted: %v", o.Pane.ID, o.Pane.Session, o.LeaseID, err)
			}
			continue
		}
		id, err := s.Pool.St.AddLease(l)
		if err != nil {
			s.Logf("lease %d: re-adopt pane %s: %v", o.LeaseID, o.Pane.ID, err)
			continue
		}
		note := ""
		if id != o.LeaseID {
			note = fmt.Sprintf(" (its lease id %d is held by another lease)", o.LeaseID)
		}
		s.Pool.St.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf(
			"lease %d: re-adopted pane %s in %s%s; its lease had been dropped and the session ran unsupervised", id, o.Pane.ID, l.Workspace, note), now)
	}
}

// orphanLease rebuilds an orphan's lease from the newest long launch that
// recorded its lease id for the same account and workspace.
func (s *Supervisor) orphanLease(o Orphan, now time.Time) (state.Lease, error) {
	launches, err := s.Pool.St.LongLaunches(pool.Hostname(), o.LeaseID)
	if err != nil {
		return state.Lease{}, err
	}
	for _, la := range launches {
		if o.Provider != "" && o.Account != "" && la.AccountID != state.AccountID(o.Provider, o.Account) {
			continue
		}
		ws := s.workspaceOf(la.Cwd, o.Pane.Session)
		if ws == "" {
			continue
		}
		args := la.Args
		if args == nil {
			args = []string{}
		}
		enc, _ := json.Marshal(args)
		fallback := ""
		if m := fallbackFlag.FindStringSubmatch(o.Pane.StartCommand); m != nil {
			fallback = m[1]
		}
		id := o.LeaseID
		if o.IDTaken {
			id = 0
		}
		return state.Lease{
			ID: id, AccountID: la.AccountID, PID: o.Pane.PID, Hostname: pool.Hostname(), Mode: state.ModeLong,
			Cwd: la.Cwd, Args: string(enc), StartedAt: la.StartedAt.Unix(),
			Workspace: ws, Pane: o.Pane.ID, SessionID: la.SessionID, Provider: la.Provider, Fallback: fallback,
			Launcher: la.Launcher,
			// The turn state is unknown until the next hook. Assume a turn is
			// running, so a drain waits for the Stop hook instead of reading
			// the session as idle and cutting a turn in half.
			TurnStartedAt: now.Unix(),
		}, nil
	}
	return state.Lease{}, fmt.Errorf("no long launch on this host recorded lease %d for %s/%s in a workspace named %s", o.LeaseID, o.Provider, o.Account, o.Pane.Session)
}

// workspaceOf is the workspace a launch in cwd ran for (the git root, as
// aiq long computes it, else cwd), provided its tmux session name matches.
func (s *Supervisor) workspaceOf(cwd, session string) string {
	var cands []string
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		cands = append(cands, strings.TrimSpace(string(out)))
	}
	cands = append(cands, cwd)
	for _, ws := range cands {
		if ws != "" && SessionName(s.Pool.Cfg.Long.TmuxPrefix, ws) == session {
			return ws
		}
	}
	return ""
}
