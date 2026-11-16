package state

import (
	"context"
	"database/sql"
)

// AskCount is how many open Asks one conversation raised in one workspace.
// It carries identifiers only: no quotes, state text, references or chains.
type AskCount struct {
	Provider, SessionID, Workspace string
	Count                          int64
}

// ReadOpenAskCounts counts open Asks per provider, session and workspace from
// one committed read-only view, without initializing or repairing anything.
// Limits bound the open rows and the identifier bytes admitted; over either,
// the whole read is refused. A database without Asks tables has no open Asks.
// Its caller owns Store opening, concurrency and deadlines.
func (s *Store) ReadOpenAskCounts(ctx context.Context, limits AsksSnapshotLimits) ([]AskCount, error) {
	if ctx == nil || s == nil || s.db == nil || limits.Rows <= 0 || limits.TextBytes <= 0 {
		return nil, ErrAsksSnapshotInvalid
	}
	out, err := s.readOpenAskCounts(ctx, limits)
	if ctx.Err() != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, context.DeadlineExceeded
		}
		return nil, context.Canceled
	}
	if err != nil {
		if err == ErrAsksSnapshotTooLarge {
			return nil, err
		}
		return nil, ErrAsksSnapshotUnavailable // Never wrap a driver cause.
	}
	return out, nil
}

func (s *Store) readOpenAskCounts(ctx context.Context, limits AsksSnapshotLimits) ([]AskCount, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var present bool
	// This first executed read pins the SQLite view.
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'asks')`).Scan(&present); err != nil {
		return nil, err
	}
	if !present {
		return []AskCount{}, nil
	}
	const open = "state = '" + string(AskOpen) + "'"
	var count, text int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(length(CAST(provider AS BLOB)) + length(CAST(session_id AS BLOB)) + length(CAST(workspace AS BLOB))), 0) FROM asks WHERE `+open).Scan(&count, &text); err != nil {
		return nil, err
	}
	if count < 0 || count > limits.Rows || text < 0 || text > limits.TextBytes {
		return nil, ErrAsksSnapshotTooLarge
	}
	if err := asksSnapshotTypes(ctx, tx, "asks", open, "provider,session_id,workspace", "", "", ""); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT provider, session_id, workspace, COUNT(*) FROM asks WHERE `+open+` GROUP BY provider, session_id, workspace ORDER BY provider, session_id, workspace`)
	if err != nil {
		return nil, err
	}
	out := []AskCount{}
	err = asksSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var v AskCount
		if err := row.Scan(&v.Provider, &v.SessionID, &v.Workspace, &v.Count); err != nil {
			return err
		}
		if v.Count <= 0 || !navigatorTextValid(v.Provider, v.SessionID, v.Workspace) {
			return ErrAsksSnapshotUnavailable
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
