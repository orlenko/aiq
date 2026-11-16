package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

var (
	ErrQuotaSnapshotInvalid     = errors.New("invalid quota snapshot request")
	ErrQuotaSnapshotTooLarge    = errors.New("quota snapshot too large")
	ErrQuotaSnapshotUnavailable = errors.New("quota snapshot unavailable")
)

// QuotaSnapshotLimits are admission allowances for one read, not SQLite or
// process memory bounds. Rows counts every selected row across the tables;
// TextBytes counts every selected text value. The caller supplies both.
type QuotaSnapshotLimits struct{ Rows, TextBytes int64 }

// QuotaSnapshot is the store side of the quota view, read from one committed
// view. Leases carry neither launch arguments, the transcript path nor the
// takeover order: nothing that reads a snapshot can show them.
type QuotaSnapshot struct {
	Accounts []Account // ORDER BY provider, priority, name, as ListAccounts
	Usage    map[string]Usage
	Windows  map[string][]Window // per account, ORDER BY kind, key, as ListWindows
	Leases   []Lease             // ORDER BY started_at, id
	Whys     map[LeaseRef]string
}

type quotaTable struct {
	name string
	text []string
}

// The text columns each select reads, admitted by length before any is read.
var quotaTables = []quotaTable{
	{"accounts", []string{"id", "provider", "name", "home", "quota_id", "identity"}},
	{"usage", []string{"account_id", "exhausted_reason", "reset_credit_id", "plan", "poll_error", "extra_usage", "cli_login", "cli_identity", "cli_login_error"}},
	{"windows", []string{"account_id", "key", "label", "kind", "scope", "severity", "source"}},
	{"leases", []string{"account_id", "hostname", "mode", "cwd", "parent_account", "workspace", "pane", "session_id", "provider", "drain", "launcher", "why"}},
}

