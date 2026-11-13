package state

import (
	"context"
	"database/sql"
	"errors"
)

var ErrAskPlacementMissing = errors.New("ask placement item missing")

// AskPlacement contains only the fields needed to find a transcript turn.
// It is never serialized as an HTTP response; quotes remain server-side.
type AskPlacement struct {
	Provider, SessionID     string
	RaisedAt                int64
	FirstQuote, LatestQuote string
}

// ReadAskPlacement reads exactly one row from one committed view without
// initializing the database. Admission precedes copying quote text into Go.
// The limit is UTF-8 bytes, not SQLite's internal memory or encoded response.
func (s *Store) ReadAskPlacement(ctx context.Context, id, textBytes int64) (AskPlacement, error) {
	if ctx == nil || s == nil || s.db == nil || id <= 0 || textBytes <= 0 {
		return AskPlacement{}, ErrAsksSnapshotInvalid
	}
	if err := ctx.Err(); err != nil {
		return AskPlacement{}, err
	}
	p, err := s.readAskPlacement(ctx, id, textBytes)
	if stopped := ctx.Err(); stopped != nil {
		return AskPlacement{}, stopped
	}
	if err != nil {
		if err == ErrAskPlacementMissing || err == ErrAsksSnapshotTooLarge {
			return AskPlacement{}, err
		}
		return AskPlacement{}, ErrAsksSnapshotUnavailable
	}
	return p, nil
}
func (s *Store) readAskPlacement(ctx context.Context, id, allowance int64) (AskPlacement, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AskPlacement{}, err
	}
	defer tx.Rollback()
	var sizes [4]int64
	err = tx.QueryRowContext(ctx, `SELECT length(CAST(COALESCE(provider,'') AS BLOB)),
	 length(CAST(COALESCE(session_id,'') AS BLOB)),length(CAST(COALESCE(first_quote,'') AS BLOB)),
	 length(CAST(COALESCE(last_quote,'') AS BLOB)) FROM asks WHERE id=?`, id).Scan(&sizes[0], &sizes[1], &sizes[2], &sizes[3])
	if errors.Is(err, sql.ErrNoRows) {
		return AskPlacement{}, ErrAskPlacementMissing
	}
	if err != nil {
		return AskPlacement{}, err
	}
	for _, n := range sizes {
		if n < 0 || n > allowance {
			return AskPlacement{}, ErrAsksSnapshotTooLarge
		}
		allowance -= n
	}
	var p AskPlacement
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(provider,''),COALESCE(session_id,''),COALESCE(raised_at,0),
	 COALESCE(first_quote,''),COALESCE(last_quote,'') FROM asks WHERE id=?`, id).
		Scan(&p.Provider, &p.SessionID, &p.RaisedAt, &p.FirstQuote, &p.LatestQuote)
	if err != nil {
		return AskPlacement{}, err
	}
	// Lengths and content are from the same read transaction. Verify the
	// driver conversion too, before returning any admitted fields.
	for i, text := range []string{p.Provider, p.SessionID, p.FirstQuote, p.LatestQuote} {
		if int64(len(text)) != sizes[i] {
			return AskPlacement{}, ErrAsksSnapshotUnavailable
		}
	}
	if err = tx.Commit(); err != nil {
		return AskPlacement{}, err
	}
	return p, nil
}
