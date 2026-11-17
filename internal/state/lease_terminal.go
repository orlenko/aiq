package state

import (
	"context"
	"database/sql"
	"errors"
)

// LeaseTerminal is the Herdr terminal an interactive session was started in,
// as aiq verified it at launch: the core's socket and ID, the terminal's
// address and its root process. It is a claim to re-verify on every read,
// never proof by itself that the session is still there.
type LeaseTerminal struct {
	LeaseID, PID, StartedAt int64
	Socket, Core, Address   string
	TerminalPID             int64
}

var ErrLeaseTerminalUnavailable = errors.New("lease terminals unavailable")

// Bounds on what one recorded terminal may hold.
const (
	leaseTerminalSocketBytes  = 1024
	leaseTerminalCoreBytes    = 128
	leaseTerminalAddressBytes = 64
)

// SetLeaseTerminal records t on an interactive lease, once. It changes
// nothing when the lease is gone, is another process's or another mode, or
// already has a terminal.
func (s *Store) SetLeaseTerminal(t LeaseTerminal) error {
	if s == nil || s.db == nil || t.LeaseID <= 0 || t.PID <= 0 || t.TerminalPID <= 0 || t.Socket == "" || t.Core == "" || t.Address == "" ||
		len(t.Socket) > leaseTerminalSocketBytes || len(t.Core) > leaseTerminalCoreBytes || len(t.Address) > leaseTerminalAddressBytes {
		return ErrLeaseTerminalUnavailable
	}
	_, err := s.db.Exec(`UPDATE leases SET term_socket = ?, term_core = ?, term_address = ?, term_pid = ?
		WHERE id = ? AND pid = ? AND mode = ? AND term_core IS NULL`, t.Socket, t.Core, t.Address, t.TerminalPID, t.LeaseID, t.PID, ModeInteractive)
	return err
}

// ReadLeaseTerminals reads the recorded terminals of this host's interactive
// leases in one read-only transaction, refusing more than rows of them or a
// value of the wrong type or size. A store from before these columns has none.
func (s *Store) ReadLeaseTerminals(ctx context.Context, hostname string, rows int64) (map[int64]LeaseTerminal, error) {
	if ctx == nil || s == nil || s.db == nil || rows <= 0 {
		return nil, ErrLeaseTerminalUnavailable
	}
	out, err := s.readLeaseTerminals(ctx, hostname, rows)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, ErrLeaseTerminalUnavailable
	}
	return out, nil
}

func (s *Store) readLeaseTerminals(ctx context.Context, hostname string, limit int64) (map[int64]LeaseTerminal, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := map[int64]LeaseTerminal{}
	var columns int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('leases') WHERE name IN ('term_socket','term_core','term_address','term_pid')`).Scan(&columns); err != nil {
		return nil, err
	}
	if columns != 4 {
		return out, tx.Commit()
	}
	const where = ` FROM leases WHERE hostname = ? AND mode = 'interactive' AND term_core IS NOT NULL`
	var count, bad int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+where, hostname).Scan(&count); err != nil {
		return nil, err
	}
	if count > limit {
		return nil, ErrLeaseTerminalUnavailable
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+where+` AND NOT (
		typeof(id) = 'integer' AND typeof(pid) = 'integer' AND typeof(started_at) = 'integer' AND typeof(term_pid) = 'integer'
		AND typeof(term_socket) = 'text' AND typeof(term_core) = 'text' AND typeof(term_address) = 'text'
		AND length(CAST(term_socket AS BLOB)) BETWEEN 1 AND ? AND length(CAST(term_core AS BLOB)) BETWEEN 1 AND ?
		AND length(CAST(term_address AS BLOB)) BETWEEN 1 AND ? AND pid > 0 AND term_pid > 0)`,
		hostname, leaseTerminalSocketBytes, leaseTerminalCoreBytes, leaseTerminalAddressBytes).Scan(&bad); err != nil {
		return nil, err
	}
	if bad != 0 {
		return nil, ErrLeaseTerminalUnavailable
	}
	rs, err := tx.QueryContext(ctx, `SELECT id, pid, started_at, term_socket, term_core, term_address, term_pid`+where, hostname)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	for rs.Next() {
		var t LeaseTerminal
		if err := rs.Scan(&t.LeaseID, &t.PID, &t.StartedAt, &t.Socket, &t.Core, &t.Address, &t.TerminalPID); err != nil {
			return nil, err
		}
		out[t.LeaseID] = t
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != count {
		return nil, ErrLeaseTerminalUnavailable
	}
	return out, tx.Commit()
}
