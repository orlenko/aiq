package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

// Test-only forwarding wrapper: actual SQLite supplies all data/transactions.
type asksSnapshotProbe struct {
	full, opened, closed atomic.Int64
	resets, validations  atomic.Int64
	badResets, invalid   atomic.Int64
	lastOpen, lastClose  atomic.Int64
	lastReset, lastValid atomic.Int64
	lastInvalid          atomic.Int64
	pinned, resume       chan struct{}
	fault                string
	cancel               context.CancelFunc
	pinQuery, fullPrefix string // Optional selectors for other read-only snapshot fixtures.
}
type asksSnapshotConnector struct {
	path string
	p    *asksSnapshotProbe
}

func (c asksSnapshotConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c asksSnapshotConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open("file:" + c.path + "?mode=ro")
	if err != nil {
		return nil, err
	}
	id := c.p.opened.Add(1)
	c.p.lastOpen.Store(id)
	return &asksSnapshotConn{Conn: conn, p: c.p, id: id}, nil
}

type asksSnapshotConn struct {
	driver.Conn
	p  *asksSnapshotProbe
	id int64
}

func (c *asksSnapshotConn) Close() error {
	c.p.closed.Add(1)
	c.p.lastClose.Store(c.id)
	return c.Conn.Close()
}
func (c *asksSnapshotConn) ResetSession(ctx context.Context) error {
	c.p.resets.Add(1)
	c.p.lastReset.Store(c.id)
	err := c.Conn.(driver.SessionResetter).ResetSession(ctx)
	if err == driver.ErrBadConn {
		c.p.badResets.Add(1)
		c.p.lastInvalid.Store(c.id)
	}
	return err
}
func (c *asksSnapshotConn) IsValid() bool {
	c.p.validations.Add(1)
	c.p.lastValid.Store(c.id)
	valid := c.Conn.(driver.Validator).IsValid()
	if !valid {
		c.p.invalid.Add(1)
		c.p.lastInvalid.Store(c.id)
	}
	return valid
}
func (c *asksSnapshotConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &asksSnapshotTx{Tx: tx, p: c.p}, nil
}
func (c *asksSnapshotConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	full := strings.HasPrefix(q, "SELECT "+askColumns+" FROM asks") || strings.HasPrefix(q, "SELECT pass_at,")
	if c.p.fullPrefix != "" {
		full = strings.HasPrefix(q, c.p.fullPrefix)
	}
	if full {
		c.p.full.Add(1)
	}
	r, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	if err != nil {
		return nil, err
	}
	pinQuery := c.p.pinQuery
	if pinQuery == "" {
		pinQuery = "SELECT COUNT(*) FROM asks"
	}
	return &asksSnapshotDriverRows{Rows: r, p: c.p, pin: q == pinQuery, full: full, ctx: ctx}, nil
}

type asksSnapshotTx struct {
	driver.Tx
	p *asksSnapshotProbe
}

func (tx *asksSnapshotTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.p.fault == "commit" {
		return errors.New("invented private commit failure")
	}
	if tx.p.fault == "after-commit" {
		tx.p.cancel()
	}
	return nil
}

type asksSnapshotDriverRows struct {
	driver.Rows
	p         *asksSnapshotProbe
	pin, full bool
	done      bool
	ctx       context.Context
}

func (r *asksSnapshotDriverRows) Next(dest []driver.Value) error {
	if r.full && r.p.fault == "iteration" {
		return errors.New("invented private iteration failure")
	}
	err := r.Rows.Next(dest)
	if r.pin && !r.done && err == nil {
		r.done = true
		if r.p.pinned != nil {
			close(r.p.pinned)
			select {
			case <-r.p.resume:
			case <-r.ctx.Done():
				return r.ctx.Err()
			}
		}
	}
	return err
}
func (r *asksSnapshotDriverRows) Close() error {
	err := r.Rows.Close()
	if r.full && r.p.fault == "close" && err == nil {
		return errors.New("invented private close failure")
	}
	return err
}

func asksSnapshotWrapped(t *testing.T, path string, p *asksSnapshotProbe) *Store {
	t.Helper()
	db := sql.OpenDB(asksSnapshotConnector{path: path, p: p})
	db.SetMaxOpenConns(1) // Cleanup evidence concerns this connection, not another pool slot.
	t.Cleanup(func() { db.Close() })
	return &Store{db: db}
}

var _ driver.QueryerContext = (*asksSnapshotConn)(nil)
var _ driver.ConnBeginTx = (*asksSnapshotConn)(nil)
var _ driver.SessionResetter = (*asksSnapshotConn)(nil)
var _ driver.Validator = (*asksSnapshotConn)(nil)
