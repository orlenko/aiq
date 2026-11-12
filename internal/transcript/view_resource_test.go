package transcript

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

type viewResult struct {
	v   *View
	err error
}

func viewKey(path string) ViewKey {
	return ViewKey{Snap: Snapshot{Path: path, Size: 1, Mtime: 1}, Last: 50}
}
func simpleView() (*View, error) { return &View{ETag: "full", Bytes: 1, Settled: true}, nil }
func receiveView(t *testing.T, ch <-chan viewResult) viewResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(3 * time.Second):
		t.Fatal("view did not finish")
		return viewResult{}
	}
}
func signalView(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("view barrier did not arrive")
	}
}
func finishViews(t *testing.T, c *Cache) {
	t.Helper()
	c.mu.Lock()
	var done []<-chan struct{}
	for _, f := range c.building {
		done = append(done, f.done)
	}
	c.mu.Unlock()
	for _, ch := range done {
		signalView(t, ch)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.building) != 0 || len(c.viewSem) != 0 {
		t.Fatal("terminal cleanup left a worker or slot")
	}
}

func TestViewResourceAdmissionAndCanceledWaiters(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var active, most atomic.Int64
	build := func() (*View, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		started <- struct{}{}
		<-release
		return simpleView()
	}
	for _, path := range []string{"a", "b"} {
		ctx, stop := context.WithCancel(context.Background())
		result := make(chan viewResult, 1)
		go func() { v, err := c.ViewContext(ctx, viewKey(path), build); result <- viewResult{v, err} }()
		signalView(t, started)
		stop()
		if r := receiveView(t, result); !errors.Is(r.err, context.Canceled) {
			t.Fatal(r)
		}
	}
	ctx, stop := context.WithCancel(context.Background())
	calls := make(chan struct{}, 4)
	queued := make(chan viewResult, 1)
	go func() {
		v, e := c.ViewContext(observedViewContext{ctx, calls}, viewKey("queued"), build)
		queued <- viewResult{v, e}
	}()
	signalView(t, calls)
	stop()
	if r := receiveView(t, queued); !errors.Is(r.err, context.Canceled) {
		t.Fatal(r)
	}
	if st := c.Stats(); st.BuildingViews != 2 || st.ViewOwners != 2 {
		t.Fatal(st)
	}
	// A follower can attach without a third builder slot, then leave independently.
	followerCtx, cancelFollower := context.WithCancel(context.Background())
	follower := make(chan viewResult, 1)
	attached := make(chan struct{}, 4)
	go func() {
		v, err := c.ViewContext(observedViewContext{followerCtx, attached}, viewKey("a"), build)
		follower <- viewResult{v, err}
	}()
	signalView(t, attached)
	cancelFollower()
	if r := receiveView(t, follower); !errors.Is(r.err, context.Canceled) {
		t.Fatal(r)
	}
	if st := c.Stats(); st.BuildingViews != 2 {
		t.Fatal("attached cancellation released active work", st)
	}
	survivor, joined := make(chan viewResult, 1), make(chan struct{}, 4)
	go func() {
		v, err := c.ViewContext(observedViewContext{context.Background(), joined}, viewKey("a"), build)
		survivor <- viewResult{v, err}
	}()
	signalView(t, joined)
	if most.Load() != 2 {
		t.Fatalf("peak=%d", most.Load())
	}
	close(release)
	if r := receiveView(t, survivor); r.err != nil || r.v.ETag != "full" {
		t.Fatal("remaining follower lost original result", r)
	}
	finishViews(t, c)
	if _, err := c.View(viewKey("queued"), simpleView); err != nil {
		t.Fatal(err)
	}
}

// Done observations establish that the second caller entered admission before
// installation, then woke to attach while the first builder still held its slot.
type observedViewContext struct {
	context.Context
	calls chan struct{}
}

