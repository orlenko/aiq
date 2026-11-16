package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// AskState is where an ask stands. Nothing is ever deleted.
type AskState string

const (
	AskOpen         AskState = "open"
	AskProbablyDone AskState = "probably-done"
	AskClosed       AskState = "closed"
	AskDismissed    AskState = "dismissed"
)

// AskPerson is the reason on a state the person set, as opposed to a rule.
const AskPerson = "person"

// Ask is a stretch of an agent's own words that asks the person for something.
// Times are Unix seconds.
type Ask struct {
	// ActionSequence is snapshot metadata, not a schema column. The latest
	// state event prevents an old browser action surviving a dismiss/restore ABA.
	ActionSequence int64    `json:"-"`
	ID             int64    `json:"id"`
	Chain          string   `json:"chain"`
	Workspace      string   `json:"workspace"`
	Provider       string   `json:"provider"`
	SessionID      string   `json:"session_id"`
	RaisedAt       int64    `json:"raised_at"`
	State          AskState `json:"state"`
	StateAt        int64    `json:"state_at"`
	StateReason    string   `json:"state_reason"`
	StateText      string   `json:"state_text"`
	FirstQuote     string   `json:"first_quote"`
	LastQuote      string   `json:"last_quote"`
	AsksReader     float64  `json:"asks_reader"`
	Risk           float64  `json:"risk"`
	Refs           []string `json:"refs"`
	Repeats        int      `json:"repeats"`
	LastSeen       int64    `json:"last_seen"`
	// RepeatAt is when the agent itself last raised the item. LastSeen also counts text aiq
	// put in a turn, which must not hold back the agent's own repeat.
	RepeatAt int64 `json:"repeat_at"`
}

// asksSchema lives outside Open's schema, so a mismatch here can only fail the
// commands that use asks, never every aiq command.
const asksSchema = `
CREATE TABLE IF NOT EXISTS asks (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    chain        TEXT NOT NULL,
    workspace    TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL DEFAULT '',
    session_id   TEXT NOT NULL DEFAULT '',
    raised_at    INTEGER NOT NULL,
    state        TEXT NOT NULL,
    state_at     INTEGER NOT NULL,
    state_reason TEXT NOT NULL DEFAULT '',
    state_text   TEXT NOT NULL DEFAULT '',
    first_quote  TEXT NOT NULL,
    last_quote   TEXT NOT NULL,
    asks_reader  REAL NOT NULL DEFAULT 0,
    risk         REAL NOT NULL DEFAULT 0,
    refs         TEXT NOT NULL DEFAULT '[]',
    repeats      INTEGER NOT NULL DEFAULT 0,
    last_seen    INTEGER NOT NULL,
    repeat_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS asks_chain_state ON asks(chain, state);
CREATE INDEX IF NOT EXISTS asks_state_raised ON asks(state, raised_at);
CREATE UNIQUE INDEX IF NOT EXISTS asks_once ON asks(chain, raised_at, first_quote);
CREATE TABLE IF NOT EXISTS asks_events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    ask_id  INTEGER NOT NULL,
    at      INTEGER NOT NULL,
    from_st TEXT NOT NULL,
    to_st   TEXT NOT NULL,
    reason  TEXT NOT NULL DEFAULT '',
    text    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS asks_events_ask ON asks_events(ask_id, id);
CREATE TABLE IF NOT EXISTS asks_sessions (
    provider      TEXT NOT NULL,
    session_id    TEXT NOT NULL,
    chain         TEXT NOT NULL DEFAULT '',
    workspace     TEXT NOT NULL DEFAULT '',
    transcript    TEXT NOT NULL DEFAULT '',
    turn_started  INTEGER NOT NULL DEFAULT 0,
    items_done    INTEGER NOT NULL DEFAULT 0,
    reply_sha     TEXT NOT NULL DEFAULT '',
    skipped       INTEGER NOT NULL DEFAULT 0,
    attempts      INTEGER NOT NULL DEFAULT 0,
    done          INTEGER NOT NULL DEFAULT 0,
    not_before    INTEGER NOT NULL DEFAULT 0,
    gen           INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (provider, session_id)
);
CREATE TABLE IF NOT EXISTS asks_watch (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    pass_at       INTEGER NOT NULL DEFAULT 0,
    ok_at         INTEGER NOT NULL DEFAULT 0,
    watched       INTEGER NOT NULL DEFAULT 0,
    waiting       INTEGER NOT NULL DEFAULT 0,
    next_try_at   INTEGER NOT NULL DEFAULT 0,
    capped        INTEGER NOT NULL DEFAULT 0,
    error_at      INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT NOT NULL DEFAULT '',
    not_watched   TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS asks_usage (
    day   TEXT PRIMARY KEY,
    calls INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS asks_off (
    workspace TEXT PRIMARY KEY,
    at        INTEGER NOT NULL
);
-- A fence is the latest restart of a workspace and the directories under it, in Unix ms; ''
-- covers every workspace, and '=' before a path covers that workspace alone. A session
-- first seen later starts no earlier than its fence.
CREATE TABLE IF NOT EXISTS asks_fences (
    workspace TEXT PRIMARY KEY,
    at        INTEGER NOT NULL
);
`

