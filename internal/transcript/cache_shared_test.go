package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type observedFlightContext struct {
	context.Context
	seen chan struct{}
	once sync.Once
}

func (c *observedFlightContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.seen) })
	return c.Context.Done()
}

type sharedCallResult struct {
	sess *Session
	snap Snapshot
	sum  Summary
	err  error
}

func sharedCall(c *Cache, ctx context.Context, path string, detail bool) sharedCallResult {
	if detail {
		s, snap, err := c.Session(ctx, path)
		return sharedCallResult{sess: s, snap: snap, err: err}
	}
	s, err := c.Summary(ctx, path)
	return sharedCallResult{sum: s, err: err}
}
func awaitSharedCall(t *testing.T, ch <-chan sharedCallResult) sharedCallResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("shared waiter did not finish")
	}
	return sharedCallResult{}
}
func awaitFlightSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("follower did not reach its wait")
	}
}

func TestListAndDetailShareOneParse(t *testing.T) {
	for _, pair := range []struct {
		name             string
		leader, follower bool
	}{
		{"list-list", false, false}, {"list-detail", false, true}, {"detail-list", true, false}, {"detail-detail", true, true},
	} {
		t.Run(pair.name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			started, release := make(chan string, 8), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			f := &fakeParse{started: started, release: release}
			c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.width = 2 })
			leaderCtx, cancelLeader := context.WithCancel(context.Background())
			defer cancelLeader()
			leader := make(chan sharedCallResult, 1)
			go func() { leader <- sharedCall(c, leaderCtx, path, pair.leader) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("leader did not start")
			}
			followerCtx := &observedFlightContext{Context: context.Background(), seen: make(chan struct{})}
			follower := make(chan sharedCallResult, 1)
			go func() { follower <- sharedCall(c, followerCtx, path, pair.follower) }()
			awaitFlightSignal(t, followerCtx.seen)
			cancelLeader()
			result := awaitSharedCall(t, leader)
			if pair.leader {
				if !errors.Is(result.err, context.Canceled) {
					t.Errorf("cancelled detail: %v", result.err)
				}
			} else if !errors.Is(result.err, ErrBudget) {
				t.Errorf("cancelled list: %v", result.err)
			}
			finish()
			result = awaitSharedCall(t, follower)
			if result.err != nil {
				t.Fatalf("surviving follower: %v", result.err)
			}
			if pair.follower && (result.sess == nil || result.sess.ID != "fixture") {
				t.Fatalf("detail result: %+v", result)
			}
			if !pair.follower && result.sum.ID != "fixture" {
				t.Fatalf("list result: %+v", result)
			}
			if n := f.n.Load(); n != 1 {
				t.Errorf("same file parsed %d times, want one", n)
			}
			st := c.Stats()
			wantFull, wantRows := 0, 0
			if pair.leader || pair.follower {
				wantFull = 1
			}
			if !pair.leader || !pair.follower {
				wantRows = 1
			}
			if st.Parsed != wantFull || st.Rows != wantRows {
				t.Errorf("role admission: %+v, want full %d rows %d", st, wantFull, wantRows)
			}
		})
	}
}

func TestSharedMixedParseErrorRoles(t *testing.T) {
	for _, mode := range []string{"no-session", "read-error", "parser-panic", "row-panic"} {
		t.Run(mode, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			started, release := make(chan string, 8), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			f := &fakeParse{started: started, release: release}
			if mode == "parser-panic" {
				f.panics.Store(path, true)
			}
			c, _ := testCache(t, f, func(cfg *cacheConfig) {
				cfg.width = 2
				if mode == "row-panic" {
					cfg.rowCount = -1
				}
			})
			parse := c.parse
			c.parse = func(path string) (*Session, error) {
				s, err := parse(path)
				if err != nil {
					return s, err
				}
				switch mode {
				case "no-session":
					return nil, nil
				case "read-error":
					return nil, os.ErrPermission
				}
				return s, nil
			}
			leader := make(chan sharedCallResult, 1)
			go func() { leader <- sharedCall(c, context.Background(), path, false) }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("leader did not start")
			}
			ctx := &observedFlightContext{Context: context.Background(), seen: make(chan struct{})}
			follower := make(chan sharedCallResult, 1)
			go func() { follower <- sharedCall(c, ctx, path, true) }()
			awaitFlightSignal(t, ctx.seen)
			finish()
			row, detail := awaitSharedCall(t, leader), awaitSharedCall(t, follower)
			switch mode {
			case "no-session":
				if !errors.Is(row.err, errNoSession) || !errors.Is(detail.err, errNoSession) {
					t.Errorf("no session: row %v detail %v", row.err, detail.err)
				}
			case "read-error":
				if !errors.Is(row.err, os.ErrPermission) || !errors.Is(detail.err, os.ErrPermission) {
					t.Errorf("read error: row %v detail %v", row.err, detail.err)
				}
			case "parser-panic":
				if !errors.Is(row.err, ErrUnreadable) || !errors.Is(detail.err, ErrUnreadable) {
					t.Errorf("parser panic: row %v detail %v", row.err, detail.err)
				}
			case "row-panic":
				if !errors.Is(row.err, ErrUnreadable) || detail.err != nil || detail.sess == nil {
					t.Errorf("row fault changed detail: row %v detail %+v", row.err, detail)
				}
			}
			if n := f.n.Load(); n != 1 {
				t.Errorf("same failed file parsed %d times", n)
			}
			c.mu.Lock()
			_, quarantined := c.quarantine[path]
			c.mu.Unlock()
			if quarantined != (mode == "parser-panic") {
				t.Errorf("quarantine %v for %s", quarantined, mode)
			}
			st := c.Stats()
			if mode == "row-panic" && (st.Parsed != 1 || st.Rows != 0) {
				t.Errorf("row fault admission: %+v", st)
			}
			if mode == "no-session" && (st.Parsed != 0 || st.Rows != 1) {
				t.Errorf("no-session admission: %+v", st)
			}
		})
	}
}