func (ctx observedViewContext) Done() <-chan struct{} {
	ctx.calls <- struct{}{}
	return ctx.Context.Done()
}
func TestViewResourceInitialMissJoinsOnInstallation(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewWidth = 1 })
	borrowed, install, started, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	c.beforeViewAdmit = func(ViewKey) { close(borrowed); <-install }
	sentinel := errors.New("ordinary render error")
	var builds atomic.Int64
	build := func() (*View, error) { builds.Add(1); close(started); <-release; return nil, sentinel }
	first, second := make(chan viewResult, 1), make(chan viewResult, 1)
	go func() { v, e := c.View(viewKey("same"), build); first <- viewResult{v, e} }()
	signalView(t, borrowed)
	calls := make(chan struct{}, 4)
	ctx := observedViewContext{context.Background(), calls}
	go func() { v, e := c.ViewContext(ctx, viewKey("same"), build); second <- viewResult{v, e} }()
	signalView(t, calls) // blocked on slot or changed
	close(install)
	signalView(t, started)
	signalView(t, calls) // now attached; builder has not completed
	close(release)
	for _, ch := range []<-chan viewResult{first, second} {
		if r := receiveView(t, ch); r.err != sentinel {
			t.Fatal(r)
		}
	}
	finishViews(t, c)
	if builds.Load() != 1 {
		t.Fatalf("%d builds for concurrent misses", builds.Load())
	}
}

func TestViewResourceCancellationHasPriority(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	k := viewKey("positive")
	if _, err := c.View(k, simpleView); err != nil {
		t.Fatal(err)
	}
	negative := viewKey("negative")
	c.View(negative, func() (*View, error) { panic("fixture") })
	ctx, stop := context.WithCancel(context.Background())
	stop()
	for _, key := range []ViewKey{k, negative, viewKey("miss")} {
		if v, err := c.ViewContext(ctx, key, simpleView); v != nil || !errors.Is(err, context.Canceled) {
			t.Fatal(v, err)
		}
	}
	f := &viewFlight{done: make(chan struct{}), view: &View{ETag: "ready"}}
	close(f.done)
	for i := 0; i < 100; i++ {
		if _, err := waitView(ctx, f); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	// Cancellation after borrowing but before installation must return the token.
	fresh, _ := testCache(t, &fakeParse{}, nil)
	preCtx, cancel := context.WithCancel(context.Background())
	fresh.beforeViewAdmit = func(ViewKey) { cancel() }
	if _, err := fresh.ViewContext(preCtx, viewKey("preinstall"), simpleView); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if st := fresh.Stats(); st.BuildingViews != 0 || st.ViewOwners != 0 || len(fresh.viewSem) != 0 {
		t.Fatal(st)
	}
}

func TestViewResourceOutcomeAndWholeWorkerCleanup(t *testing.T) {
	for _, fault := range []string{"first-clock", "after-build-clock", "publication-clock", "publication-hook", "ordinary-after-clock"} {
		t.Run(fault, func(t *testing.T) {
			c, clk := testCache(t, &fakeParse{}, nil)
			var calls, builds atomic.Int64
			target := int64(1)
			switch fault {
			case "after-build-clock", "ordinary-after-clock":
				target = 2
			case "publication-clock":
				target = 3
			case "publication-hook":
				target = 0
				c.beforeViewPublish = func() { panic("publication fixture") }
			}
			c.now = func() time.Time {
				if calls.Add(1) == target {
					panic("timing fixture")
				}
				return clk.Now()
			}
			partial := &View{Value: "partial"}
			sentinel := errors.New("ordinary failure")
			build := func() (*View, error) {
				builds.Add(1)
				if fault == "ordinary-after-clock" {
					return partial, sentinel
				}
				return simpleView()
			}
			v, err := c.View(viewKey("fault"), build)
			finishViews(t, c)
			if fault == "ordinary-after-clock" {
				if v != partial || err != sentinel {
					t.Fatal(v, err)
				}
			} else if v != nil || !errors.Is(err, ErrUnshowable) {
				t.Fatal(v, err)
			}
			if st := c.Stats(); st.FailedViews != 0 || st.Views != 0 || st.ViewOwners != 0 {
				t.Fatal(st)
			}
			c.now = clk.Now
			c.beforeViewPublish = nil
			if _, err := c.View(viewKey("fault"), simpleView); err != nil {
				t.Fatal("internal fault quarantined healthy callback", err)
			}
			if fault == "first-clock" && builds.Load() != 0 {
				t.Fatal("builder ran after initial clock failed")
			}
		})
	}
	// A running builder remains counted after its only caller cancels. Its later
	// publication fault must retire the flight and slot without a waiter present.
	c, _ := testCache(t, &fakeParse{}, nil)
	c.beforeViewPublish = func() { panic("publication fixture") }
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan viewResult, 1)
	go func() {
		v, e := c.ViewContext(ctx, viewKey("detached"), func() (*View, error) { close(entered); <-release; return simpleView() })
		result <- viewResult{v, e}
	}()
	signalView(t, entered)
	cancel()
	if r := receiveView(t, result); !errors.Is(r.err, context.Canceled) {
		t.Fatal(r)
	}
	close(release)
	finishViews(t, c)
	if st := c.Stats(); st.FailedViews != 0 || st.ViewOwners != 0 {
		t.Fatal(st)
	}
}

