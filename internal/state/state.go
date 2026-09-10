// Package state persists accounts, usage windows, leases, workspace affinity
// and an event log in a local SQLite database. Credentials never live here:
// each account's credential stays inside its own provider home.
package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id                  TEXT PRIMARY KEY,
    provider            TEXT NOT NULL,
    name                TEXT NOT NULL,
    enabled             INTEGER NOT NULL DEFAULT 1,
    home                TEXT NOT NULL,
    native              INTEGER NOT NULL DEFAULT 0,
    quota_id            TEXT,
    identity            TEXT,
    priority            INTEGER NOT NULL DEFAULT 100,
    created_at          INTEGER NOT NULL,
    last_selected_at    INTEGER,
    last_success_at     INTEGER
);

CREATE TABLE IF NOT EXISTS windows (
    account_id          TEXT NOT NULL,
    key                 TEXT NOT NULL,
    label               TEXT,
    kind                TEXT NOT NULL,
    scope               TEXT,
    used_pct            REAL,
    resets_at           INTEGER,
    window_seconds      INTEGER,
    severity            TEXT,
    source              TEXT,
    observed_at         INTEGER,
    PRIMARY KEY(account_id, key)
);

CREATE TABLE IF NOT EXISTS usage (
    account_id          TEXT PRIMARY KEY,
    exhausted           INTEGER NOT NULL DEFAULT 0,
    exhausted_reason    TEXT,
    cooldown_until      INTEGER,
    reset_credits       INTEGER NOT NULL DEFAULT 0,
    plan                TEXT,
    poll_error          TEXT,
    observed_at         INTEGER,
    exhausted_at        INTEGER
);

CREATE TABLE IF NOT EXISTS leases (
    id                  INTEGER PRIMARY KEY,
    account_id          TEXT NOT NULL,
    pid                 INTEGER,
    hostname            TEXT,
    mode                TEXT,
    cwd                 TEXT,
    depth               INTEGER NOT NULL DEFAULT 0,
    parent_account      TEXT,
    root_id             INTEGER,
    args                TEXT,
    started_at          INTEGER,
    workspace           TEXT,
    pane                TEXT,
    session_id          TEXT,
    provider            TEXT,
    fallback            TEXT,
    drain               TEXT,
    drain_at            INTEGER,
    turn_started_at     INTEGER,
    turn_ended_at       INTEGER,
    takeover_of         INTEGER
);

CREATE TABLE IF NOT EXISTS affinity (
    provider            TEXT,
    workspace           TEXT,
    account_id          TEXT,
    updated_at          INTEGER,
    PRIMARY KEY(provider, workspace)
);