// Admission waits observe registry installation as well as free capacity. The
// second Done call proves the follower has joined before the leader completes.
type stagedFlightContext struct {
	context.Context
	calls         atomic.Int64
	first, second chan struct{}
}

func (c *stagedFlightContext) Done() <-chan struct{} {
	switch c.calls.Add(1) {
	case 1:
		close(c.first)
	case 2:
		close(c.second)
	}
	return c.Context.Done()
}

func TestSharedFlightInstalledWhileFollowerWaitsForCapacity(t *testing.T) {
	for _, leaderDetail := range []bool{false, true} {
		t.Run(fmt.Sprint(leaderDetail), func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			acquired, install, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var installOnce, releaseOnce sync.Once
			allowInstall := func() { installOnce.Do(func() { close(install) }) }
			defer allowInstall()
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			defer finish()
			f := &fakeParse{started: make(chan string, 4), release: release}
			c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.width = 1 })
			c.beforeAdmit = func(string) { close(acquired); <-install }
			leader := make(chan sharedCallResult, 1)
			go func() { leader <- sharedCall(c, context.Background(), path, leaderDetail) }()
			awaitFlightSignal(t, acquired)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &stagedFlightContext{Context: base, first: make(chan struct{}), second: make(chan struct{})}
			follower := make(chan sharedCallResult, 1)
			go func() { follower <- sharedCall(c, ctx, path, !leaderDetail) }()
			awaitFlightSignal(t, ctx.first)
			allowInstall()
			awaitFlightSignal(t, ctx.second)
			cancel()
			result := awaitSharedCall(t, follower)
			want := error(ErrBudget)
			if !leaderDetail {
				want = context.Canceled
			}
			if !errors.Is(result.err, want) {
				t.Fatalf("follower cancellation: %v", result.err)
			}
			finish()
			if r := awaitSharedCall(t, leader); r.err != nil {
				t.Fatal(r.err)
			}
			if n := f.n.Load(); n != 1 {
				t.Fatalf("parses %d", n)
			}
			if st := c.Stats(); st.Parsed != 1 || st.Rows != 1 {
				t.Fatalf("joined role lost: %+v", st)
			}
		})
	}
}

func TestSharedAdmissionStopsBeforeInstallation(t *testing.T) {
	for _, mode := range []string{"list-cancel", "detail-cancel", "detail-timeout"} {
		t.Run(mode, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			acquired, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			f := &fakeParse{}
			c, _ := testCache(t, f, func(cfg *cacheConfig) {
				if mode == "detail-timeout" {
					cfg.wait = 20 * time.Millisecond
				}
			})
			c.beforeAdmit = func(string) { close(acquired); <-release }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan sharedCallResult, 1)
			go func() { result <- sharedCall(c, ctx, path, mode != "list-cancel") }()
			awaitFlightSignal(t, acquired)
			want := error(context.Canceled)
			if mode == "detail-timeout" {
				time.Sleep(40 * time.Millisecond)
				want = ErrPreparing
			} else {
				cancel()
			}
			if mode == "list-cancel" {
				want = ErrBudget
			}
			finish()
			if r := awaitSharedCall(t, result); !errors.Is(r.err, want) {
				t.Fatalf("admission outcome: %v, want %v", r.err, want)
			}
			if n := f.n.Load(); n != 0 {
				t.Fatalf("stopped caller launched %d parses", n)
			}
			if len(c.sem) != 0 || len(c.flights) != 0 {
				t.Fatal("stopped admission retained capacity or work")
			}
		})
	}
}

