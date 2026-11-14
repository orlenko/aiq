package state

import (
	"database/sql"
	"errors"
)

// LeaseTarget is the read-only identity/display projection for an attachment.
// It deliberately contains no launch arguments or transcript locations.
type LeaseTarget struct {
	ID, StartedAt                              int64
	AccountID, Hostname, Mode, Workspace, Pane string
	PID                                        int
}

var (
	ErrLeaseTargetNotFound    = errors.New("lease target not found")
	ErrLeaseTargetUnavailable = errors.New("lease target unavailable")
)

func (s *Store) GetLeaseTarget(id int64) (LeaseTarget, error) {
	var t LeaseTarget
	err := s.db.QueryRow(`SELECT id, COALESCE(started_at,0), COALESCE(account_id,''),
		COALESCE(hostname,''), COALESCE(mode,''), COALESCE(workspace,''),
		COALESCE(pane,''), COALESCE(pid,0) FROM leases WHERE id = ?`, id).
		Scan(&t.ID, &t.StartedAt, &t.AccountID, &t.Hostname, &t.Mode, &t.Workspace, &t.Pane, &t.PID)
	if errors.Is(err, sql.ErrNoRows) {
		return LeaseTarget{}, ErrLeaseTargetNotFound
	}
	if err != nil {
		return LeaseTarget{}, ErrLeaseTargetUnavailable
	}
	return t, nil
}
