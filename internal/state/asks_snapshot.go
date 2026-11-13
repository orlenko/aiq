package state

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

var (
	ErrAsksSnapshotInvalid     = errors.New("invalid asks snapshot request")
	ErrAsksSnapshotTooLarge    = errors.New("asks snapshot too large")
	ErrAsksSnapshotUnavailable = errors.New("asks snapshot unavailable")
)

// Required admission allowances, not production defaults or SQLite/heap bounds.
type AsksSnapshotLimits struct{ Rows, TextBytes int64 }

// ReportPresent describes stored watcher facts, independently of derived totals.
// Status.Skipped/CallsToday always mirror the corresponding snapshot totals.
type AsksSnapshot struct {
	Items               []Ask
	ReportPresent       bool
	Status              AsksStatus
	Skipped, CallsToday int64
	UTCDay              string
}

// ReadAsksSnapshot reads one committed database view without initializing or
// repairing anything. Its caller owns Store opening, concurrency and deadlines.
func (s *Store) ReadAsksSnapshot(ctx context.Context, now time.Time, limits AsksSnapshotLimits) (*AsksSnapshot, error) {
	if ctx == nil || s == nil || s.db == nil || limits.Rows <= 0 || limits.TextBytes <= 0 {
		return nil, ErrAsksSnapshotInvalid
	}
	out, err := s.readAsksSnapshot(ctx, AsksDay(now), limits)
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

func (s *Store) readAsksSnapshot(ctx context.Context, day string, limits AsksSnapshotLimits) (*AsksSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var count int64
	// This first executed table read pins the SQLite view, even when empty.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks`).Scan(&count); err != nil {
		return nil, err
	}
	if count > limits.Rows || count < 0 || uint64(count) > uint64(^uint(0)>>1) {
		return nil, ErrAsksSnapshotTooLarge
	}
	asksText := "chain,workspace,provider,session_id,state,state_reason,state_text,first_quote,last_quote,refs"
	reportText := "last_error,not_watched"
	for _, spec := range []struct {
		table, where, text, ints, reals, counts string
		args                                    []any
	}{
		{"asks", "1", asksText, "id,raised_at,state_at,repeats,last_seen,repeat_at", "asks_reader,risk", "repeats", nil},
		{"asks_watch", "id = 1", reportText, "pass_at,ok_at,watched,waiting,next_try_at,error_at", "", "watched,waiting", nil},
		{"asks_sessions", "1", "", "skipped", "", "skipped", nil},
		{"asks_usage", "day = ?", "", "calls", "", "calls", []any{day}},
	} {
		if err := asksSnapshotTypes(ctx, tx, spec.table, spec.where, spec.text, spec.ints, spec.reals, spec.counts, spec.args...); err != nil {
			return nil, err
		}
	}
	remaining := limits.TextBytes
	for _, spec := range []struct{ table, where, fields string }{
		{"asks", "1", asksText}, {"asks_watch", "id = 1", reportText},
	} {
		fields := strings.Split(spec.fields, ",")
		lengths, dest, expressions := make([]int64, len(fields)), make([]any, len(fields)), make([]string, len(fields))
		for i, name := range fields {
			dest[i] = &lengths[i]
			expressions[i] = "length(CAST(" + name + " AS BLOB))"
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(expressions, ",")+" FROM "+spec.table+" WHERE "+spec.where)
		if err != nil {
			return nil, err
		}
		if err := asksSnapshotRows(ctx, rows, func(row *sql.Rows) error {
			if err := row.Scan(dest...); err != nil {
				return err
			}
			for _, length := range lengths {
				if length < 0 || length > remaining {
					return ErrAsksSnapshotTooLarge
				}
				remaining -= length
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := &AsksSnapshot{Items: make([]Ask, 0), Status: AsksStatus{NotWatched: make([]string, 0)}, UTCDay: day}
	rows, err := tx.QueryContext(ctx, `SELECT `+askColumns+` FROM asks ORDER BY raised_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	if err := asksSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var a Ask
		var refs string
		if err := row.Scan(&a.ID, &a.Chain, &a.Workspace, &a.Provider, &a.SessionID, &a.RaisedAt, &a.State, &a.StateAt,
			&a.StateReason, &a.StateText, &a.FirstQuote, &a.LastQuote, &a.AsksReader, &a.Risk, &refs, &a.Repeats, &a.LastSeen, &a.RepeatAt); err != nil {
			return err
		}
		if math.IsNaN(a.Risk) || math.IsInf(a.Risk, 0) || math.IsNaN(a.AsksReader) || math.IsInf(a.AsksReader, 0) {
			return ErrAsksSnapshotUnavailable
		}
		var err error
		if a.Refs, err = asksSnapshotStrings(refs); err != nil {
			return err
		}
		out.Items = append(out.Items, a)
		return nil
	}); err != nil {
		return nil, err
	}
	var reports int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks_watch WHERE id = 1`).Scan(&reports); err != nil || reports > 1 {
		return nil, ErrAsksSnapshotUnavailable
	}
	if reports == 1 {
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
	// All contributors passed INTEGER/nonnegative checks in the same view.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(skipped),0) FROM asks_sessions`).Scan(&out.Skipped); err != nil {
		return nil, err
	}
	var days int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM asks_usage WHERE day = ?`, day).Scan(&days); err != nil || days > 1 {
		return nil, ErrAsksSnapshotUnavailable
	}
	if days == 1 {
		if err := tx.QueryRowContext(ctx, `SELECT calls FROM asks_usage WHERE day = ?`, day).Scan(&out.CallsToday); err != nil {
			return nil, err
		}
	}
	if out.Skipped < 0 || uint64(out.Skipped) > uint64(^uint(0)>>1) {
		return nil, ErrAsksSnapshotUnavailable
	}
	out.Status.Skipped, out.Status.CallsToday = int(out.Skipped), int(out.CallsToday)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Identifiers come only from the fixed specifications above, never stored input.
func asksSnapshotTypes(ctx context.Context, tx *sql.Tx, table, where, text, ints, reals, counts string, args ...any) error {
	var invalid []string
	for _, spec := range []struct{ fields, condition string }{
		{text, " <> 'text'"}, {ints, " <> 'integer'"}, {reals, " NOT IN ('integer','real')"},
	} {
		if spec.fields != "" {
			for _, name := range strings.Split(spec.fields, ",") {
				invalid = append(invalid, "typeof("+name+")"+spec.condition)
			}
		}
	}
	if counts != "" {
		for _, name := range strings.Split(counts, ",") {
			invalid = append(invalid, name+" < 0", name+" > "+strconv.FormatUint(uint64(^uint(0)>>1), 10))
		}
	}
	var bad bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE ("+where+") AND ("+strings.Join(invalid, " OR ")+"))", args...).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return ErrAsksSnapshotUnavailable
	}
	return nil
}

func asksSnapshotRows(ctx context.Context, rows *sql.Rows, consume func(*sql.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := consume(rows); err != nil {
			return err
		}
	}
	err, closed := rows.Err(), rows.Close()
	if err != nil {
		return err
	}
	return closed
}