CREATE TABLE IF NOT EXISTS events (
    id                  INTEGER PRIMARY KEY,
    timestamp           INTEGER,
    provider            TEXT,
    account_id          TEXT,
    event_type          TEXT,
    detail              TEXT
);
`

// Window kinds.
const (
	KindShort  = "short"  // rolling window of a day or less (Claude 5h, Codex 5h)
	KindWeekly = "weekly" // 7-day window
	KindOther  = "other"  // credits, spend, anything that is not a rolling cap
)

// Lease modes.
const (
	ModeInteractive = "interactive"
	ModeWorker      = "worker"
	ModeLong        = "long" // supervised long-running session in a tmux pane
)

// Drain states of a long lease.
const (
	DrainNone      = ""          // running normally
	DrainRequested = "requested" // daemon asked for a wrap-up; hook has not delivered it yet
	DrainDraining  = "draining"  // wrap-up instruction delivered, agent is finishing
	DrainReady     = "ready"     // agent finished its wrap-up turn; safe to respawn
	DrainWaiting   = "waiting"   // needs a successor but no account is eligible yet
)

type Account struct {
	ID             string
	Provider       string
	Name           string
	Enabled        bool
	Home           string // provider home directory used for launches
	Native         bool   // Home is the user's real ~/.claude or ~/.codex
	QuotaID        string // aiquota account id feeding telemetry ("" = none)
	Identity       string // email reported by the provider
	Priority       int
	CreatedAt      int64
	LastSelectedAt int64
	LastSuccessAt  int64
}

type Window struct {
	AccountID     string
	Key           string
	Label         string
	Kind          string
	Scope         string  // model or feature the window applies to ("" = whole account)
	UsedPct       float64 // -1 = unknown
	ResetsAt      int64   // unix seconds, 0 = unknown
	WindowSeconds int64
	Severity      string
	Source        string // "aiquota", "statusline", "probe", "manual"
	ObservedAt    int64
}

type Usage struct {
	AccountID       string
	Exhausted       bool
	ExhaustedReason string
	CooldownUntil   int64
	ExhaustedAt     int64 // when the mark was made (0 = not marked)
	ResetCredits    int
	Plan            string
	PollError       string
	ObservedAt      int64
}

type Lease struct {
	ID            int64
	AccountID     string
	PID           int
	Hostname      string
	Mode          string
	Cwd           string
	Depth         int
	ParentAccount string
	RootID        int64
	Args          string
	StartedAt     int64

	// Long-session fields (ModeLong only).
	Workspace     string
	Pane          string // tmux pane id (%17)
	SessionID     string // CLI session/thread id, for resume
	Provider      string
	Fallback      string // comma-separated provider order for takeover
	Drain         string
	DrainAt       int64
	TurnStartedAt int64
	TurnEndedAt   int64
	TakeoverOf    int64
	// Launcher names the registered launcher this session runs under, so a
	// takeover can start the successor the same way. Empty means bare.
	Launcher string
}

// InTurn reports whether the agent is in the middle of a turn.
func (l Lease) InTurn() bool { return l.TurnStartedAt > l.TurnEndedAt }

type Event struct {
	ID        int64
	Timestamp int64
	Provider  string
	AccountID string
	Type      string
	Detail    string
}

type Store struct {
	db *sql.DB
}

// AccountID builds the canonical "<provider>/<name>" identifier.
func AccountID(provider, name string) string { return provider + "/" + name }

// ParseAccountID splits "<provider>/<name>".
func ParseAccountID(id string) (provider, name string, err error) {
	provider, name, ok := strings.Cut(id, "/")
	if !ok || provider == "" || name == "" {
		return "", "", fmt.Errorf("invalid account id %q (want <provider>/<name>)", id)
	}
	return provider, name, nil
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	// Migrations for databases created before a column existed.
	db.Exec(`ALTER TABLE usage ADD COLUMN exhausted_at INTEGER`)
	for _, col := range []string{"workspace TEXT", "pane TEXT", "session_id TEXT", "provider TEXT", "fallback TEXT",
		"drain TEXT", "drain_at INTEGER", "turn_started_at INTEGER", "turn_ended_at INTEGER", "takeover_of INTEGER",
		"launcher TEXT"} {
		db.Exec(`ALTER TABLE leases ADD COLUMN ` + col)
	}
	os.Chmod(path, 0o600)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ErrNotFound is returned when an account does not exist.
var ErrNotFound = errors.New("account not found")

// --- accounts ---

const accountCols = `id, provider, name, enabled, home, native, COALESCE(quota_id,''), COALESCE(identity,''),
	priority, created_at, COALESCE(last_selected_at,0), COALESCE(last_success_at,0)`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var enabled, native int
	err := row.Scan(&a.ID, &a.Provider, &a.Name, &enabled, &a.Home, &native, &a.QuotaID, &a.Identity,
		&a.Priority, &a.CreatedAt, &a.LastSelectedAt, &a.LastSuccessAt)
	a.Enabled = enabled != 0
	a.Native = native != 0
	return a, err
}

func (s *Store) AddAccount(a Account) error {
	_, err := s.db.Exec(
		`INSERT INTO accounts (id, provider, name, enabled, home, native, quota_id, identity, priority, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Provider, a.Name, boolInt(a.Enabled), a.Home, boolInt(a.Native), a.QuotaID, a.Identity, a.Priority, a.CreatedAt)
	return err
}

// UpdateAccount rewrites the mutable fields (home, native, quota_id,
// identity, priority) of an existing account.
func (s *Store) UpdateAccount(a Account) error {
	res, err := s.db.Exec(
		`UPDATE accounts SET home = ?, native = ?, quota_id = ?, identity = ?, priority = ? WHERE id = ?`,
		a.Home, boolInt(a.Native), a.QuotaID, a.Identity, a.Priority, a.ID)
	return s.mustAffect(res, err, a.ID)
}