func TestViewResourceOrdinaryErrorsAndRendererFailures(t *testing.T) {
	for _, kind := range []string{"ordinary", "panic", "nil", "explicit"} {
		t.Run(kind, func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			var n int
			partial := &View{Value: "partial"}
			sentinel := errors.New("fixture ordinary")
			build := func() (*View, error) {
				n++
				switch kind {
				case "ordinary":
					return partial, sentinel
				case "panic":
					panic("fixture")
				case "nil":
					return nil, nil
				default:
					return nil, ErrUnshowable
				}
			}
			for i := 0; i < 2; i++ {
				v, e := c.View(viewKey("same"), build)
				if kind == "ordinary" {
					if v != partial || e != sentinel {
						t.Fatal(v, e)
					}
				} else if !errors.Is(e, ErrUnshowable) {
					t.Fatal(e)
				}
			}
			want := 1
			if kind == "ordinary" {
				want = 2
			}
			if n != want {
				t.Fatalf("builds=%d want%d", n, want)
			}
		})
	}
}

func TestViewResourceFailureLRUAndWindowIsolation(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewFailures = 2 })
	var n int
	boom := func() (*View, error) { n++; panic("renderer fixture") }
	a, b, d := viewKey("a"), viewKey("b"), viewKey("c")
	for _, k := range []ViewKey{a, b, a, d} {
		c.View(k, boom)
	}
	if n != 3 {
		t.Fatal(n)
	}
	if st := c.Stats(); st.FailedViews != 2 || st.ViewOwners != 2 {
		t.Fatal(st)
	}
	c.View(b, boom)
	if n != 4 {
		t.Fatal("evicted failure did not permit retry", n)
	}
	for _, warm := range []bool{true, false} {
		t.Run(fmt.Sprint("warm=", warm), func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			healthy := viewKey("window")
			failed := healthy
			failed.Start = 3
			var good int
			build := func() (*View, error) { good++; return simpleView() }
			if warm {
				c.View(healthy, build)
			}
			c.View(failed, boom)
			if _, err := c.View(healthy, build); err != nil || good != 1 {
				t.Fatal("sibling window poisoned", err, good)
			}
			if _, err := c.View(failed, build); !errors.Is(err, ErrUnshowable) {
				t.Fatal(err)
			}
		})
	}
}

func TestViewResourceObsoleteStandaloneCompletion(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			old := viewKey("generation")
			newer := old
			newer.Snap.Mtime = 0 // authority is admission, not timestamp order
			entered, release := make(chan struct{}), make(chan struct{})
			result := make(chan viewResult, 1)
			go func() {
				v, e := c.View(old, func() (*View, error) {
					close(entered)
					<-release
					if fail {
						panic("old fixture")
					}
					return &View{ETag: "old", Bytes: 1, Settled: true}, nil
				})
				result <- viewResult{v, e}
			}()
			signalView(t, entered)
			if _, err := c.View(newer, simpleView); err != nil {
				t.Fatal(err)
			}
			close(release)
			r := receiveView(t, result)
			finishViews(t, c)
			if fail {
				if !errors.Is(r.err, ErrUnshowable) {
					t.Fatal(r)
				}
			} else if r.err != nil || r.v.ETag != "old" {
				t.Fatal(r)
			}
			if !c.NotModified(newer, "", "full") || c.NotModified(old, "", "old") {
				t.Fatal("obsolete completion replaced current view")
			}
			if st := c.Stats(); st.FailedViews != 0 || st.Views != 1 || st.ViewOwners != 1 {
				t.Fatal(st)
			}
		})
	}
}

