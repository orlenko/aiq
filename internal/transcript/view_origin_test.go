package transcript

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/orlenko/aiq/internal/diagnostic"
)

func viewOriginA(t *testing.T) diagnostic.Origin {
	t.Helper()
	o := panicOriginCaught(viewOriginLeafA)
	panicOriginWant(t, o, "internal/transcript/view_origin_leaf_test.go", 8)
	return o
}
func viewOriginB(t *testing.T) diagnostic.Origin {
	t.Helper()
	o := panicOriginCaught(func() { viewOriginClockB() })
	panicOriginWant(t, o, "internal/transcript/view_origin_leaf_test.go", 10)
	return o
}
func viewOriginUnknown(t *testing.T) diagnostic.Origin {
	t.Helper()
	o := panicOriginCaught(viewOriginExternal)
	if _, _, known := o.Location(); !o.Present() || known {
		t.Fatal("external leaf was attributed to a View guard")
	}
	return o
}

func TestViewOriginExistingGuardsAndFirstBeforeLaterFault(t *testing.T) {
	a, b, u := viewOriginA(t), viewOriginB(t), viewOriginUnknown(t)
	for _, tc := range []struct{ stage, kind string }{
		{"first", "success"}, {"after", "ordinary-known"}, {"after", "ordinary-unknown"}, {"after", "ordinary-bare"},
		{"after", "renderer-known"}, {"after", "renderer-unknown"}, {"after", "caught"}, {"after", "repanic"}, {"after", "legacy-repanic"},
		{"hook", "renderer-known"}, {"publication-clock", "success"},
	} {
		t.Run(tc.stage+"/"+tc.kind, func(t *testing.T) {
			c, clk := testCache(t, &fakeParse{}, nil)
			var clocks, builds atomic.Int64
			target := int64(2)
			if tc.stage == "first" {
				target = 1
			}
			if tc.stage == "publication-clock" {
				target = 3
			}
			if tc.stage == "hook" {
				target = 0
				c.beforeViewPublish = func() { viewOriginClockB() }
			}
			c.now = func() time.Time {
				if clocks.Add(1) == target {
					return viewOriginClockB()
				}
				return clk.Now()
			}
			want := b
			origin := a
			if strings.Contains(tc.kind, "unknown") {
				origin = u
			}
			ordinary := strings.HasPrefix(tc.kind, "ordinary")
			var returned error
			if ordinary {
				returned = diagnostic.Failure(diagnostic.Inspection, origin)
				if tc.kind == "ordinary-bare" {
					returned = errors.New("ordinary fixture error")
				}
			}
			if strings.HasPrefix(tc.kind, "renderer") {
				returned = diagnostic.Failure(diagnostic.Unshowable, origin)
			}
			if tc.kind != "success" && tc.kind != "ordinary-bare" {
				want = origin
			}
			partial := &View{Value: "partial fixture"}
			ctx, h := diagnostic.WithHolder(context.Background())
			v, err := c.ViewContext(ctx, viewKey("origin-fault"), func() (*View, error) {
				builds.Add(1)
				switch tc.kind {
				case "caught":
					viewOriginLeafA()
				case "repanic":
					panic(diagnostic.Failure(diagnostic.Unshowable, a))
				case "legacy-repanic":
					panic(&unreadableError{path: "invented.jsonl", origin: a})
				}
				if ordinary {
					return partial, returned
				}
				if returned != nil {
					return nil, returned
				}
				return simpleView()
			})
			finishViews(t, c)
			if ordinary {
				if v != partial || err != returned {
					t.Fatal("ordinary partial pointer/error identity changed")
				}
			} else if v != nil || !errors.Is(err, ErrUnshowable) || err.Error() != "transcript: could not be shown" || PanicOrigin(err) != want {
				t.Fatal("fixed guarded failure lost original origin")
			}
			if h.Origin() != want {
				t.Fatal("later fault replaced first known/unknown observation")
			}
			if st := c.Stats(); st.BuildingViews != 0 || st.FailedViews != 0 || st.Views != 0 || st.ViewOwners != 0 || len(c.viewSem) != 0 {
				t.Fatal("internal fault changed retention or leaked worker/slot")
			}
			if tc.stage == "first" && builds.Load() != 0 {
				t.Fatal("builder ran after first clock fault")
			}
			c.now = clk.Now
			c.beforeViewPublish = nil
			if _, err := c.View(viewKey("origin-fault"), simpleView); err != nil {
				t.Fatal("internal fault prevented healthy retry")
			}
		})
	}
	// The fourth existing guard is viewRole, before a worker is admitted.
	c, clk := testCache(t, &fakeParse{}, nil)
	c.held = inspectionParse("held-origin", "claude", inspectionID)
	c.now = viewOriginClockB
	ctx, h := diagnostic.WithHolder(context.Background())
	v, err := c.ViewContext(ctx, viewKey("role-clock"), simpleView)
	if v != nil || !errors.Is(err, ErrUnshowable) || h.Origin() != b || c.Stats().ViewBuilds != 0 || len(c.building) != 0 || len(c.viewSem) != 0 {
		t.Fatal("role guard lost fixed fault/cleanup")
	}
	c.now = clk.Now
	c.held = nil
	if _, err := c.View(viewKey("role-clock"), simpleView); err != nil {
		t.Fatal("role fault poisoned retry")
	}
}

