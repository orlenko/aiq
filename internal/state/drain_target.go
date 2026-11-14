package state

import (
	"errors"
	"time"
)

// RequestDrainTarget admits a copied command against one unchanged primary
// lease. prepare runs only after the matching UPDATE holds SQLite's writer
// lock, before the request becomes visible. It must do bounded metadata work,
// never read/redact a note or wait on a process. A mismatch never calls it.
func (s *Store) RequestDrainTarget(target LeaseTarget, now time.Time, prepare func() error) (bool, error) {
	if s == nil || s.db == nil || prepare == nil || target.ID <= 0 || target.StartedAt <= 0 || target.PID <= 0 || target.Hostname == "" || target.AccountID == "" || target.Workspace == "" || target.Mode != ModeLong {
		return false, errors.New("invalid drain target")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE leases SET drain=?,drain_at=?,drain_manual=1
 WHERE id=? AND started_at=? AND pid=? AND COALESCE(hostname,'')=? AND COALESCE(account_id,'')=?
 AND COALESCE(mode,'')=? AND COALESCE(workspace,'')=? AND COALESCE(pane,'')=? AND COALESCE(depth,0)=0`,
		DrainRequested, now.Unix(), target.ID, target.StartedAt, target.PID, target.Hostname, target.AccountID, target.Mode, target.Workspace, target.Pane)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 0 {
		return false, nil
	}
	if count != 1 {
		return false, errors.New("ambiguous drain target")
	}
	if err := prepare(); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
