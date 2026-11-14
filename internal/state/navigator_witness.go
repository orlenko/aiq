package state

import "context"

// NavigatorLeaseMatches rechecks a previously admitted snapshot's exact lease.
// It selects only a boolean, never materializes new stored text, and performs
// no write or migration. A false result does not establish that a process ended.
func (s *Store) NavigatorLeaseMatches(ctx context.Context, host string, v NavigatorLease) (match bool, err error) {
	if ctx == nil || s == nil || s.db == nil || host == "" || v.ID <= 0 {
		return false, ErrNavigatorSnapshotInvalid
	}
	defer func() {
		if ctx.Err() != nil {
			match = false
			err = ctx.Err()
		} else if err != nil {
			match = false
			err = ErrNavigatorSnapshotUnavailable
		}
	}()
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM leases WHERE hostname=? AND id=?
 AND COALESCE(started_at,0)=? AND COALESCE(pid,0)=? AND COALESCE(depth,0)=?
 AND COALESCE(turn_started_at,0)=? AND COALESCE(turn_ended_at,0)=?
 AND COALESCE(provider,'')=? AND COALESCE(account_id,'')=? AND COALESCE(launcher,'')=?
 AND COALESCE(cwd,'')=? AND COALESCE(mode,'')=? AND COALESCE(workspace,'')=?
 AND COALESCE(pane,'')=? AND COALESCE(session_id,'')=? AND COALESCE(drain,'')=?
 AND COALESCE(drain_manual,0)=?)`, host, v.ID, v.StartedAt, v.PID, v.Depth, v.TurnStartedAt, v.TurnEndedAt, v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.Workspace, v.Pane, v.SessionID, v.Drain, v.DrainManual).Scan(&match)
	return match, err
}