// ReadQuotaSnapshot reads accounts, usage, windows and leases from one
// read-only transaction. It counts rows and measures every text value it will
// select before selecting any, so an oversized store is refused before its
// text is held. It writes nothing, prunes no lease and asks no provider.
func (s *Store) ReadQuotaSnapshot(ctx context.Context, limits QuotaSnapshotLimits) (out *QuotaSnapshot, err error) {
	if ctx == nil || s == nil || s.db == nil || limits.Rows <= 0 || limits.TextBytes <= 0 {
		return nil, ErrQuotaSnapshotInvalid
	}
	defer func() {
		if ctx.Err() != nil {
			out = nil
			err = ctx.Err()
		} else if err != nil && err != ErrQuotaSnapshotTooLarge {
			out = nil
			err = ErrQuotaSnapshotUnavailable
		}
	}()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rowsLeft, bytesLeft := limits.Rows, limits.TextBytes
	for _, table := range quotaTables {
		var count int64
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&count); err != nil {
			return nil, err
		}
		if count < 0 || count > rowsLeft {
			return nil, ErrQuotaSnapshotTooLarge
		}
		rowsLeft -= count
		lengths := make([]string, len(table.text))
		for i, name := range table.text {
			lengths[i] = "length(CAST(COALESCE(" + name + ",'') AS BLOB))"
		}
		rows, e := tx.QueryContext(ctx, "SELECT "+strings.Join(lengths, ",")+" FROM "+table.name)
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
				if n < 0 || n > bytesLeft {
					return ErrQuotaSnapshotTooLarge
				}
				bytesLeft -= n
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out = &QuotaSnapshot{Accounts: []Account{}, Usage: map[string]Usage{}, Windows: map[string][]Window{}, Leases: []Lease{}, Whys: map[LeaseRef]string{}}
	rows, err := tx.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts ORDER BY provider, priority, name`)
	if err != nil {
		return nil, err
	}
	err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		a, e := scanAccount(row)
		if e != nil {
			return e
		}
		if !navigatorTextValid(a.ID, a.Provider, a.Name, a.Home, a.QuotaID, a.Identity) {
			return ErrQuotaSnapshotUnavailable
		}
		out.Accounts = append(out.Accounts, a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The same columns and decoding as GetUsage.
	rows, err = tx.QueryContext(ctx,
		`SELECT account_id, exhausted, COALESCE(exhausted_reason,''), COALESCE(cooldown_until,0),
		        reset_credits, COALESCE(reset_credit_expires_at,0), COALESCE(reset_credit_id,''),
		        COALESCE(plan,''), COALESCE(poll_error,''), COALESCE(observed_at,0), COALESCE(exhausted_at,0),
		        COALESCE(extra_usage,''), COALESCE(cli_login,''), COALESCE(cli_identity,''), COALESCE(cli_checked_at,0),
		        COALESCE(cli_login_error,'')
		 FROM usage`)
	if err != nil {
		return nil, err
	}
	err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var u Usage
		var exhausted int
		var extra string
		if e := row.Scan(&u.AccountID, &exhausted, &u.ExhaustedReason, &u.CooldownUntil,
			&u.ResetCredits, &u.ResetCreditExpiry, &u.ResetCreditID, &u.Plan, &u.PollError, &u.ObservedAt, &u.ExhaustedAt,
			&extra, &u.CLILogin, &u.CLIIdentity, &u.CLICheckedAt, &u.CLILoginError); e != nil {
			return e
		}
		if !navigatorTextValid(u.AccountID, u.ExhaustedReason, u.ResetCreditID, u.Plan, u.PollError, extra, u.CLILogin, u.CLIIdentity, u.CLILoginError) {
			return ErrQuotaSnapshotUnavailable
		}
		u.Exhausted = exhausted != 0
		if extra != "" {
			var e ExtraUsage
			if json.Unmarshal([]byte(extra), &e) == nil {
				u.Extra = &e
			}
		}
		out.Usage[u.AccountID] = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx,
		`SELECT account_id, key, COALESCE(label,''), kind, COALESCE(scope,''), COALESCE(used_pct,-1),
		        COALESCE(resets_at,0), COALESCE(window_seconds,0), COALESCE(severity,''), COALESCE(source,''), COALESCE(observed_at,0)
		 FROM windows ORDER BY account_id, kind, key`)
	if err != nil {
		return nil, err
	}
	err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var w Window
		if e := row.Scan(&w.AccountID, &w.Key, &w.Label, &w.Kind, &w.Scope, &w.UsedPct,
			&w.ResetsAt, &w.WindowSeconds, &w.Severity, &w.Source, &w.ObservedAt); e != nil {
			return e
		}
		if !navigatorTextValid(w.AccountID, w.Key, w.Label, w.Kind, w.Scope, w.Severity, w.Source) {
			return ErrQuotaSnapshotUnavailable
		}
		out.Windows[w.AccountID] = append(out.Windows[w.AccountID], w)
		return nil
	})
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx,
		`SELECT id, account_id, COALESCE(pid,0), COALESCE(hostname,''), COALESCE(mode,''), COALESCE(cwd,''),
		        depth, COALESCE(parent_account,''), COALESCE(root_id,0), COALESCE(started_at,0),
		        COALESCE(workspace,''), COALESCE(pane,''), COALESCE(session_id,''), COALESCE(provider,''),
		        COALESCE(drain,''), COALESCE(drain_at,0), COALESCE(turn_started_at,0), COALESCE(turn_ended_at,0), COALESCE(takeover_of,0),
		        COALESCE(launcher,''), COALESCE(drain_manual,0) != 0, COALESCE(why,'')
		 FROM leases ORDER BY started_at, id`)
	if err != nil {
		return nil, err
	}
	err = navigatorSnapshotRows(ctx, rows, func(row *sql.Rows) error {
		var l Lease
		var why string
		if e := row.Scan(&l.ID, &l.AccountID, &l.PID, &l.Hostname, &l.Mode, &l.Cwd,
			&l.Depth, &l.ParentAccount, &l.RootID, &l.StartedAt,
			&l.Workspace, &l.Pane, &l.SessionID, &l.Provider,
			&l.Drain, &l.DrainAt, &l.TurnStartedAt, &l.TurnEndedAt, &l.TakeoverOf, &l.Launcher, &l.DrainManual, &why); e != nil {
			return e
		}
		if !navigatorTextValid(l.AccountID, l.Hostname, l.Mode, l.Cwd, l.ParentAccount, l.Workspace, l.Pane, l.SessionID, l.Provider, l.Drain, l.Launcher, why) {
			return ErrQuotaSnapshotUnavailable
		}
		out.Leases = append(out.Leases, l)
		if why != "" {
			out.Whys[LeaseRef{ID: l.ID, StartedAt: l.StartedAt}] = why
		}
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