func (s *Store) GetAccount(id string) (Account, error) {
	row := s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return a, err
}

// ListAccounts returns accounts, all providers if provider is empty.
func (s *Store) ListAccounts(provider string) ([]Account, error) {
	q := `SELECT ` + accountCols + ` FROM accounts`
	args := []any{}
	if provider != "" {
		q += ` WHERE provider = ?`
		args = append(args, provider)
	}
	q += ` ORDER BY provider, priority, name`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SetEnabled(id string, enabled bool) error {
	res, err := s.db.Exec(`UPDATE accounts SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	return s.mustAffect(res, err, id)
}

func (s *Store) RemoveAccount(id string) error {
	res, err := s.db.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err := s.mustAffect(res, err, id); err != nil {
		return err
	}
	s.db.Exec(`DELETE FROM usage WHERE account_id = ?`, id)
	s.db.Exec(`DELETE FROM windows WHERE account_id = ?`, id)
	s.db.Exec(`DELETE FROM leases WHERE account_id = ?`, id)
	s.db.Exec(`DELETE FROM affinity WHERE account_id = ?`, id)
	return nil
}

func (s *Store) TouchSelected(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE accounts SET last_selected_at = ? WHERE id = ?`, ts, id)
	return err
}

func (s *Store) TouchSuccess(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE accounts SET last_success_at = ? WHERE id = ?`, ts, id)
	return err
}

// --- windows ---

// UpsertWindow inserts or replaces one window observation.
func (s *Store) UpsertWindow(w Window) error {
	_, err := s.db.Exec(
		`INSERT INTO windows (account_id, key, label, kind, scope, used_pct, resets_at, window_seconds, severity, source, observed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(account_id, key) DO UPDATE SET
		   label = excluded.label, kind = excluded.kind, scope = excluded.scope,
		   used_pct = excluded.used_pct,
		   resets_at = CASE WHEN excluded.resets_at > 0 THEN excluded.resets_at ELSE windows.resets_at END,
		   window_seconds = CASE WHEN excluded.window_seconds > 0 THEN excluded.window_seconds ELSE windows.window_seconds END,
		   severity = excluded.severity, source = excluded.source, observed_at = excluded.observed_at`,
		w.AccountID, w.Key, w.Label, w.Kind, w.Scope, w.UsedPct, w.ResetsAt, w.WindowSeconds, w.Severity, w.Source, w.ObservedAt)
	return err
}

// PruneWindowSources deletes an account's windows whose source is not in keep.
func (s *Store) PruneWindowSources(accountID string, keep ...string) error {
	args := []any{accountID}
	q := `DELETE FROM windows WHERE account_id = ?`
	for _, k := range keep {
		q += ` AND source != ?`
		args = append(args, k)
	}
	_, err := s.db.Exec(q, args...)
	return err
}

// ReplaceWindows replaces every window recorded by source for an account.
func (s *Store) ReplaceWindows(accountID, source string, ws []Window) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM windows WHERE account_id = ? AND source = ?`, accountID, source); err != nil {
		tx.Rollback()
		return err
	}
	for _, w := range ws {
		w.AccountID = accountID
		w.Source = source
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO windows (account_id, key, label, kind, scope, used_pct, resets_at, window_seconds, severity, source, observed_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			w.AccountID, w.Key, w.Label, w.Kind, w.Scope, w.UsedPct, w.ResetsAt, w.WindowSeconds, w.Severity, w.Source, w.ObservedAt); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListWindows(accountID string) ([]Window, error) {
	rows, err := s.db.Query(
		`SELECT account_id, key, COALESCE(label,''), kind, COALESCE(scope,''), COALESCE(used_pct,-1),
		        COALESCE(resets_at,0), COALESCE(window_seconds,0), COALESCE(severity,''), COALESCE(source,''), COALESCE(observed_at,0)
		 FROM windows WHERE account_id = ? ORDER BY kind, key`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Window
	for rows.Next() {
		var w Window
		if err := rows.Scan(&w.AccountID, &w.Key, &w.Label, &w.Kind, &w.Scope, &w.UsedPct,
			&w.ResetsAt, &w.WindowSeconds, &w.Severity, &w.Source, &w.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// --- usage ---

// SetUsageMeta upserts the non-window telemetry (plan, reset credits, poll
// error) without touching exhaustion state.
func (s *Store) SetUsageMeta(id string, plan string, resetCredits int, pollError string, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO usage (account_id, plan, reset_credits, poll_error, observed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(account_id) DO UPDATE SET
		   plan = excluded.plan, reset_credits = excluded.reset_credits,
		   poll_error = excluded.poll_error, observed_at = excluded.observed_at`,
		id, plan, resetCredits, pollError, now.Unix())
	return err
}

func (s *Store) GetUsage(id string) (Usage, bool, error) {
	row := s.db.QueryRow(
		`SELECT account_id, exhausted, COALESCE(exhausted_reason,''), COALESCE(cooldown_until,0),
		        reset_credits, COALESCE(plan,''), COALESCE(poll_error,''), COALESCE(observed_at,0), COALESCE(exhausted_at,0)
		 FROM usage WHERE account_id = ?`, id)
	var u Usage
	var exhausted int
	err := row.Scan(&u.AccountID, &exhausted, &u.ExhaustedReason, &u.CooldownUntil,
		&u.ResetCredits, &u.Plan, &u.PollError, &u.ObservedAt, &u.ExhaustedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return u, false, nil
	}
	u.Exhausted = exhausted != 0
	return u, err == nil, err
}

// MarkExhausted puts an account into cooldown until the given time.
func (s *Store) MarkExhausted(id, reason string, until, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO usage (account_id, exhausted, exhausted_reason, cooldown_until, observed_at, exhausted_at)
		 VALUES (?, 1, ?, ?, ?, ?)
		 ON CONFLICT(account_id) DO UPDATE SET
		   exhausted = 1, exhausted_reason = excluded.exhausted_reason,
		   cooldown_until = excluded.cooldown_until, exhausted_at = excluded.exhausted_at`,
		id, reason, until.Unix(), now.Unix(), now.Unix())
	return err
}

// MarkReady clears exhaustion/cooldown.
func (s *Store) MarkReady(id string, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO usage (account_id, exhausted, exhausted_reason, cooldown_until, observed_at, exhausted_at)
		 VALUES (?, 0, '', 0, ?, 0)
		 ON CONFLICT(account_id) DO UPDATE SET
		   exhausted = 0, exhausted_reason = '', cooldown_until = 0, exhausted_at = 0`,
		id, now.Unix())
	return err
}

// --- leases ---

func (s *Store) AddLease(l Lease) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO leases (account_id, pid, hostname, mode, cwd, depth, parent_account, root_id, args, started_at,
		                     workspace, pane, session_id, provider, fallback, drain, drain_at, takeover_of, launcher)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.AccountID, l.PID, l.Hostname, l.Mode, l.Cwd, l.Depth, l.ParentAccount, l.RootID, l.Args, l.StartedAt,
		l.Workspace, l.Pane, l.SessionID, l.Provider, l.Fallback, l.Drain, l.DrainAt, l.TakeoverOf, l.Launcher)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if l.RootID == 0 {
		s.db.Exec(`UPDATE leases SET root_id = ? WHERE id = ?`, id, id)
	}
	return id, nil
}

func (s *Store) ReleaseLease(id int64) error {
	_, err := s.db.Exec(`DELETE FROM leases WHERE id = ?`, id)
	return err
}

const leaseCols = `id, account_id, COALESCE(pid,0), COALESCE(hostname,''), COALESCE(mode,''), COALESCE(cwd,''),
	depth, COALESCE(parent_account,''), COALESCE(root_id,0), COALESCE(args,''), COALESCE(started_at,0),
	COALESCE(workspace,''), COALESCE(pane,''), COALESCE(session_id,''), COALESCE(provider,''), COALESCE(fallback,''),
	COALESCE(drain,''), COALESCE(drain_at,0), COALESCE(turn_started_at,0), COALESCE(turn_ended_at,0), COALESCE(takeover_of,0),
	COALESCE(launcher,'')`

func scanLease(row interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	err := row.Scan(&l.ID, &l.AccountID, &l.PID, &l.Hostname, &l.Mode, &l.Cwd,
		&l.Depth, &l.ParentAccount, &l.RootID, &l.Args, &l.StartedAt,
		&l.Workspace, &l.Pane, &l.SessionID, &l.Provider, &l.Fallback,
		&l.Drain, &l.DrainAt, &l.TurnStartedAt, &l.TurnEndedAt, &l.TakeoverOf, &l.Launcher)
	return l, err
}

func (s *Store) ListLeases() ([]Lease, error) {
	rows, err := s.db.Query(`SELECT ` + leaseCols + ` FROM leases ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) GetLease(id int64) (Lease, error) {
	row := s.db.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE id = ?`, id)
	l, err := scanLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return l, fmt.Errorf("lease %d not found", id)
	}
	return l, err
}

// SetLeaseSession records the CLI session id (from a SessionStart hook or
// the status line) so the session can be resumed on another account.
func (s *Store) SetLeaseSession(id int64, sessionID string) error {
	_, err := s.db.Exec(`UPDATE leases SET session_id = ? WHERE id = ?`, sessionID, id)
	return err
}

// SetLeaseDrain moves a long lease through the drain state machine.
func (s *Store) SetLeaseDrain(id int64, drain string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE leases SET drain = ?, drain_at = ? WHERE id = ?`, drain, now.Unix(), id)
	return err
}

// MarkTurn records turn boundaries reported by hooks.
func (s *Store) MarkTurn(id int64, started bool, now time.Time) error {
	col := "turn_ended_at"
	if started {
		col = "turn_started_at"
	}
	_, err := s.db.Exec(`UPDATE leases SET `+col+` = ? WHERE id = ?`, now.Unix(), id)
	return err
}

// SetLeasePane records the tmux pane a long session runs in.
func (s *Store) SetLeasePane(id int64, pane string) error {
	_, err := s.db.Exec(`UPDATE leases SET pane = ? WHERE id = ?`, pane, id)
	return err
}

// MaxLeaseAge bounds how long a lease from a host we cannot probe (another
// machine, or this one under an earlier name) is believed.
const MaxLeaseAge = 36 * time.Hour

// PruneLeases removes leases on this host whose pid is gone, and leases from
// anywhere older than MaxLeaseAge. It returns the surviving leases.
func (s *Store) PruneLeases(hostname string, alive func(pid int) bool) ([]Lease, error) {
	leases, err := s.ListLeases()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-MaxLeaseAge).Unix()
	var live []Lease
	for _, l := range leases {
		if (l.Hostname == hostname && !alive(l.PID)) || l.StartedAt < cutoff {
			s.ReleaseLease(l.ID)
			continue
		}
		live = append(live, l)
	}
	return live, nil
}

// --- affinity ---

func (s *Store) GetAffinity(provider, workspace string) (string, error) {
	row := s.db.QueryRow(
		`SELECT account_id FROM affinity WHERE provider = ? AND workspace = ?`, provider, workspace)
	var id string
	err := row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) SetAffinity(provider, workspace, accountID string, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO affinity (provider, workspace, account_id, updated_at) VALUES (?, ?, ?, ?)`,
		provider, workspace, accountID, now.Unix())
	return err
}

// --- events ---

func (s *Store) LogEvent(provider, accountID, eventType, detail string, now time.Time) {
	s.db.Exec(
		`INSERT INTO events (timestamp, provider, account_id, event_type, detail) VALUES (?, ?, ?, ?, ?)`,
		now.Unix(), provider, accountID, eventType, detail)
}

func (s *Store) ListEvents(limit int) ([]Event, error) {
	rows, err := s.db.Query(
		`SELECT id, timestamp, COALESCE(provider,''), COALESCE(account_id,''), COALESCE(event_type,''), COALESCE(detail,'')
		 FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Provider, &e.AccountID, &e.Type, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TrimEvents keeps only the newest keep events.
func (s *Store) TrimEvents(keep int) {
	s.db.Exec(`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, keep)
}

func (s *Store) mustAffect(res sql.Result, err error, id string) error {
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