// EnsureAsks creates the asks tables.
func (s *Store) EnsureAsks() error {
	if _, err := s.db.Exec(asksSchema); err != nil {
		return fmt.Errorf("init asks schema: %w", err)
	}
	// For a registry made before these columns existed.
	s.db.Exec(`ALTER TABLE asks_sessions ADD COLUMN not_before INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE asks_sessions ADD COLUMN gen INTEGER NOT NULL DEFAULT 0`)
	// An old registry cannot tell the agent's repeats from aiq's own text, so its last sighting
	// counts as a repeat: an older reply then leaves the item open rather than closing it.
	if _, err := s.db.Exec(`ALTER TABLE asks ADD COLUMN repeat_at INTEGER NOT NULL DEFAULT 0`); err == nil {
		s.db.Exec(`UPDATE asks SET repeat_at = MAX(raised_at, last_seen)`)
	}
	return nil
}

const askColumns = `id, chain, workspace, provider, session_id, raised_at, state, state_at, state_reason,
	state_text, first_quote, last_quote, asks_reader, risk, refs, repeats, last_seen, repeat_at`

func scanAsk(row interface{ Scan(...any) error }) (Ask, error) {
	var a Ask
	var refs string
	err := row.Scan(&a.ID, &a.Chain, &a.Workspace, &a.Provider, &a.SessionID, &a.RaisedAt, &a.State, &a.StateAt,
		&a.StateReason, &a.StateText, &a.FirstQuote, &a.LastQuote, &a.AsksReader, &a.Risk, &refs, &a.Repeats,
		&a.LastSeen, &a.RepeatAt)
	if err != nil {
		return a, err
	}
	json.Unmarshal([]byte(refs), &a.Refs)
	return a, nil
}

// GetAsk returns one item.
func (s *Store) GetAsk(id int64) (Ask, error) {
	return scanAsk(s.db.QueryRow(`SELECT `+askColumns+` FROM asks WHERE id = ?`, id))
}