func TestViewResourceProducerGuardsEqualSnapshotABA(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	k := ViewKey{Snap: viewFixture(t, c), Last: 50}
	reused := &View{ETag: "same", Bytes: 1, Settled: true}
	build := func() (*View, error) { return reused, nil }
	old, err := c.View(k, build)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.removeFullLocked(k.Snap.Path)
	c.mu.Unlock()
	_, snap, err := c.Session(context.Background(), k.Snap.Path)
	if err != nil || snap != k.Snap {
		t.Fatal(snap, err)
	}
	current, err := c.View(k, build)
	if err != nil {
		t.Fatal(err)
	}
	if old == reused || current == reused || old == current || reused.owner != nil || reused.key != (ViewKey{}) {
		t.Fatal("builder header mutated or reused")
	}
	if old.owner == current.owner {
		t.Fatal("parse ABA reused owner")
	}
	c.NoteDelta(k, old, "previous", "stale")
	c.NoteDelta(k, nil, "nil", "stale")
	if c.NotModified(k, "previous", "stale") || c.NotModified(k, "nil", "stale") {
		t.Fatal("stale producer amended new record")
	}
	c.NoteDelta(k, current, "current", "delta")
	if !c.NotModified(k, "current", "delta") {
		t.Fatal("current producer rejected")
	}
}

func TestViewResourceLateBuildAfterParseReadmission(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			k := ViewKey{Snap: viewFixture(t, c), Last: 50}
			entered, release := make(chan struct{}), make(chan struct{})
			result := make(chan viewResult, 1)
			go func() {
				v, e := c.View(k, func() (*View, error) {
					close(entered)
					<-release
					if fail {
						panic("old parsed fixture")
					}
					return &View{ETag: "old-parsed", Value: "old parsed result", Bytes: 1, Settled: true}, nil
				})
				result <- viewResult{v, e}
			}()
			signalView(t, entered)
			c.mu.Lock()
			c.removeFullLocked(k.Snap.Path)
			c.mu.Unlock()
			_, snap, err := c.Session(context.Background(), k.Snap.Path)
			if err != nil || snap != k.Snap {
				t.Fatal(snap, err)
			}
			// A different Start can establish the new parsed incarnation while old runs.
			other := k
			other.Start = 1
			current, err := c.View(other, simpleView)
			if err != nil {
				t.Fatal(err)
			}
			close(release)
			r := receiveView(t, result)
			finishViews(t, c)
			if fail && !errors.Is(r.err, ErrUnshowable) {
				t.Fatal(r)
			}
			if !fail && (r.err != nil || r.v == nil || r.v.ETag != "old-parsed" || r.v.Value != "old parsed result") {
				t.Fatal("obsolete parsed builder lost its caller outcome", r)
			}
			if c.NotModified(k, "", "full") || !c.NotModified(other, "", "full") {
				t.Fatal("old parsed completion polluted current state")
			}
			c.NoteDelta(other, current, "valid", "delta")
			if !c.NotModified(other, "valid", "delta") {
				t.Fatal("new owner removed by old cleanup")
			}
			if st := c.Stats(); st.FailedViews != 0 || st.ViewOwners != 1 {
				t.Fatal(st)
			}
		})
	}
}

func TestViewResourceOwnerBookkeepingAndKeyBacking(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewCount = 2; cfg.viewFailures = 2 })
	for i := 0; i < 200; i++ {
		k := viewKey(fmt.Sprint("path", i))
		if i%2 == 0 {
			c.View(k, simpleView)
		} else {
			c.View(k, func() (*View, error) { panic("fixture") })
		}
		if st := c.Stats(); st.ViewOwners > 4 || st.Views > 2 || st.FailedViews > 2 {
			t.Fatal(st)
		}
	}
	backing := make([]byte, 1<<20)
	for i := range backing {
		backing[i] = 'x'
	}
	source := string(backing)
	k := viewKey(source[:7])
	k.Lease = source[8:17]
	v, err := c.View(k, simpleView)
	if err != nil {
		t.Fatal(err)
	}
	if unsafe.StringData(v.key.Snap.Path) == unsafe.StringData(k.Snap.Path) || unsafe.StringData(v.key.Lease) == unsafe.StringData(k.Lease) {
		t.Fatal("retained view key kept caller backing")
	}
	// Full and failure eviction must eventually prune all standalone owners.
	c.mu.Lock()
	for c.viewLRU.Len() > 0 {
		c.removeViewLocked(c.viewLRU.Back())
	}
	for c.brokenLRU.Len() > 0 {
		c.removeBrokenViewLocked(c.brokenLRU.Back())
	}
	c.mu.Unlock()
	if st := c.Stats(); st.ViewOwners != 0 {
		t.Fatal(st)
	}
}