func TestSharedFlightKeepsOriginalSnapshot(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
	original, err := Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	f := &fakeParse{started: make(chan string, 2), release: release}
	c, _ := testCache(t, f, nil)
	leader := make(chan sharedCallResult, 1)
	go func() { leader <- sharedCall(c, context.Background(), path, false) }()
	select {
	case <-f.started:
	case <-time.After(3 * time.Second):
		t.Fatal("parse did not start")
	}
	appendFile(t, path, "more")
	ctx := &observedFlightContext{Context: context.Background(), seen: make(chan struct{})}
	detail := make(chan sharedCallResult, 1)
	go func() { detail <- sharedCall(c, ctx, path, true) }()
	awaitFlightSignal(t, ctx.seen)
	finish()
	if r := awaitSharedCall(t, leader); r.err != nil {
		t.Fatal(r.err)
	}
	if r := awaitSharedCall(t, detail); r.err != nil || r.snap != original {
		t.Fatalf("flight key changed: %+v", r)
	}
	c.mu.Lock()
	cached := c.rows[path].Value.(*row).snap
	c.mu.Unlock()
	if cached != original {
		t.Fatalf("row key changed: %+v", cached)
	}
	// Summary still requires an exact snapshot despite detail's reparse floor.
	if _, err := c.Summary(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if n := f.n.Load(); n != 2 {
		t.Fatalf("summary inherited detail's floor: %d parses", n)
	}
}

func TestSharedFlightAllWaitersCancelAndOtherPathProgresses(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
	other := writeFile(t, filepath.Join(t.TempDir(), "other.jsonl"), "other invented")
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	f := &fakeParse{started: make(chan string, 4)}
	c, _ := testCache(t, f, nil)
	c.parse = func(p string) (*Session, error) {
		if p == path {
			close(entered)
			<-release
		}
		return f.parse(p)
	}
	leaderBase, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderCtx := &observedFlightContext{Context: leaderBase, seen: make(chan struct{})}
	leader := make(chan sharedCallResult, 1)
	go func() { leader <- sharedCall(c, leaderCtx, path, false) }()
	awaitFlightSignal(t, entered)
	base, cancelFollower := context.WithCancel(context.Background())
	defer cancelFollower()
	ctx := &observedFlightContext{Context: base, seen: make(chan struct{})}
	follower := make(chan sharedCallResult, 1)
	go func() { follower <- sharedCall(c, ctx, path, true) }()
	awaitFlightSignal(t, ctx.seen)
	cancelLeader()
	cancelFollower()
	if r := awaitSharedCall(t, leader); !errors.Is(r.err, ErrBudget) {
		t.Fatal(r.err)
	}
	if r := awaitSharedCall(t, follower); !errors.Is(r.err, context.Canceled) {
		t.Fatal(r.err)
	}
	if _, _, err := c.Session(context.Background(), other); err != nil {
		t.Fatalf("spare slot blocked: %v", err)
	}
	c.mu.Lock()
	done := c.flights[path].done
	c.mu.Unlock()
	finish()
	awaitFlightSignal(t, done)
	if st := c.Stats(); st.Parsed != 2 || st.Rows != 1 {
		t.Fatalf("abandoned admitted roles: %+v", st)
	}
	if len(c.sem) != 0 {
		t.Fatal("admitted task leaked capacity")
	}
}

func TestListDirsCanceledSharedLastCandidate(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	f := &fakeParse{started: make(chan string, 2), release: release}
	c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.width = 1 })
	detail := make(chan sharedCallResult, 1)
	go func() { detail <- sharedCall(c, context.Background(), path, true) }()
	select {
	case <-f.started:
	case <-time.After(3 * time.Second):
		t.Fatal("detail did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan struct{})
	list := make(chan ListResult, 1)
	go func() {
		got, err := ListDirs(ctx, cancelListSource{Discovery: DirectWalk(Roots{}), path: path}, []string{"/w"}, func(ctx context.Context, path string) (Summary, error) {
			return c.Summary(&observedFlightContext{Context: ctx, seen: joined}, path)
		})
		if err != nil {
			t.Error(err)
		}
		list <- got
	}()
	awaitFlightSignal(t, joined)
	cancel()
	select {
	case got := <-list:
		if !got.Partial || got.Of != 1 || got.Scanned != 0 || len(got.Rows) != 0 {
			t.Fatalf("cancelled list: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("list waited for shared parser")
	}
	finish()
	if r := awaitSharedCall(t, detail); r.err != nil || r.sess == nil {
		t.Fatalf("surviving detail: %+v", r)
	}
	if n := f.n.Load(); n != 1 {
		t.Fatalf("parses %d", n)
	}
}