// ListAsks returns items newest first. An empty chain means every chain, and no
// states means every state.
func (s *Store) ListAsks(chain string, states ...AskState) ([]Ask, error) {
	q := `SELECT ` + askColumns + ` FROM asks`
	var where []string
	var args []any
	if chain != "" {
		where = append(where, "chain = ?")
		args = append(args, chain)
	}
	if len(states) > 0 {
		marks := make([]string, len(states))
		for i, st := range states {
			marks[i] = "?"
			args = append(args, st)
		}
		where = append(where, "state IN ("+strings.Join(marks, ",")+")")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(q+` ORDER BY raised_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ask{}
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAsksMoved returns items a rule moved at or after since, newest first: what the
// person may want to check. A move the person made is left out.
func (s *Store) ListAsksMoved(since int64) ([]Ask, error) {
	rows, err := s.db.Query(`SELECT `+askColumns+` FROM asks
		WHERE state IN (?, ?) AND state_at >= ? AND state_reason <> ? ORDER BY state_at DESC, id DESC`,
		AskClosed, AskProbablyDone, since, AskPerson)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ask{}
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AskAction says what the person can do to an item in a state.
func AskAction(st AskState) (dismiss, restore bool) {
	switch st {
	case AskOpen:
		return true, false
	case AskProbablyDone:
		return true, true
	case AskClosed, AskDismissed:
		return false, true
	}
	return false, false
}

// SetAskByPerson applies the person's dismiss or restore. The text that last moved the
// item stays, and the move is logged, so an undo and a check of the rules both work.
func (s *Store) SetAskByPerson(id int64, to AskState, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Write first, so the transaction holds the lock before it reads.
	if _, err := tx.Exec(`UPDATE asks SET id = id WHERE id = ?`, id); err != nil {
		return err
	}
	a, err := scanAsk(tx.QueryRow(`SELECT `+askColumns+` FROM asks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no ask %d", id)
	}
	if err != nil {
		return err
	}
	dismiss, restore := AskAction(a.State)
	if (to == AskDismissed && !dismiss) || (to == AskOpen && !restore) || (to != AskDismissed && to != AskOpen) {
		return fmt.Errorf("ask %d is %s, which cannot move to %s", id, a.State, to)
	}
	if _, err := tx.Exec(`UPDATE asks SET state = ?, state_at = ?, state_reason = ? WHERE id = ?`, to, now, AskPerson, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO asks_events (ask_id, at, from_st, to_st, reason) VALUES (?,?,?,?,?)`,
		id, now, a.State, to, AskPerson); err != nil {
		return err
	}
	return tx.Commit()
}

// UndoAsk reverses the person's last move of an item, returning what it became. An item
// a rule moved since then is left alone.
func (s *Store) UndoAsk(id int64, now int64) (AskState, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE asks SET id = id WHERE id = ?`, id); err != nil {
		return "", err
	}
	a, err := scanAsk(tx.QueryRow(`SELECT `+askColumns+` FROM asks WHERE id = ?`, id))
	if err != nil {
		return "", err
	}
	var from, to AskState
	var reason string
	err = tx.QueryRow(`SELECT from_st, to_st, reason FROM asks_events WHERE ask_id = ? ORDER BY id DESC LIMIT 1`, id).Scan(&from, &to, &reason)
	if errors.Is(err, sql.ErrNoRows) || reason != AskPerson || to != a.State {
		return a.State, fmt.Errorf("nothing to undo on ask %d", id)
	}
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE asks SET state = ?, state_at = ?, state_reason = ? WHERE id = ?`, from, now, AskPerson, id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO asks_events (ask_id, at, from_st, to_st, reason) VALUES (?,?,?,?,?)`,
		id, now, to, from, AskPerson); err != nil {
		return "", err
	}
	return from, tx.Commit()
}

// AskMove is a rule moving an item, planned against the state it read. At is when the
// text that moved it was written.
type AskMove struct {
	ID     int64
	From   AskState
	FromAt int64
	To     AskState
	Reason string
	Text   string
	At     int64
}

// AskRepeat says an agent raised an existing item again.
type AskRepeat struct {
	ID    int64
	Quote string
	Refs  []string
	Risk  float64
	At    int64
	// Aiq marks text aiq itself put in the turn, such as a takeover prompt: it only
	// updates last-seen, so a successor restating a list cannot reopen items.
	Aiq bool
}

// AskPlan is everything the rules decided for one entry, applied in one transaction.
type AskPlan struct {
	Inserts []Ask
	Repeats []AskRepeat
	Moves   []AskMove
}

// AskPosition is how far a session has been scored. TurnStarted is Unix milliseconds.
// Gen counts the session's restarts: a plan read before one cannot land after it, even
// when the restart writes the same place. A position built from another keeps its Gen.
type AskPosition struct {
	TurnStarted int64
	ItemsDone   int
	ReplySHA    string
	Gen         int64
}

// ErrAskPositionMoved means another watcher scored this entry first.
var ErrAskPositionMoved = errors.New("asks: session position moved")

// repeatNext is what a repeat does to an item's state. A dismissal stays: dismissing is
// the person's call, and reopening on an agent's repeat would make every nag an ask again.
func repeatNext(from AskState, aiq bool) AskState {
	if aiq {
		return from
	}
	switch from {
	case AskProbablyDone, AskClosed:
		return AskOpen
	}
	return from
}

