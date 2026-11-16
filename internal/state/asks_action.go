package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

var ErrAskActionChanged = errors.New("ask changed; read it again")
var ErrAskActionUnavailable = errors.New("ask action unavailable")

// AskActionRevision describes the question and its actionable state. Observation
// bookkeeping alone does not invalidate a person's selection. Event sequence
// distinguishes a state which moved away and back in the same clock second.
func AskActionRevision(a Ask) string {
	a.LastSeen = 0
	// ActionSequence is deliberately included despite its json omission on Ask.
	b, err := json.Marshal(struct {
		Ask      Ask
		Sequence int64
	}{a, a.ActionSequence})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SetAskByPersonRevision updates an existing registry only. The writer takes its
// lock before comparing the supplied snapshot; watcher/person updates cannot
// replace that question between comparison, state write and event insertion.
func (s *Store) SetAskByPersonRevision(ctx context.Context, id int64, revision string, to AskState, now, textLimit int64) (Ask, error) {
	if ctx == nil || s == nil || s.db == nil || id <= 0 || textLimit <= 0 || len(revision) != 64 || strings.Trim(revision, "0123456789abcdef") != "" || to != AskOpen && to != AskDismissed {
		return Ask{}, ErrAskActionUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE asks SET id = id WHERE id = ?`, id); err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	// Bound materialized text before reading a row from the existing registry.
	var size int64
	if err = tx.QueryRowContext(ctx, `SELECT length(CAST(chain AS BLOB))+length(CAST(workspace AS BLOB))+length(CAST(provider AS BLOB))+length(CAST(session_id AS BLOB))+length(CAST(state AS BLOB))+length(CAST(state_reason AS BLOB))+length(CAST(state_text AS BLOB))+length(CAST(first_quote AS BLOB))+length(CAST(last_quote AS BLOB))+length(CAST(refs AS BLOB)) FROM asks WHERE id = ?`, id).Scan(&size); err != nil || size < 0 || size > textLimit {
		return Ask{}, ErrAskActionUnavailable
	}
	a, err := scanAsk(tx.QueryRowContext(ctx, `SELECT `+askColumns+` FROM asks WHERE id = ?`, id))
	if err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM asks_events WHERE ask_id = ?`, id).Scan(&a.ActionSequence); err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	if AskActionRevision(a) != revision {
		return Ask{}, ErrAskActionChanged
	}
	dismiss, restore := AskAction(a.State)
	if to == AskDismissed && !dismiss || to == AskOpen && !restore {
		return Ask{}, ErrAskActionChanged
	}
	if _, err = tx.ExecContext(ctx, `UPDATE asks SET state = ?, state_at = ?, state_reason = ? WHERE id = ?`, to, now, AskPerson, id); err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO asks_events (ask_id,at,from_st,to_st,reason) VALUES(?,?,?,?,?)`, id, now, a.State, to, AskPerson)
	if err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	a.ActionSequence, err = result.LastInsertId()
	if err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	a.State, a.StateAt, a.StateReason = to, now, AskPerson
	if err = tx.Commit(); err != nil {
		return Ask{}, ErrAskActionUnavailable
	}
	return a, nil
}