func TestViewOriginBrokenReplayBoundsOwnershipAndBareFailures(t *testing.T) {
	a, b, u := viewOriginA(t), viewOriginB(t), viewOriginUnknown(t)
	for _, origin := range []diagnostic.Origin{a, u, {}} {
		c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewFailures = 2 })
		backing := strings.Repeat("x", 65536) + "borrowed-view" + strings.Repeat("y", 65536)
		borrowed := backing[65536 : 65536+len("borrowed-view")]
		k := viewKey(borrowed)
		var builds atomic.Int64
		build := func() (*View, error) { builds.Add(1); return nil, diagnostic.Failure(diagnostic.Unshowable, origin) }
		for i := 0; i < 3; i++ {
			ctx, h := diagnostic.WithHolder(context.Background())
			poll := k
			poll.Lease = fmt.Sprint(i)
			v, err := c.ViewContext(ctx, poll, build)
			if v != nil || !errors.Is(err, ErrUnshowable) || h.Origin() != origin || PanicOrigin(err) != origin {
				t.Fatal("broken shape replay lost first origin")
			}
			if !origin.Present() && err != ErrUnshowable {
				t.Fatal("bare nonpanic failure changed identity")
			}
		}
		if builds.Load() != 1 {
			t.Fatal("broken replay rebuilt a context variant")
		}
		c.mu.Lock()
		record := c.broken[shapeOf(k)].Value.(*brokenView)
		owner := record.owner
		for key, el := range c.broken {
			r := el.Value.(*brokenView)
			if unsafe.StringData(key.snap.Path) == unsafe.StringData(borrowed) || unsafe.StringData(key.snap.Path) != unsafe.StringData(r.shape.snap.Path) || unsafe.StringData(r.shape.snap.Path) != unsafe.StringData(owner.snap.Path) {
				c.mu.Unlock()
				t.Fatal("broken key retained borrowed/different path backing")
			}
		}
		c.breakViewLocked(shapeOf(k), owner, b)
		selected := record.origin
		c.mu.Unlock()
		want := origin
		if !origin.Present() {
			want = b
		}
		if selected != want {
			t.Fatal("same owner overwrote first known/unknown or missed first present")
		}
		window := k
		window.Start++
		ctx, h := diagnostic.WithHolder(context.Background())
		if _, err := c.ViewContext(ctx, window, simpleView); err != nil || h.Origin().Present() {
			t.Fatal("failure crossed window shape")
		}
		for i := 1; i <= 2; i++ {
			other := k
			other.Last += i
			if _, err := c.View(other, func() (*View, error) { return nil, diagnostic.Failure(diagnostic.Unshowable, b) }); !errors.Is(err, ErrUnshowable) {
				t.Fatal(err)
			}
		}
		if c.Stats().FailedViews != 2 {
			t.Fatal("new origin escaped existing failure count cap")
		}
		ctx, h = diagnostic.WithHolder(context.Background())
		if _, err := c.ViewContext(ctx, k, simpleView); err != nil || h.Origin().Present() {
			t.Fatal("evicted failure did not recover")
		}
		finishViews(t, c)
		if c.cfg.viewBytes != defaultCacheConfig.viewBytes || c.Stats().ViewBytes != 2 || c.Stats().Views != 2 {
			t.Fatal("origin added a healthy View charge or changed budget")
		}
	}
	// nil success is a nonpanic renderer refusal, not an invented origin.
	c, _ := testCache(t, &fakeParse{}, nil)
	ctx, h := diagnostic.WithHolder(context.Background())
	if v, err := c.ViewContext(ctx, viewKey("nil-result"), func() (*View, error) { return nil, nil }); v != nil || err != ErrUnshowable || h.Origin().Present() {
		t.Fatal("nil success acquired diagnostic origin")
	}
	type oldBroken struct {
		shape viewShape
		owner *viewOwner
	}
	type oldFlight struct {
		done  chan struct{}
		view  *View
		err   error
		owner *viewOwner
	}
	if unsafe.Sizeof(brokenView{})-unsafe.Sizeof(oldBroken{}) != unsafe.Sizeof(diagnostic.Origin{}) || unsafe.Sizeof(viewFlight{})-unsafe.Sizeof(oldFlight{}) != unsafe.Sizeof(diagnostic.Origin{}) {
		t.Fatal("unexpected inline origin layout")
	}
	t.Logf("count-bounded layout bytes: Origin=%d brokenView=%d viewFlight=%d; healthy View=%d unchanged", unsafe.Sizeof(diagnostic.Origin{}), unsafe.Sizeof(brokenView{}), unsafe.Sizeof(viewFlight{}), unsafe.Sizeof(View{}))
}