func TestViewResourceHeldParseIncarnationHooks(t *testing.T) {
	c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.parseBytes = 1 })
	big := &Session{Turns: []Turn{{Prompt: "fixture"}}}
	admit := func(path string) { c.mu.Lock(); defer c.mu.Unlock(); c.admitLocked(Snapshot{Path: path, Size: 1}, big) }
	key := func(path string) ViewKey { return ViewKey{Snap: Snapshot{Path: path, Size: 1}, Last: 50} }
	admit("full")
	admit("held-a")
	a, err := c.View(key("held-a"), simpleView)
	if err != nil {
		t.Fatal(err)
	}
	admit("held-b") // overwrites held-a and invalidates its views
	if c.NotModified(key("held-a"), "", "full") {
		t.Fatal("held overwrite retained old view")
	}
	b, err := c.View(key("held-b"), simpleView)
	if err != nil {
		t.Fatal(err)
	}
	clk.Add(c.cfg.floor)
	c.mu.Lock()
	c.heldLocked()
	c.mu.Unlock()
	if c.NotModified(key("held-b"), "", "full") {
		t.Fatal("held expiry retained old view")
	}
	admit("held-b")
	current, err := c.View(key("held-b"), simpleView)
	if err != nil {
		t.Fatal(err)
	}
	if current.owner == b.owner || current.owner == a.owner {
		t.Fatal("held incarnation token reused")
	}
	c.NoteDelta(key("held-b"), b, "old", "stale")
	if c.NotModified(key("held-b"), "old", "stale") {
		t.Fatal("expired held producer accepted")
	}
}

func TestViewResourceConflictingParsedSnapshotIsTransient(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	current := ViewKey{Snap: viewFixture(t, c), Last: 50}
	c.View(current, simpleView)
	other := current
	other.Snap.Size++
	var n int
	for i := 0; i < 2; i++ {
		v, err := c.View(other, func() (*View, error) { n++; return simpleView() })
		if err != nil || v.owner != nil {
			t.Fatal(v, err)
		}
	}
	if n != 2 || c.NotModified(other, "", "full") || !c.NotModified(current, "", "full") {
		t.Fatal("conflicting request changed parsed authority", n)
	}
	if st := c.Stats(); st.Views != 1 || st.ViewOwners != 1 {
		t.Fatal(st)
	}
}

func TestViewResourceSiblingEvictionPreservesParsedOwner(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewCount = 1 })
	k := ViewKey{Snap: viewFixture(t, c), Last: 50}
	first, err := c.View(k, simpleView)
	if err != nil {
		t.Fatal(err)
	}
	sibling := k
	sibling.Start = 3
	second, err := c.View(sibling, simpleView)
	if err != nil {
		t.Fatal(err)
	}
	if first.owner != second.owner || c.NotModified(k, "", "full") || !c.NotModified(sibling, "", "full") {
		t.Fatal("sibling eviction invalidated the parsed incarnation")
	}
}

func TestViewResourceExpiredHeldViewPrunesOnlyUnreferencedOwner(t *testing.T) {
	c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewBytes = 1; cfg.viewCount = 1 })
	k := viewKey("oversized")
	oversized := func() (*View, error) { return &View{Bytes: 100, ETag: "big", Settled: true}, nil }
	producer, err := c.View(k, oversized)
	if err != nil {
		t.Fatal(err)
	}
	c.NoteDelta(k, producer, "from", "delta")
	if !c.NotModified(k, "from", "delta") {
		t.Fatal("ETag-only producer rejected")
	}
	c.View(viewKey("different"), simpleView) // removes ETag record; held view still owns its token
	if st := c.Stats(); st.ViewOwners != 1 {
		t.Fatal(st)
	}
	clk.Add(c.cfg.floor)
	c.mu.Lock()
	c.heldViewLocked()
	c.mu.Unlock()
	if st := c.Stats(); st.ViewOwners != 0 {
		t.Fatal("unreferenced expired held owner retained", st)
	}
}