// ApplyAskPlan advances a session's position and applies a plan as one transaction. The
// first statement writes, so the transaction holds the lock before reading and the busy
// wait applies. A move only lands on the state it was planned against, and only for text
// newer than that state and than the agent's last repeat of the item, so a dismiss made
// during the model calls is kept and an answer cannot settle a question asked after it.
func (s *Store) ApplyAskPlan(provider, sessionID string, from, to AskPosition, skipped int, p AskPlan) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE asks_sessions SET turn_started = ?, items_done = ?, reply_sha = ?, skipped = skipped + ?, attempts = 0
		WHERE provider = ? AND session_id = ? AND turn_started = ? AND items_done = ? AND reply_sha = ? AND gen = ?`,
		to.TurnStarted, to.ItemsDone, to.ReplySHA, skipped, provider, sessionID, from.TurnStarted, from.ItemsDone, from.ReplySHA, from.Gen)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAskPositionMoved
	}
	for _, a := range p.Inserts {
		if a.State == "" {
			a.State = AskOpen
		}
		if a.StateAt == 0 {
			a.StateAt = a.RaisedAt
		}
		if a.LastSeen == 0 {
			a.LastSeen = a.RaisedAt
		}
		if a.RepeatAt == 0 {
			a.RepeatAt = a.RaisedAt
		}
		if a.LastQuote == "" {
			a.LastQuote = a.FirstQuote
		}
		if a.Refs == nil {
			a.Refs = []string{}
		}
		refs, _ := json.Marshal(a.Refs)
		if _, err := tx.Exec(`INSERT INTO asks (chain, workspace, provider, session_id, raised_at, state, state_at, state_reason,
			state_text, first_quote, last_quote, asks_reader, risk, refs, repeats, last_seen, repeat_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
			a.Chain, a.Workspace, a.Provider, a.SessionID, a.RaisedAt, a.State, a.StateAt, a.StateReason, a.StateText,
			a.FirstQuote, a.LastQuote, a.AsksReader, a.Risk, string(refs), a.Repeats, a.LastSeen, a.RepeatAt); err != nil {
			return err
		}
	}
	for _, r := range p.Repeats {
		if err := applyRepeat(tx, r); err != nil {
			return err
		}
	}
	for _, m := range p.Moves {
		res, err := tx.Exec(`UPDATE asks SET state = ?, state_at = ?, state_reason = ?, state_text = ?
			WHERE id = ? AND state = ? AND state_at = ? AND state_at < ? AND repeat_at < ?`,
			m.To, m.At, m.Reason, m.Text, m.ID, m.From, m.FromAt, m.At, m.At)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // the person or a newer text got there first
		}
		if _, err := tx.Exec(`INSERT INTO asks_events (ask_id, at, from_st, to_st, reason, text) VALUES (?,?,?,?,?,?)`,
			m.ID, m.At, m.From, m.To, m.Reason, m.Text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func applyRepeat(tx *sql.Tx, r AskRepeat) error {
	a, err := scanAsk(tx.QueryRow(`SELECT `+askColumns+` FROM asks WHERE id = ?`, r.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Aiq {
		_, err := tx.Exec(`UPDATE asks SET last_seen = ? WHERE id = ? AND last_seen < ?`, r.At, r.ID, r.At)
		return err
	}
	if r.At <= a.RepeatAt {
		return nil // older text must not overwrite newer
	}
	seen := map[string]bool{}
	merged := []string{}
	for _, ref := range append(a.Refs, r.Refs...) {
		if !seen[ref] {
			seen[ref] = true
			merged = append(merged, ref)
		}
	}
	refs, _ := json.Marshal(merged)
	risk := a.Risk
	if r.Risk > risk {
		risk = r.Risk
	}
	next := repeatNext(a.State, false)
	if next != a.State && r.At <= a.StateAt {
		next = a.State
	}
	stateAt, reason := a.StateAt, a.StateReason
	if next != a.State {
		stateAt, reason = r.At, "repeat"
		if _, err := tx.Exec(`INSERT INTO asks_events (ask_id, at, from_st, to_st, reason, text) VALUES (?,?,?,?,?,?)`,
			r.ID, r.At, a.State, next, reason, r.Quote); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE asks SET last_quote = ?, refs = ?, risk = ?, repeats = repeats + 1, last_seen = MAX(last_seen, ?),
		repeat_at = ?, state = ?, state_at = ?, state_reason = ? WHERE id = ?`,
		r.Quote, string(refs), risk, r.At, r.At, next, stateAt, reason, r.ID)
	return err
}

// AskSession is a session's scoring progress.
type AskSession struct {
	Provider   string
	SessionID  string
	Chain      string
	Workspace  string
	Transcript string
	Position   AskPosition
	Skipped    int
	Attempts   int
	Done       bool
	// NotBefore drops text written earlier, in Unix milliseconds: nothing older is sent or
	// raised. It moves on when the watcher starts and when the person resets.
	NotBefore int64
}

// UpsertAskSession records a live session and returns its row. A new row has no position.
// An existing row keeps its position and is watched again.
func (s *Store) UpsertAskSession(a AskSession) (AskSession, bool, error) {
	// A workspace named through a link is fenced by its target's name too.
	real, _ := filepath.EvalSymlinks(a.Workspace)
	tx, err := s.db.Begin()
	if err != nil {
		return a, false, err
	}
	defer tx.Rollback()
	// Write first, so the transaction holds the lock before it reads the fence: turning a
	// workspace on or a reset then lands wholly before or after this insert.
	if _, err := tx.Exec(`UPDATE asks_sessions SET done = done WHERE 0`); err != nil {
		return a, false, err
	}
	fence, err := askFence(tx, a.Workspace)
	if err != nil {
		return a, false, err
	}
	if real != "" && real != a.Workspace {
		other, err := askFence(tx, real)
		if err != nil {
			return a, false, err
		}
		fence = max(fence, other)
	}
	a.NotBefore = max(a.NotBefore, fence)
	res, err := tx.Exec(`INSERT INTO asks_sessions (provider, session_id, chain, workspace, transcript, not_before)
		VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING`, a.Provider, a.SessionID, a.Chain, a.Workspace, a.Transcript, a.NotBefore)
	if err != nil {
		return a, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if _, err := tx.Exec(`UPDATE asks_sessions SET done = 0, transcript = ? WHERE provider = ? AND session_id = ?`,
			a.Transcript, a.Provider, a.SessionID); err != nil {
			return a, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return a, false, err
	}
	row, err := s.GetAskSession(a.Provider, a.SessionID)
	return row, n == 1, err
}

// askFence is the latest fence covering workspace.
func askFence(tx *sql.Tx, workspace string) (int64, error) {
	rows, err := tx.Query(`SELECT workspace, at FROM asks_fences`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var out int64
	for rows.Next() {
		var w string
		var at int64
		if err := rows.Scan(&w, &at); err != nil {
			return 0, err
		}
		exact, isExact := strings.CutPrefix(w, "=")
		if w == "" || workspace == w || isExact && workspace == exact || !isExact && strings.HasPrefix(workspace, strings.TrimSuffix(w, "/")+"/") {
			out = max(out, at)
		}
	}
	return out, rows.Err()
}

// FenceAsks starts every session first seen from now on in dir, and the directories under it,
// no earlier than now. An empty dir covers every workspace. A reset calls it for the
// directory it was run for, which may hold sessions not seen yet.
func (s *Store) FenceAsks(dir string, now time.Time) error {
	_, err := s.db.Exec(`INSERT INTO asks_fences (workspace, at) VALUES (?, ?) ON CONFLICT(workspace) DO UPDATE SET at = MAX(at, excluded.at)`, dir, now.UnixMilli())
	return err
}

// FenceAskWorkspace is FenceAsks for one workspace alone, not the directories under it.
func (s *Store) FenceAskWorkspace(workspace string, now time.Time) error {
	return s.FenceAsks("="+workspace, now)
}

// GetAskSession returns one session's row.
func (s *Store) GetAskSession(provider, sessionID string) (AskSession, error) {
	var a AskSession
	var done int
	err := s.db.QueryRow(`SELECT provider, session_id, chain, workspace, transcript, turn_started, items_done, reply_sha, skipped, attempts, done, not_before, gen
		FROM asks_sessions WHERE provider = ? AND session_id = ?`, provider, sessionID).Scan(&a.Provider, &a.SessionID, &a.Chain,
		&a.Workspace, &a.Transcript, &a.Position.TurnStarted, &a.Position.ItemsDone, &a.Position.ReplySHA, &a.Skipped, &a.Attempts, &done, &a.NotBefore, &a.Position.Gen)
	a.Done = done != 0
	return a, err
}

// ListAskSessions returns the rows still being read.
func (s *Store) ListAskSessions() ([]AskSession, error) {
	rows, err := s.db.Query(`SELECT provider, session_id, chain, workspace, transcript, turn_started, items_done, reply_sha, skipped, attempts, done, not_before, gen
		FROM asks_sessions WHERE done = 0 ORDER BY provider, session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AskSession{}
	for rows.Next() {
		var a AskSession
		var done int
		if err := rows.Scan(&a.Provider, &a.SessionID, &a.Chain, &a.Workspace, &a.Transcript, &a.Position.TurnStarted,
			&a.Position.ItemsDone, &a.Position.ReplySHA, &a.Skipped, &a.Attempts, &done, &a.NotBefore, &a.Position.Gen); err != nil {
			return nil, err
		}
		a.Done = done != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAskSessionDone stops reading a session, once it has ended and been read to its end.
func (s *Store) SetAskSessionDone(provider, sessionID string) error {
	_, err := s.db.Exec(`UPDATE asks_sessions SET done = 1 WHERE provider = ? AND session_id = ?`, provider, sessionID)
	return err
}

// AddAskAttempt counts a failed try of a session's current entry and returns the count.
func (s *Store) AddAskAttempt(provider, sessionID string) (int, error) {
	if _, err := s.db.Exec(`UPDATE asks_sessions SET attempts = attempts + 1 WHERE provider = ? AND session_id = ?`, provider, sessionID); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT attempts FROM asks_sessions WHERE provider = ? AND session_id = ?`, provider, sessionID).Scan(&n)
	return n, err
}

// askRestart forgets a session's place, so everything before not_before is passed over, and
// counts a restart, so a plan already being scored can no longer land.
const askRestart = `not_before = ?, turn_started = -1, items_done = 0, reply_sha = '', attempts = 0, gen = gen + 1`

// RestartAskSessions starts every session over from now: text older than this is never read.
func (s *Store) RestartAskSessions(now time.Time) error {
	_, err := s.db.Exec(`UPDATE asks_sessions SET `+askRestart+`, skipped = 0`, now.UnixMilli())
	return err
}

// ResetAsks erases the items of every workspace match accepts, with their history, and starts
// those sessions over from now, so nothing older comes back. It returns how many went.
func (s *Store) ResetAsks(match func(workspace string) bool, now time.Time) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Write first, so the transaction holds the lock before it reads.
	if _, err := tx.Exec(`UPDATE asks_sessions SET done = done WHERE 0`); err != nil {
		return 0, err
	}
	rows, err := tx.Query(`SELECT workspace FROM asks UNION SELECT workspace FROM asks_sessions`)
	if err != nil {
		return 0, err
	}
	var names []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			rows.Close()
			return 0, err
		}
		if match(w) {
			names = append(names, w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, w := range names {
		if _, err := tx.Exec(`DELETE FROM asks_events WHERE ask_id IN (SELECT id FROM asks WHERE workspace = ?)`, w); err != nil {
			return 0, err
		}
		res, err := tx.Exec(`DELETE FROM asks WHERE workspace = ?`, w)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if _, err := tx.Exec(`UPDATE asks_sessions SET `+askRestart+`, skipped = 0 WHERE workspace = ?`, now.UnixMilli(), w); err != nil {
			return 0, err
		}
		// This workspace alone: one that holds the reset's directory also holds others the
		// reset does not cover. The caller fences the directory and what is under it.
		if _, err := tx.Exec(`INSERT INTO asks_fences (workspace, at) VALUES (?, ?) ON CONFLICT(workspace) DO UPDATE SET at = MAX(at, excluded.at)`, "="+w, now.UnixMilli()); err != nil {
			return 0, err
		}
	}
	return total, tx.Commit()
}

// AsksStatus is what the watcher last reported. Everything that shows asks shows this
// too, so an empty list can be told from a watcher that is off, behind or failing.
type AsksStatus struct {
	PassAt     int64    `json:"pass_at"`
	OkAt       int64    `json:"ok_at"`
	Watched    int      `json:"watched"`
	Waiting    int      `json:"waiting"`
	NextTryAt  int64    `json:"next_try_at"`
	ErrorAt    int64    `json:"error_at"`
	LastError  string   `json:"last_error"`
	NotWatched []string `json:"not_watched"`
	Skipped    int      `json:"skipped"`
	CallsToday int      `json:"calls_today"`
}

// SetAsksStatus stores the watcher's report.
func (s *Store) SetAsksStatus(st AsksStatus) error {
	if st.NotWatched == nil {
		st.NotWatched = []string{}
	}
	nw, _ := json.Marshal(st.NotWatched)
	_, err := s.db.Exec(`INSERT INTO asks_watch (id, pass_at, ok_at, watched, waiting, next_try_at, capped, error_at, last_error, not_watched)
		VALUES (1,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET pass_at = excluded.pass_at, ok_at = excluded.ok_at,
		watched = excluded.watched, waiting = excluded.waiting, next_try_at = excluded.next_try_at, capped = excluded.capped,
		error_at = excluded.error_at, last_error = excluded.last_error, not_watched = excluded.not_watched`,
		st.PassAt, st.OkAt, st.Watched, st.Waiting, st.NextTryAt, 0, st.ErrorAt, st.LastError, string(nw))
	return err
}

// GetAsksStatus reads the watcher's report. No report leaves PassAt zero.
func (s *Store) GetAsksStatus(now time.Time) (AsksStatus, error) {
	var st AsksStatus
	var capped int
	var nw string
	err := s.db.QueryRow(`SELECT pass_at, ok_at, watched, waiting, next_try_at, capped, error_at, last_error, not_watched FROM asks_watch WHERE id = 1`).
		Scan(&st.PassAt, &st.OkAt, &st.Watched, &st.Waiting, &st.NextTryAt, &capped, &st.ErrorAt, &st.LastError, &nw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return st, err
	}
	json.Unmarshal([]byte(nw), &st.NotWatched)
	s.db.QueryRow(`SELECT COALESCE(SUM(skipped), 0) FROM asks_sessions`).Scan(&st.Skipped)
	s.db.QueryRow(`SELECT calls FROM asks_usage WHERE day = ?`, AsksDay(now)).Scan(&st.CallsToday)
	return st, nil
}

// AsksDay is the day a model call counts against: UTC, so a laptop changing timezone
// cannot reset the cap early.
func AsksDay(t time.Time) string { return t.UTC().Format("2006-01-02") }

// CountAskCall adds one model call to the day's count.
func (s *Store) CountAskCall(day string) error {
	_, err := s.db.Exec(`INSERT INTO asks_usage (day, calls) VALUES (?, 1)
		ON CONFLICT(day) DO UPDATE SET calls = calls + 1`, day)
	return err
}

// AskWorkspacesOff lists the workspaces the person turned off.
func (s *Store) AskWorkspacesOff() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT workspace FROM asks_off`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			return nil, err
		}
		out[w] = true
	}
	return out, rows.Err()
}

// AskWorkspaceOff says whether a workspace is turned off now.
func (s *Store) AskWorkspaceOff(workspace string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM asks_off WHERE workspace = ?`, workspace).Scan(&n)
	return n > 0, err
}

// SetAskWorkspaceOff turns a workspace off or back on. While off, nothing from it is sent.
// Either way its sessions start over from now, so a plan scored before cannot land and
// nothing written while it was off is read once it is back on.
func (s *Store) SetAskWorkspaceOff(workspace string, off bool, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if off {
		_, err = tx.Exec(`INSERT INTO asks_off (workspace, at) VALUES (?, ?) ON CONFLICT(workspace) DO UPDATE SET at = excluded.at`, workspace, now.Unix())
	} else {
		_, err = tx.Exec(`DELETE FROM asks_off WHERE workspace = ?`, workspace)
	}
	if err != nil {
		return err
	}
	// To the millisecond, so text written in the second before turning back on stays unread.
	// The fence holds a session not seen yet in this workspace, and only this one, to the
	// same moment.
	if _, err := tx.Exec(`UPDATE asks_sessions SET `+askRestart+` WHERE workspace = ?`, now.UnixMilli(), workspace); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO asks_fences (workspace, at) VALUES (?, ?) ON CONFLICT(workspace) DO UPDATE SET at = MAX(at, excluded.at)`, "="+workspace, now.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