type viewOriginRoleContext struct {
	context.Context
	c        *Cache
	n        atomic.Int64
	cancelAt int64
	locked   atomic.Bool
}

func (c *viewOriginRoleContext) Err() error {
	if c.n.Add(1) >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
func (c *viewOriginRoleContext) Value(k any) any {
	if !c.c.mu.TryLock() {
		c.locked.Store(true)
	} else {
		c.c.mu.Unlock()
	}
	return c.Context.Value(k)
}

func TestViewOriginBothRoleReturnsBeforeCancellation(t *testing.T) {
	a := viewOriginA(t)
	for _, second := range []bool{false, true} {
		for _, joined := range []bool{false, true} {
			t.Run(fmt.Sprintf("second=%v/joined=%v", second, joined), func(t *testing.T) {
				c, _ := testCache(t, &fakeParse{}, nil)
				k := viewKey("role-recheck")
				var bk viewBuildKey
				install := func() {
					c.mu.Lock()
					defer c.mu.Unlock()
					owner := c.viewOwnerLocked(k.Snap)
					if joined {
						// A returned-role readiness fixture: immutable terminal fields
						// model the flight that completed before the outer ctx check.
						f := &viewFlight{done: make(chan struct{}), owner: owner, err: diagnostic.Failure(diagnostic.Unshowable, a), origin: a}
						close(f.done)
						bk = viewBuildKey{key: k, owner: owner}
						c.building[bk] = f
					} else {
						c.breakViewLocked(shapeOf(k), owner, a)
					}
				}
				if second {
					c.beforeViewAdmit = func(ViewKey) { install() }
				} else {
					install()
				}
				t.Cleanup(func() { c.mu.Lock(); delete(c.building, bk); c.pruneViewOwnerLocked(k.Snap.Path); c.mu.Unlock() })
				base, h := diagnostic.WithHolder(context.Background())
				at := int64(3)
				if second {
					at = 5
				}
				ctx := &viewOriginRoleContext{Context: base, c: c, cancelAt: at}
				builds := 0
				v, err := c.ViewContext(ctx, k, func() (*View, error) { builds++; return simpleView() })
				if v != nil || err != context.Canceled || h.Origin() != a || ctx.locked.Load() || builds != 0 || len(c.viewSem) != 0 {
					t.Fatal("selected role lost available origin before cancellation/unlock/slot release")
				}
			})
		}
	}
}

type viewOriginWaitContext struct {
	context.Context
	n                atomic.Int64
	entered, release chan struct{}
	once             sync.Once
}

func (c *viewOriginWaitContext) Done() <-chan struct{} {
	if c.n.Add(1) == 2 {
		c.once.Do(func() { close(c.entered); <-c.release })
	}
	return c.Context.Done()
}

func TestViewOriginActualTerminalCancelAndUnfinished(t *testing.T) {
	a := viewOriginA(t)
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			base, h := diagnostic.WithHolder(context.Background())
			parent, cancel := context.WithCancel(base)
			ctx := &viewOriginWaitContext{Context: parent, entered: make(chan struct{}), release: make(chan struct{})}
			buildGate, releaseBuild := viewGate()
			var open sync.Once
			unblock := func() { open.Do(func() { close(ctx.release) }) }
			var wg sync.WaitGroup
			wg.Add(1)
			done := make(chan viewResult, 1)
			go func() {
				defer wg.Done()
				v, e := c.ViewContext(ctx, viewKey("terminal-origin"), func() (*View, error) { <-buildGate; return nil, diagnostic.Failure(diagnostic.Unshowable, a) })
				done <- viewResult{v, e}
			}()
			t.Cleanup(func() { cancel(); unblock(); releaseBuild(); wg.Wait(); finishViews(t, c) })
			signalView(t, ctx.entered)
			if ready {
				releaseBuild()
				finishViews(t, c)
			}
			cancel()
			unblock()
			wg.Wait()
			r := receiveView(t, done)
			if r.v != nil || r.err != context.Canceled {
				t.Fatal("terminal/cancel changed public outcome")
			}
			if ready {
				if h.Origin() != a {
					t.Fatal("ready failure origin erased by cancellation")
				}
			} else {
				if h.Origin().Present() {
					t.Fatal("pending metadata was read")
				}
				releaseBuild()
				finishViews(t, c)
				if h.Origin().Present() {
					t.Fatal("detached completion retained caller holder")
				}
			}
		})
	}
}

