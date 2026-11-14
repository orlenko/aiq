package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

var (
	ErrNavigatorSnapshotInvalid     = errors.New("invalid navigator metadata request")
	ErrNavigatorSnapshotTooLarge    = errors.New("navigator metadata too large")
	ErrNavigatorSnapshotUnavailable = errors.New("navigator metadata unavailable")
)

// These are admission allowances, not SQLite or total-process memory bounds.
// The caller supplies its existing response's allowances; no defaults live here.
type NavigatorSnapshotLimits struct{ Rows, TextBytes int64 }

// Closed projections deliberately exclude args, transcript paths, account homes,
// identities, usage, events and notes. A terminal reference is NOT liveness.
type NavigatorLaunch struct {
	ID, StartedMS, LeaseID                              int64
	Provider, AccountID, Launcher, Cwd, Mode, SessionID string
}
type NavigatorLease struct {
	ID, StartedAt, PID, Depth, TurnStartedAt, TurnEndedAt                       int64
	Provider, AccountID, Launcher, Cwd, Mode, Workspace, Pane, SessionID, Drain string
	DrainManual                                                                 bool
}
type NavigatorSnapshot struct {
	Launches []NavigatorLaunch
	Leases   []NavigatorLease
	hostname string
}

// HostIdentity correlates a later point witness with this admitted snapshot.
// It is deliberately not a JSON field or a source of terminal authority.
func (s *NavigatorSnapshot) HostIdentity() string {
	if s == nil {
		return ""
	}
	return s.hostname
}

type navigatorTable struct {
	name       string
	text, ints []string
}

var navigatorTables = []navigatorTable{
	{"launches", []string{"provider", "account_id", "launcher", "cwd", "mode", "session_id"}, []string{"id", "started_at_ms", "lease_id"}},
	{"leases", []string{"provider", "account_id", "launcher", "cwd", "mode", "workspace", "pane", "session_id", "drain"}, []string{"id", "started_at", "pid", "depth", "turn_started_at", "turn_ended_at", "drain_manual"}},
}

const navigatorMaxInteger int64 = 9007199254740991

