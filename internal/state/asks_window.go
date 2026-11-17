package state

import (
	"context"
	"database/sql"
	"math"
)

// AskIndex is what deciding an ask's section and order needs: no text.
type AskIndex struct {
	ID, RaisedAt     int64
	State            AskState
	AsksReader, Risk float64
}

// AsksWindow is one committed view of the asks store: every ask's index
// entry (newest raised first), the watcher report, and the full rows chosen
// from that index, in the chosen order. Full rows stop at the first one that
// would pass the text allowance; Complete says whether all chosen rows fit.
type AsksWindow struct {
	Index         []AskIndex
	ReportPresent bool
	Status        AsksStatus
	Items         []Ask
	Complete      bool
}

// AsksWindowLimits bound one window: Rows is the most index entries admitted
// (more refuses the read), TextBytes the text of the full rows returned (more
// ends the rows early, without refusing).
type AsksWindowLimits struct{ Rows, TextBytes int64 }

// ReadAsksWindow reads the index, lets choose pick which asks to return in
// full, and reads those, all in one read-only transaction. It initializes and
// repairs nothing. A store without asks tables is an error (the caller tells
// "not set up" from a missing file). Its caller owns opening and deadlines.
func (s *Store) ReadAsksWindow(ctx context.Context, limits AsksWindowLimits, choose func([]AskIndex) []int64) (*AsksWindow, error) {
	if ctx == nil || s == nil || s.db == nil || limits.Rows <= 0 || limits.TextBytes <= 0 || choose == nil {
		return nil, ErrAsksSnapshotInvalid
	}
	out, err := s.readAsksWindow(ctx, limits, choose)
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

func (s *Store) readAsksWindow(ctx context.Context, limits AsksWindowLimits, choose func([]AskIndex) []int64) (*AsksWindow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var count int64
	// This first executed table read pins the SQLite view.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks`).Scan(&count); err != nil {
		return nil, err
	}
	if count < 0 || count > limits.Rows {
		return nil, ErrAsksSnapshotTooLarge
	}
	if err := asksSnapshotTypes(ctx, tx, "asks", "1", "state", "id,raised_at", "asks_reader,risk", ""); err != nil {
		return nil, err
	}
	out := &AsksWindow{Index: make([]AskIndex, 0, count), Status: AsksStatus{NotWatched: []string{}}, Items: []Ask{}, Complete: true}
	rows, err := tx.QueryContext(ctx, `SELECT id, raised_at, state, asks_reader, risk FROM asks ORDER BY raised_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	if err := asksSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var v AskIndex
		if err := row.Scan(&v.ID, &v.RaisedAt, &v.State, &v.AsksReader, &v.Risk); err != nil {
			return err
		}
		if math.IsNaN(v.Risk) || math.IsInf(v.Risk, 0) || math.IsNaN(v.AsksReader) || math.IsInf(v.AsksReader, 0) || len(v.State) > 64 {
			return ErrAsksSnapshotUnavailable
		}
		out.Index = append(out.Index, v)
		return nil
	}); err != nil {
		return nil, err
	}
	if int64(len(out.Index)) != count {
		return nil, ErrAsksSnapshotUnavailable
	}
	if err := asksSnapshotTypes(ctx, tx, "asks_watch", "id = 1", "last_error,not_watched", "pass_at,ok_at,watched,waiting,next_try_at,error_at", "", "watched,waiting"); err != nil {
		return nil, err
	}
	var reports int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks_watch WHERE id = 1`).Scan(&reports); err != nil || reports > 1 {
		return nil, ErrAsksSnapshotUnavailable
	}
	if reports == 1 {
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT length(CAST(last_error AS BLOB)) + length(CAST(not_watched AS BLOB)) FROM asks_watch WHERE id = 1`).Scan(&size); err != nil || size < 0 || size > limits.TextBytes {
			return nil, ErrAsksSnapshotTooLarge
		}
		var notWatched string
		st := &out.Status
		if err := tx.QueryRowContext(ctx, `SELECT pass_at,ok_at,watched,waiting,next_try_at,error_at,last_error,not_watched FROM asks_watch WHERE id = 1`).Scan(
			&st.PassAt, &st.OkAt, &st.Watched, &st.Waiting, &st.NextTryAt, &st.ErrorAt, &st.LastError, &notWatched); err != nil {
			return nil, err
		}
		if st.NotWatched, err = asksSnapshotStrings(notWatched); err != nil {
			return nil, err
		}
		out.ReportPresent = true
	}
	const text = "length(CAST(chain AS BLOB))+length(CAST(workspace AS BLOB))+length(CAST(provider AS BLOB))+length(CAST(session_id AS BLOB))+length(CAST(state AS BLOB))+length(CAST(state_reason AS BLOB))+length(CAST(state_text AS BLOB))+length(CAST(first_quote AS BLOB))+length(CAST(last_quote AS BLOB))+length(CAST(refs AS BLOB))"
	remaining := limits.TextBytes
	for _, id := range choose(out.Index) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := asksSnapshotTypes(ctx, tx, "asks", "id = ?", "chain,workspace,provider,session_id,state,state_reason,state_text,first_quote,last_quote,refs", "id,raised_at,state_at,repeats,last_seen,repeat_at", "asks_reader,risk", "repeats", id); err != nil {
			return nil, err
		}
		var size int64
		if err := tx.QueryRowContext(ctx, `SELECT `+text+` FROM asks WHERE id = ?`, id).Scan(&size); err == sql.ErrNoRows {
			return nil, ErrAsksSnapshotUnavailable // chosen from this same view
		} else if err != nil {
			return nil, err
		}
		if size < 0 || size > remaining {
			out.Complete = false
			break
		}
		remaining -= size
		var a Ask
		var refs string
		if err := tx.QueryRowContext(ctx, `SELECT `+askColumns+`, (SELECT COALESCE(MAX(id),0) FROM asks_events WHERE ask_id = asks.id) FROM asks WHERE id = ?`, id).Scan(
			&a.ID, &a.Chain, &a.Workspace, &a.Provider, &a.SessionID, &a.RaisedAt, &a.State, &a.StateAt,
			&a.StateReason, &a.StateText, &a.FirstQuote, &a.LastQuote, &a.AsksReader, &a.Risk, &refs, &a.Repeats, &a.LastSeen, &a.RepeatAt, &a.ActionSequence); err != nil {
			return nil, err
		}
		if a.Refs, err = asksSnapshotStrings(refs); err != nil {
			return nil, err
		}
		out.Items = append(out.Items, a)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, ctx.Err()
}