func TestViewOriginCurrentOwnerAndEqualSnapshotReplacement(t *testing.T) {
	a, b := viewOriginA(t), viewOriginB(t)
	for _, oldFirst := range []bool{false, true} {
		for _, healthy := range []bool{false, true} {
			t.Run(fmt.Sprintf("oldFirst=%v/healthy=%v", oldFirst, healthy), func(t *testing.T) {
				c, _ := testCache(t, &fakeParse{}, nil)
				k := viewKey("incarnation-origin")
				k.Snap = viewFixture(t, c)
				oldGate, releaseOld := viewGate()
				newGate, releaseNew := viewGate()
				oldEntered, newEntered := make(chan struct{}), make(chan struct{})
				oldCtx, oldHolder := diagnostic.WithHolder(context.Background())
				newCtx, newHolder := diagnostic.WithHolder(context.Background())
				var wg sync.WaitGroup
				start := func(ctx context.Context, build func() (*View, error)) <-chan viewResult {
					ch := make(chan viewResult, 1)
					wg.Add(1)
					go func() { defer wg.Done(); v, e := c.ViewContext(ctx, k, build); ch <- viewResult{v, e} }()
					return ch
				}
				t.Cleanup(func() { releaseOld(); releaseNew(); wg.Wait(); finishViews(t, c) })
				old := start(oldCtx, func() (*View, error) {
					close(oldEntered)
					<-oldGate
					return nil, diagnostic.Failure(diagnostic.Unshowable, a)
				})
				signalView(t, oldEntered)
				c.mu.Lock()
				oldOwner := c.viewOwners[k.Snap.Path]
				c.mu.Unlock()
				replaceViewParse(t, c, k) // real equal-Snapshot S1 retirement/reparse as S2
				current := start(newCtx, func() (*View, error) {
					close(newEntered)
					<-newGate
					if healthy {
						return simpleView()
					}
					return nil, diagnostic.Failure(diagnostic.Unshowable, b)
				})
				signalView(t, newEntered)
				if oldFirst {
					releaseOld()
					r := receiveView(t, old)
					if !errors.Is(r.err, ErrUnshowable) || oldHolder.Origin() != a {
						t.Fatal("old waiter lost captured origin")
					}
				}
				releaseNew()
				fresh := receiveView(t, current)
				if !oldFirst {
					releaseOld()
					r := receiveView(t, old)
					if !errors.Is(r.err, ErrUnshowable) || oldHolder.Origin() != a {
						t.Fatal("late old waiter lost origin")
					}
				}
				wg.Wait()
				finishViews(t, c)
				ctx, h := diagnostic.WithHolder(context.Background())
				var rebuilt atomic.Int64
				v, err := c.ViewContext(ctx, k, func() (*View, error) { rebuilt.Add(1); return simpleView() })
				if rebuilt.Load() != 0 {
					t.Fatal("current success/failure was not retained")
				}
				if healthy {
					if fresh.err != nil || v == nil || err != nil || newHolder.Origin().Present() || h.Origin().Present() || c.Stats().FailedViews != 0 {
						t.Fatal("old failure contaminated healthy new owner")
					}
				} else {
					if !errors.Is(fresh.err, ErrUnshowable) || !errors.Is(err, ErrUnshowable) || newHolder.Origin() != b || h.Origin() != b {
						t.Fatal("old origin replaced current broken origin")
					}
					c.mu.Lock()
					q := c.broken[shapeOf(k)].Value.(*brokenView)
					wrong := q.owner == oldOwner || q.origin != b
					c.mu.Unlock()
					if wrong {
						t.Fatal("broken origin crossed equal-Snapshot owner fence")
					}
				}
			})
		}
	}
	// A changed Snapshot retires the old shape even without a parse object.
	c, _ := testCache(t, &fakeParse{}, nil)
	k := viewKey("changed-origin")
	_, _ = c.View(k, func() (*View, error) { return nil, diagnostic.Failure(diagnostic.Unshowable, a) })
	k.Snap.Mtime++
	ctx, h := diagnostic.WithHolder(context.Background())
	if _, err := c.ViewContext(ctx, k, simpleView); err != nil || h.Origin().Present() || c.Stats().FailedViews != 0 {
		t.Fatal("changed Snapshot retained old origin")
	}
}