// ReadNavigatorSnapshot reads both tables from one committed view. Missing or
// incompatible data is unavailable, never evidence of a complete empty catalog.
// This method neither creates nor repairs a database and never queries a backend.
func (s *Store) ReadNavigatorSnapshot(ctx context.Context, hostname string, limits NavigatorSnapshotLimits) (out *NavigatorSnapshot, err error) {
	if ctx == nil || s == nil || s.db == nil || hostname == "" || len(hostname) > 4096 || !utf8.ValidString(hostname) || strings.ContainsRune(hostname, 0) || limits.Rows <= 0 || limits.TextBytes <= 0 {
		return nil, ErrNavigatorSnapshotInvalid
	}
	defer func() {
		if ctx.Err() != nil {
			out = nil
			err = ctx.Err()
		} else if err != nil && err != ErrNavigatorSnapshotTooLarge {
			out = nil
			err = ErrNavigatorSnapshotUnavailable
		}
	}()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	remainingRows, remainingBytes := limits.Rows, limits.TextBytes
	for _, table := range navigatorTables {
		// The first executed count pins the transaction, including an empty DB.
		var count int64
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name+" WHERE hostname = ?", hostname).Scan(&count); err != nil {
			return nil, err
		}
		if count < 0 || count > remainingRows || count > remainingBytes/256 {
			return nil, ErrNavigatorSnapshotTooLarge
		}
		remainingRows -= count
		remainingBytes -= count * 256
		var checks []string
		for _, name := range table.text {
			checks = append(checks, "typeof("+name+") NOT IN ('null','text')")
		}
		for _, name := range table.ints {
			checks = append(checks, "typeof("+name+") NOT IN ('null','integer')", name+" < 0", name+" > 9007199254740991")
		}
		checks = append(checks, "id IS NULL", "id = 0")
		if table.name == "leases" {
			checks = append(checks, "started_at > 9007199254740", "drain_manual NOT IN (0,1)")
		}
		var invalid bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table.name+" WHERE hostname = ? AND ("+strings.Join(checks, " OR ")+"))", hostname).Scan(&invalid); err != nil {
			return nil, err
		}
		if invalid {
			return nil, ErrNavigatorSnapshotUnavailable
		}
		// Lengths only: reject before selecting any unbounded stored text. Check
		// each column separately, avoiding SQL SUM and arithmetic overflow.
		lengths := make([]string, len(table.text))
		for i, name := range table.text {
			lengths[i] = "length(CAST(COALESCE(" + name + ",'') AS BLOB))"
		}
		rows, e := tx.QueryContext(ctx, "SELECT "+strings.Join(lengths, ",")+" FROM "+table.name+" WHERE hostname = ?", hostname)
		if e != nil {
			return nil, e
		}
		err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
			values := make([]int64, len(lengths))
			dest := make([]any, len(values))
			for i := range values {
				dest[i] = &values[i]
			}
			if e := row.Scan(dest...); e != nil {
				return e
			}
			for _, n := range values {
				if n < 0 || n > remainingBytes {
					return ErrNavigatorSnapshotTooLarge
				}
				remainingBytes -= n
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out = &NavigatorSnapshot{Launches: []NavigatorLaunch{}, Leases: []NavigatorLease{}, hostname: strings.Clone(hostname)}
	rows, err := tx.QueryContext(ctx, `SELECT id,COALESCE(started_at_ms,0),COALESCE(lease_id,0),COALESCE(provider,''),COALESCE(account_id,''),COALESCE(launcher,''),COALESCE(cwd,''),COALESCE(mode,''),COALESCE(session_id,'') FROM launches WHERE hostname = ? ORDER BY started_at_ms,id`, hostname)
	if err != nil {
		return nil, err
	}
	err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var v NavigatorLaunch
		if e := row.Scan(&v.ID, &v.StartedMS, &v.LeaseID, &v.Provider, &v.AccountID, &v.Launcher, &v.Cwd, &v.Mode, &v.SessionID); e != nil {
			return e
		}
		if !navigatorTextValid(v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.SessionID) {
			return ErrNavigatorSnapshotUnavailable
		}
		out.Launches = append(out.Launches, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,COALESCE(started_at,0),COALESCE(pid,0),COALESCE(depth,0),COALESCE(turn_started_at,0),COALESCE(turn_ended_at,0),COALESCE(provider,''),COALESCE(account_id,''),COALESCE(launcher,''),COALESCE(cwd,''),COALESCE(mode,''),COALESCE(workspace,''),COALESCE(pane,''),COALESCE(session_id,''),COALESCE(drain,''),COALESCE(drain_manual,0) FROM leases WHERE hostname = ? ORDER BY started_at,id`, hostname)
	if err != nil {
		return nil, err
	}
	err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var v NavigatorLease
		if e := row.Scan(&v.ID, &v.StartedAt, &v.PID, &v.Depth, &v.TurnStartedAt, &v.TurnEndedAt, &v.Provider, &v.AccountID, &v.Launcher, &v.Cwd, &v.Mode, &v.Workspace, &v.Pane, &v.SessionID, &v.Drain, &v.DrainManual); e != nil {
			return e
		}
		if !navigatorTextValid(v.Provider, v.AccountID, v.Launcher, v.Cwd, v.Mode, v.Workspace, v.Pane, v.SessionID, v.Drain) {
			return ErrNavigatorSnapshotUnavailable
		}
		out.Leases = append(out.Leases, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
func navigatorTextValid(values ...string) bool {
	for _, s := range values {
		if !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}
func navigatorSnapshotRows(ctx context.Context, rows *sql.Rows, read func(*sql.Rows) error) error {
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := read(rows); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}

// NavigatorDirectories describes launch metadata, not a transcript catalog or
// live-state claim. Worker-only directories cannot supply a default scope.
type NavigatorDirectory struct {
	Path      string
	UpdatedMS int64
}

func NavigatorDirectories(s *NavigatorSnapshot) []NavigatorDirectory {
	byPath := map[string]int64{}
	put := func(provider, mode, path string, ms int64) {
		if (provider != "claude" && provider != "codex" && provider != "agy") || (mode != ModeInteractive && mode != ModeLong) || path == "" || !filepath.IsAbs(path) || !navigatorTextValid(path) || ms <= 0 || ms > navigatorMaxInteger {
			return
		}
		path = filepath.Clean(path)
		if prev, ok := byPath[path]; !ok || ms > prev {
			byPath[path] = ms
		}
	}
	if s != nil {
		for _, v := range s.Launches {
			put(v.Provider, v.Mode, v.Cwd, v.StartedMS)
		}
		for _, v := range s.Leases {
			if v.Depth == 0 && v.StartedAt > 0 && v.StartedAt <= navigatorMaxInteger/1000 {
				put(v.Provider, v.Mode, v.Cwd, v.StartedAt*1000)
			}
		}
	}
	out := make([]NavigatorDirectory, 0, len(byPath))
	for path, ms := range byPath {
		out = append(out, NavigatorDirectory{path, ms})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedMS != out[j].UpdatedMS {
			return out[i].UpdatedMS > out[j].UpdatedMS
		}
		return out[i].Path < out[j].Path
	})
	return out
}
