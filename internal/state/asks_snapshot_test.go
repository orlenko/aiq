package state

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var snapshotNow = time.Date(2031, 4, 5, 21, 0, 0, 0, time.FixedZone("fixture", -4*3600))

func snapshotExec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
func snapshotFixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	snapshotExec(t, s, `INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats)
		VALUES(1,'c','/w','codex','source',100,'open',200,'old','latest',101,'["ref"]',2)`)
	snapshotExec(t, s, `INSERT INTO asks_sessions(provider,session_id,skipped) VALUES('codex','fixture',3)`)
	snapshotExec(t, s, `INSERT INTO asks_usage(day,calls) VALUES('2031-04-06',7)`)
	snapshotExec(t, s, `INSERT INTO asks_watch(id,watched,waiting,last_error,not_watched) VALUES(1,4,5,'oops','["cold"]')`)
	return s, path
}
func snapshotLimits() AsksSnapshotLimits { return AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 20} }
func snapshotRead(t *testing.T, s *Store) *AsksSnapshot {
	t.Helper()
	v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAsksSnapshotLiteralReadOnly(t *testing.T) {
	s, path := snapshotFixture(t)
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	var beforeSchema, afterSchema string
	if err := s.db.QueryRow(`SELECT group_concat(sql) FROM sqlite_schema ORDER BY name`).Scan(&beforeSchema); err != nil {
		t.Fatal(err)
	}
	v := snapshotRead(t, ro) // Actual unwrapped ReadOnly BeginTx driver path.
	want := Ask{ID: 1, Chain: "c", Workspace: "/w", Provider: "codex", SessionID: "source", RaisedAt: 100,
		State: AskOpen, StateAt: 200, FirstQuote: "old", LastQuote: "latest", Refs: []string{"ref"}, Repeats: 2, LastSeen: 101}
	if !reflect.DeepEqual(v.Items, []Ask{want}) || !v.ReportPresent || v.UTCDay != "2031-04-06" ||
		v.Skipped != 3 || v.CallsToday != 7 || v.Status.Skipped != 3 || v.Status.CallsToday != 7 ||
		v.Status.PassAt != 0 || v.Status.Watched != 4 || v.Status.Waiting != 5 || v.Status.LastError != "oops" || !reflect.DeepEqual(v.Status.NotWatched, []string{"cold"}) {
		t.Fatalf("literal snapshot: %#v", v)
	}
	// No application/schema changes: compare a complete second fixture read and schema/event counts.
	if !reflect.DeepEqual(snapshotRead(t, ro), v) {
		t.Fatal("read mutated facts")
	}
	if err := s.db.QueryRow(`SELECT group_concat(sql) FROM sqlite_schema ORDER BY name`).Scan(&afterSchema); err != nil || beforeSchema != afterSchema {
		t.Fatal("schema changed")
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM asks_events`).Scan(&events); err != nil || events != 0 {
		t.Fatal("read wrote events")
	}
	v.Items[0].Refs[0], v.Status.NotWatched[0] = "mutated", "mutated"
	if got := snapshotRead(t, ro); got.Items[0].Refs[0] != "ref" || got.Status.NotWatched[0] != "cold" {
		t.Fatal("returned arrays alias")
	}
	owned := snapshotRead(t, ro)
	if err := ro.Close(); err != nil || owned.Items[0].FirstQuote != "old" || owned.Status.NotWatched[0] != "cold" {
		t.Fatal("owned result lost after closure")
	}
}

func TestAsksSnapshotPresenceOrderAndUTC(t *testing.T) {
	s, _ := snapshotFixture(t)
	snapshotExec(t, s, `DELETE FROM asks_watch`)
	v := snapshotRead(t, s)
	if v.ReportPresent || v.Status.NotWatched == nil || v.Skipped != 3 || v.Status.Skipped != 3 || v.CallsToday != 7 || v.Status.CallsToday != 7 {
		t.Fatal("absent report erased known 3/7 totals")
	}
	snapshotExec(t, s, `DELETE FROM asks_usage`)
	v = snapshotRead(t, s)
	if v.CallsToday != 0 || v.Status.CallsToday != 0 {
		t.Fatal("missing usage is not zero")
	}
	snapshotExec(t, s, `INSERT INTO asks_usage(day,calls) VALUES('2031-04-05',99),('2031-04-06',0)`)
	if snapshotRead(t, s).CallsToday != 0 {
		t.Fatal("used local day or confused explicit zero")
	}
	snapshotExec(t, s, `INSERT INTO asks SELECT 2,'c2',workspace,provider,session_id,raised_at,state,state_at,state_reason,state_text,first_quote,last_quote,asks_reader,risk,refs,repeats,last_seen,repeat_at FROM asks WHERE id=1`)
	snapshotExec(t, s, `INSERT INTO asks SELECT 3,'c3',workspace,provider,session_id,101,state,state_at,state_reason,state_text,first_quote,last_quote,asks_reader,risk,refs,repeats,last_seen,repeat_at FROM asks WHERE id=1`)
	v = snapshotRead(t, s)
	if got := []int64{v.Items[0].ID, v.Items[1].ID, v.Items[2].ID}; !reflect.DeepEqual(got, []int64{3, 2, 1}) {
		t.Fatal(got)
	}
	snapshotExec(t, s, `DELETE FROM asks; DELETE FROM asks_sessions`)
	v = snapshotRead(t, s)
	if v.Items == nil || len(v.Items) != 0 || v.Status.NotWatched == nil || v.Skipped != 0 {
		t.Fatal("empty arrays/totals")
	}
}

func TestAsksSnapshotAdmissionBeforePayload(t *testing.T) {
	for _, tc := range []struct {
		name, update string
		bytes        int64
	}{{"ordinary", "", 46}, {"multibyte", `UPDATE asks SET chain='😀'`, 49}, {"report only", `DELETE FROM asks`, 12}} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := snapshotFixture(t)
			if tc.update != "" {
				snapshotExec(t, s, tc.update)
			}
			p := &asksSnapshotProbe{}
			wrapped := asksSnapshotWrapped(t, path, p)
			if v, err := wrapped.ReadAsksSnapshot(context.Background(), snapshotNow, AsksSnapshotLimits{1, tc.bytes}); err != nil || v == nil {
				t.Fatalf("literal exact byte allowance%d: %v", tc.bytes, err)
			}
			p.full.Store(0)
			if v, err := wrapped.ReadAsksSnapshot(context.Background(), snapshotNow, AsksSnapshotLimits{1, tc.bytes - 1}); v != nil || err != ErrAsksSnapshotTooLarge || p.full.Load() != 0 {
				t.Fatalf("one-short before ANY payload: %v queries%d", err, p.full.Load())
			}
		})
	}
	s, path := snapshotFixture(t)
	snapshotExec(t, s, `INSERT INTO asks SELECT 2,'c2','/excluded',provider,session_id,raised_at,state,state_at,state_reason,state_text,first_quote,last_quote,asks_reader,risk,refs,repeats,last_seen,repeat_at FROM asks WHERE id=1`)
	p := &asksSnapshotProbe{}
	w := asksSnapshotWrapped(t, path, p)
	if v, err := w.ReadAsksSnapshot(context.Background(), snapshotNow, AsksSnapshotLimits{1, 1000}); v != nil || err != ErrAsksSnapshotTooLarge || p.full.Load() != 0 {
		t.Fatal("row count bypassed or partial output")
	}
}

func TestAsksSnapshotStorageTypesBeforePayload(t *testing.T) {
	for _, spec := range []struct{ table, fields string }{
		{"asks", "chain,workspace,provider,session_id,raised_at,state,state_at,state_reason,state_text,first_quote,last_quote,asks_reader,risk,refs,repeats,last_seen,repeat_at"},
		{"asks_watch", "pass_at,ok_at,watched,waiting,next_try_at,error_at,last_error,not_watched"},
		{"asks_usage", "calls"}, {"asks_sessions", "skipped"},
	} {
		for _, column := range strings.Split(spec.fields, ",") {
			t.Run(spec.table+"/"+column, func(t *testing.T) {
				s, path := snapshotFixture(t)
				snapshotExec(t, s, "UPDATE "+spec.table+" SET "+column+" = ?", []byte(strings.Repeat("invented malformed value", 100)))
				p := &asksSnapshotProbe{}
				w := asksSnapshotWrapped(t, path, p)
				if v, err := w.ReadAsksSnapshot(context.Background(), snapshotNow, AsksSnapshotLimits{10, 1}); v != nil || err != ErrAsksSnapshotUnavailable || p.full.Load() != 0 {
					t.Fatalf("wrong class was transferred/charged late: %v queries%d", err, p.full.Load())
				}
			})
		}
	}
	for _, column := range []string{"id", "raised_at"} {
		t.Run("unconstrained/"+column, func(t *testing.T) {
			s, path := snapshotFixture(t)
			// An invented malformed schema permits NULL/wrong primary-key storage.
			snapshotExec(t, s, `CREATE TABLE copy AS SELECT * FROM asks; DROP TABLE asks; ALTER TABLE copy RENAME TO asks`)
			snapshotExec(t, s, "UPDATE asks SET "+column+" = NULL")
			p := &asksSnapshotProbe{}
			if v, err := asksSnapshotWrapped(t, path, p).ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits()); v != nil || err != ErrAsksSnapshotUnavailable || p.full.Load() != 0 {
				t.Fatal("NULL storage passed")
			}
		})
	}
	for _, spec := range []struct{ table, column string }{{"asks", "repeats"}, {"asks_watch", "watched"}, {"asks_usage", "calls"}} {
		s, path := snapshotFixture(t)
		snapshotExec(t, s, "UPDATE "+spec.table+" SET "+spec.column+"=?", strings.Repeat("invented nonnumeric text", 100))
		p := &asksSnapshotProbe{}
		if v, err := asksSnapshotWrapped(t, path, p).ReadAsksSnapshot(context.Background(), snapshotNow, AsksSnapshotLimits{10, 1}); v != nil || err != ErrAsksSnapshotUnavailable || p.full.Load() != 0 {
			t.Fatal("numeric TEXT payload was transferred before refusal")
		}
	}
}

func TestAsksSnapshotCountsAndFiniteScores(t *testing.T) {
	for _, q := range []string{
		`UPDATE asks SET repeats=-1`, `UPDATE asks_watch SET watched=-1`, `UPDATE asks_watch SET waiting=-1`,
		`UPDATE asks_usage SET calls=-1`, `UPDATE asks_usage SET calls='wrong'`, `UPDATE asks_sessions SET skipped=0.5`,
		`UPDATE asks_sessions SET skipped=-1; INSERT INTO asks_sessions(provider,session_id,skipped) VALUES('codex','other',2); DELETE FROM asks_watch`,
		`UPDATE asks_sessions SET skipped=9223372036854775807; INSERT INTO asks_sessions(provider,session_id,skipped) VALUES('codex','other',1)`,
	} {
		s, _ := snapshotFixture(t)
		snapshotExec(t, s, q)
		if v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits()); v != nil || err != ErrAsksSnapshotUnavailable {
			t.Fatalf("malformed contributor/count: %#v %v", v, err)
		}
	}
	s, _ := snapshotFixture(t)
	snapshotExec(t, s, `UPDATE asks SET risk=?`, math.Inf(1))
	if v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits()); v != nil || err != ErrAsksSnapshotUnavailable {
		t.Fatal("nonfinite score accepted")
	}
	snapshotExec(t, s, `UPDATE asks SET risk=2,asks_reader=-1,raised_at=-123`)
	if v := snapshotRead(t, s); v.Items[0].Risk != 2 || v.Items[0].AsksReader != -1 || v.Items[0].RaisedAt != -123 {
		t.Fatal("reader imposed score ranges or changed signed time")
	}
}

func TestAsksSnapshotStrictJSONBothCarriers(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string
	}{
		{`[]`, []string{}}, {` ["", "a", "a"] `, []string{"", "a", "a"}},
		{`["\ud83d\ude00", "�", "\\uD800", "\u0000\n\t"]`, []string{"😀", "�", `\uD800`, "\x00\n\t"}},
		{`null`, nil}, {`[null]`, nil}, {`["a",null]`, nil}, {`[[]]`, nil}, {`[1]`, nil}, {`{}`, nil},
		{`["\ud800"]`, nil}, {`["\udc00"]`, nil}, {`["\ud800\u0000"]`, nil}, {`["\ud800\ud800"]`, nil},
		{`["\q"]`, nil}, {`[] []`, nil}, {``, nil}, {"[\"\xff\"]", nil},
	} {
		for _, carrier := range []struct{ table, column string }{{"asks", "refs"}, {"asks_watch", "not_watched"}} {
			s, _ := snapshotFixture(t)
			snapshotExec(t, s, "UPDATE "+carrier.table+" SET "+carrier.column+"=?", tc.raw)
			v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits())
			if tc.want == nil {
				if v != nil || err != ErrAsksSnapshotUnavailable {
					t.Fatalf("%s malformed JSON%q: %#v %v", carrier.column, tc.raw, v, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			got := v.Items[0].Refs
			if carrier.column == "not_watched" {
				got = v.Status.NotWatched
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("%s literal array%q: %#v", carrier.column, tc.raw, got)
			}
		}
	}
}

func TestAsksSnapshotFailuresReleaseAndCancellation(t *testing.T) {
	_, path := snapshotFixture(t)
	for _, fault := range []string{"iteration", "close", "commit", "after-commit"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &asksSnapshotProbe{fault: fault, cancel: cancel}
			s := asksSnapshotWrapped(t, path, p)
			v, err := s.ReadAsksSnapshot(ctx, snapshotNow, snapshotLimits())
			want := ErrAsksSnapshotUnavailable
			if fault == "after-commit" {
				want = context.Canceled
			}
			if v != nil || err != want || strings.Contains(err.Error(), "private") {
				t.Fatalf("partial/leaked failure: %#v %v", v, err)
			}
			p.fault = ""
			if s.db.Stats().InUse != 0 {
				t.Fatal("affected connection still checked out")
			}
			ctx2, done := context.WithTimeout(context.Background(), time.Second)
			defer done()
			if _, err := s.ReadAsksSnapshot(ctx2, snapshotNow, snapshotLimits()); err != nil {
				t.Fatal("single affected pool slot unusable:", err)
			}
			if p.opened.Load()-p.closed.Load() != 1 {
				t.Fatal("discarded or reused connection accounting inconsistent")
			}
		})
	}
	s, _ := snapshotFixture(t)
	for _, limits := range []AsksSnapshotLimits{{0, 1}, {-1, 1}, {1, 0}, {1, -1}} {
		if v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, limits); v != nil || err != ErrAsksSnapshotInvalid {
			t.Fatal("invalid allowance")
		}
	}
	if _, err := s.ReadAsksSnapshot(nil, snapshotNow, snapshotLimits()); err != ErrAsksSnapshotInvalid {
		t.Fatal("nil context")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v, err := s.ReadAsksSnapshot(ctx, snapshotNow, snapshotLimits()); v != nil || err != context.Canceled {
		t.Fatal("entry cancellation")
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	if v, err := s.ReadAsksSnapshot(ctx, snapshotNow, snapshotLimits()); v != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline classification")
	}
	snapshotExec(t, s, `DROP TABLE asks_watch`)
	if v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits()); v != nil || err != ErrAsksSnapshotUnavailable {
		t.Fatal("missing schema became empty success")
	}
}

func TestAsksSnapshotPinnedWALAndIndependentReaders(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "initially empty"}[empty], func(t *testing.T) {
			s, path := snapshotFixture(t)
			if empty {
				snapshotExec(t, s, `DELETE FROM asks`)
			}
			p := &asksSnapshotProbe{pinned: make(chan struct{}), resume: make(chan struct{})}
			r := asksSnapshotWrapped(t, path, p)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type result struct {
				v   *AsksSnapshot
				err error
			}
			results := make(chan result, 1)
			go func() { v, err := r.ReadAsksSnapshot(ctx, snapshotNow, snapshotLimits()); results <- result{v, err} }()
			select {
			case <-p.pinned:
			case <-ctx.Done():
				t.Fatal("reader did not pin")
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = tx.ExecContext(ctx, `UPDATE asks SET first_quote='new',last_quote='new latest',state='closed',state_at=999;
				UPDATE asks_watch SET watched=8,last_error='new error'; UPDATE asks_sessions SET skipped=9; UPDATE asks_usage SET calls=11`)
			if empty && err == nil {
				_, err = tx.ExecContext(ctx, `INSERT INTO asks(chain,raised_at,state,state_at,first_quote,last_quote,last_seen) VALUES('new',999,'open',999,'new','new latest',999)`)
			}
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal("writer must commit BEFORE resume:", err)
			}
			close(p.resume)
			old := <-results
			if old.err != nil || old.v.Status.Watched != 4 || old.v.Status.LastError != "oops" || old.v.Skipped != 3 || old.v.CallsToday != 7 || old.v.Status.Skipped != 3 || old.v.Status.CallsToday != 7 {
				t.Fatalf("mixed old tuple: %#v %v", old.v, old.err)
			}
			if empty {
				if len(old.v.Items) != 0 {
					t.Fatal("empty pin saw insertion")
				}
			} else if old.v.Items[0].FirstQuote != "old" || old.v.Items[0].LastQuote != "latest" || old.v.Items[0].State != AskOpen || old.v.Items[0].StateAt != 200 {
				t.Fatal("mixed old item")
			}
			// A separate unwrapped read sees the complete later committed tuple.
			newer := snapshotRead(t, s)
			if newer.Items[0].FirstQuote != "new" || newer.Items[0].LastQuote != "new latest" || newer.Status.Watched != 8 || newer.Status.LastError != "new error" || newer.Skipped != 9 || newer.CallsToday != 11 {
				t.Fatal("later tuple missing")
			}
			if !empty && (newer.Items[0].State != AskClosed || newer.Items[0].StateAt != 999) {
				t.Fatal("later item state missing")
			}
		})
	}
	s, _ := snapshotFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := s.ReadAsksSnapshot(context.Background(), snapshotNow, snapshotLimits())
			if err != nil || v.CallsToday != 7 {
				t.Errorf("independent read: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestAsksSnapshotCanceledReaderDoesNotCancelAnother(t *testing.T) {
	s, path := snapshotFixture(t)
	p := &asksSnapshotProbe{pinned: make(chan struct{}), resume: make(chan struct{})}
	r := asksSnapshotWrapped(t, path, p)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		v, err := r.ReadAsksSnapshot(ctx, snapshotNow, snapshotLimits())
		if v != nil {
			finished <- errors.New("published canceled snapshot")
			return
		}
		finished <- err
	}()
	select {
	case <-p.pinned:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not pin")
	}
	if other := snapshotRead(t, s); other.CallsToday != 7 {
		t.Fatal("independent reader changed")
	}
	cancel()
	select {
	case err := <-finished:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled reader did not finish")
	}
	p.pinned, p.resume = nil, nil
	healthy, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	if _, err := r.ReadAsksSnapshot(healthy, snapshotNow, snapshotLimits()); err != nil || r.db.Stats().InUse != 0 || p.opened.Load()-p.closed.Load() != 1 {
		t.Fatal("affected single connection was neither reused nor replaced cleanly:", err)
	}
}
