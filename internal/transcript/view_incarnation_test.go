package transcript

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func viewGate() (chan struct{}, func()) {
	ch := make(chan struct{})
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}
func asyncView(c *Cache, ctx context.Context, k ViewKey, build func() (*View, error)) <-chan viewResult {
	result := make(chan viewResult, 1)
	go func() { v, err := c.ViewContext(ctx, k, build); result <- viewResult{v, err} }()
	return result
}
func replaceViewParse(t *testing.T, c *Cache, k ViewKey) *parsed {
	t.Helper()
	c.mu.Lock()
	old := c.viewParseLocked(k.Snap.Path)
	c.removeFullLocked(k.Snap.Path)
	c.mu.Unlock()
	_, snap, err := c.Session(context.Background(), k.Snap.Path)
	if err != nil || snap != k.Snap {
		t.Fatal("equal-snapshot replacement failed", err)
	}
	c.mu.Lock()
	current := c.viewParseLocked(k.Snap.Path)
	c.mu.Unlock()
	if current == nil || current == old {
		t.Fatal("parse incarnation did not change")
	}
	return current
}

func TestViewIncarnationSeparatesExactKeys(t *testing.T) {
	for _, authority := range []string{"full", "held", "standalone"} {
		for _, outcome := range []string{"success", "ordinary", "panic"} {
			for _, oldFirst := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/old-first=%t", authority, outcome, oldFirst), func(t *testing.T) {
					c, clk := testCache(t, &fakeParse{}, nil)
					k := viewKey("incarnation")
					var replace func()
					switch authority {
					case "full":
						k.Snap = viewFixture(t, c)
						replace = func() { replaceViewParse(t, c, k) }
					case "held":
						c.cfg.parseBytes = 1
						big := &Session{Turns: []Turn{{Prompt: "invented"}}}
						c.mu.Lock()
						c.admitLocked(Snapshot{Path: "anchor"}, big)
						c.admitLocked(k.Snap, big)
						c.mu.Unlock()
						replace = func() { clk.Add(c.cfg.floor); c.mu.Lock(); c.heldLocked(); c.admitLocked(k.Snap, big); c.mu.Unlock() }
					default:
						replace = func() {
							other := k
							other.Snap.Mtime++
							if _, err := c.View(other, simpleView); err != nil {
								t.Fatal(err)
							}
						}
					}
					oldGate, releaseOld := viewGate()
					newGate, releaseNew := viewGate()
					t.Cleanup(func() { releaseOld(); releaseNew(); finishViews(t, c) })
					oldEntered, newEntered := make(chan struct{}), make(chan struct{})
					ordinary := errors.New("old ordinary renderer failure")
					partial := &View{Value: "old partial"}
					old := asyncView(c, context.Background(), k, func() (*View, error) {
						close(oldEntered)
						<-oldGate
						switch outcome {
						case "ordinary":
							return partial, ordinary
						case "panic":
							panic("old renderer fixture")
						default:
							return &View{Value: "old result", ETag: "old", Bytes: 1, Settled: true}, nil
						}
					})
					signalView(t, oldEntered)
					oldAttached := make(chan struct{}, 4)
					oldFollower := asyncView(c, observedViewContext{context.Background(), oldAttached}, k, func() (*View, error) { t.Error("old follower built again"); return simpleView() })
					signalView(t, oldAttached)
					replace()
					var newBuilds atomic.Int64
					current := asyncView(c, context.Background(), k, func() (*View, error) {
						newBuilds.Add(1)
						close(newEntered)
						<-newGate
						return &View{Value: "new result", ETag: "new", Bytes: 1, Settled: true}, nil
					})
					signalView(t, newEntered) // must enter before the obsolete renderer is released
					if st := c.Stats(); st.BuildingViews != 2 {
						t.Fatal("incarnations did not coexist", st)
					}
					var before viewResult
					if oldFirst {
						releaseOld()
						before = receiveView(t, old)
						if st := c.Stats(); st.BuildingViews != 1 {
							t.Fatal("old cleanup removed the current worker", st)
						}
					}
					joined := make(chan struct{}, 4)
					follower := asyncView(c, observedViewContext{context.Background(), joined}, k, func() (*View, error) { newBuilds.Add(1); return &View{ETag: "wrong follower"}, nil })
					signalView(t, joined)
					releaseNew()
					fresh := receiveView(t, current)
					follow := receiveView(t, follower)
					if fresh.err != nil || fresh.v == nil || fresh.v.ETag != "new" || fresh.v.Value != "new result" || follow.err != nil || follow.v != fresh.v || newBuilds.Load() != 1 {
						t.Fatal("new callers lost their own incarnation", fresh, follow)
					}
					if !oldFirst {
						releaseOld()
						before = receiveView(t, old)
					}
					oldFollow := receiveView(t, oldFollower)
					switch outcome {
					case "ordinary":
						if before.v != partial || before.err != ordinary {
							t.Fatal("old partial result changed", before)
						}
					case "panic":
						if !errors.Is(before.err, ErrUnshowable) {
							t.Fatal(before)
						}
					default:
						if before.err != nil || before.v == nil || before.v.ETag != "old" {
							t.Fatal("old waiter lost its outcome", before)
						}
					}
					if oldFollow.v != before.v || oldFollow.err != before.err {
						t.Fatal("old follower changed incarnation", oldFollow, before)
					}
					finishViews(t, c)
					if !c.NotModified(k, "", "new") || c.NotModified(k, "", "old") {
						t.Fatal("obsolete retained outcome replaced current result")
					}
					c.NoteDelta(k, before.v, "old from", "old delta")
					c.NoteDelta(k, fresh.v, "new from", "new delta")
					if c.NotModified(k, "old from", "old delta") || !c.NotModified(k, "new from", "new delta") {
						t.Fatal("producer provenance crossed incarnations")
					}
					if st := c.Stats(); st.Views != 1 || st.FailedViews != 0 || st.ViewOwners != 1 {
						t.Fatal(st)
					}
				})
			}
		}
	}
}

func TestViewIncarnationTransientAuthorityPointer(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	parsedKey := ViewKey{Snap: viewFixture(t, c), Last: 50}
	k := parsedKey
	k.Snap.Size++ // conflicts with BOTH authoritative parsed objects
	oldGate, releaseOld := viewGate()
	newGate, releaseNew := viewGate()
	t.Cleanup(func() { releaseOld(); releaseNew(); finishViews(t, c) })
	entered, newEntered := make(chan struct{}), make(chan struct{})
	oldError, newError := errors.New("old transient error"), errors.New("new transient error")
	oldPartial, newPartial := &View{ETag: "old partial"}, &View{ETag: "new partial"}
	old := asyncView(c, context.Background(), k, func() (*View, error) { close(entered); <-oldGate; return oldPartial, oldError })
	signalView(t, entered)
	replaceViewParse(t, c, parsedKey)
	good, err := c.View(parsedKey, simpleView)
	if err != nil {
		t.Fatal(err)
	}
	var builds atomic.Int64
	current := asyncView(c, context.Background(), k, func() (*View, error) { builds.Add(1); close(newEntered); <-newGate; return newPartial, newError })
	signalView(t, newEntered)
	attached := make(chan struct{}, 4)
	follower := asyncView(c, observedViewContext{context.Background(), attached}, k, func() (*View, error) { builds.Add(1); return simpleView() })
	signalView(t, attached)
	if st := c.Stats(); st.BuildingViews != 2 {
		t.Fatal(st)
	}
	releaseOld()
	before := receiveView(t, old)
	if before.v != oldPartial || before.err != oldError {
		t.Fatal(before)
	}
	releaseNew()
	after := receiveView(t, current)
	follow := receiveView(t, follower)
	if after.v != newPartial || after.err != newError || follow.v != newPartial || follow.err != newError || builds.Load() != 1 {
		t.Fatal(after, follow)
	}
	finishViews(t, c)
	if !c.NotModified(parsedKey, "", "full") || c.NotModified(k, "", "old partial") || c.NotModified(k, "", "new partial") {
		t.Fatal("transient work changed retained authority")
	}
	c.NoteDelta(parsedKey, good, "valid", "delta")
	if !c.NotModified(parsedKey, "valid", "delta") {
		t.Fatal("transient cleanup pruned current parsed owner")
	}
	if st := c.Stats(); st.Views != 1 || st.FailedViews != 0 || st.ViewOwners != 1 {
		t.Fatal(st)
	}
}

func TestViewIncarnationRecapturesAfterBorrow(t *testing.T) {
	for _, cancelAtPause := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancelAtPause), func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			k := ViewKey{Snap: viewFixture(t, c), Last: 50}
			oldGate, releaseOld := viewGate()
			pause, unpause := viewGate()
			t.Cleanup(func() { unpause(); releaseOld(); finishViews(t, c) })
			entered := make(chan struct{})
			old := asyncView(c, context.Background(), k, func() (*View, error) { close(entered); <-oldGate; return simpleView() })
			signalView(t, entered)
			replaceViewParse(t, c, k)
			borrowed := make(chan struct{})
			c.beforeViewAdmit = func(ViewKey) { close(borrowed); <-pause }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			current := asyncView(c, ctx, k, simpleView)
			signalView(t, borrowed)
			if st := c.Stats(); st.ViewOwners != 0 || st.BuildingViews != 1 {
				t.Fatal("queued lookup created authority", st)
			}
			newest := replaceViewParse(t, c, k)
			if cancelAtPause {
				cancel()
			}
			unpause()
			after := receiveView(t, current)
			if cancelAtPause {
				if !errors.Is(after.err, context.Canceled) {
					t.Fatal(after)
				}
			} else if after.err != nil || after.v == nil || after.v.owner == nil || after.v.owner.parsed != newest {
				t.Fatal("installation captured stale authority", after)
			}
			releaseOld()
			receiveView(t, old)
			finishViews(t, c)
			if cancelAtPause && c.Stats().ViewOwners != 0 {
				t.Fatal("canceled borrowed caller created an owner")
			}
		})
	}
}

func TestViewIncarnationBorrowedCallerBecomesFollower(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	k := ViewKey{Snap: viewFixture(t, c), Last: 50}
	oldGate, releaseOld := viewGate()
	pause, unpause := viewGate()
	newGate, releaseNew := viewGate()
	t.Cleanup(func() { unpause(); releaseOld(); releaseNew(); finishViews(t, c) })
	entered, newEntered := make(chan struct{}), make(chan struct{})
	old := asyncView(c, context.Background(), k, func() (*View, error) { close(entered); <-oldGate; return simpleView() })
	signalView(t, entered)
	replaceViewParse(t, c, k)
	borrowed := make(chan struct{})
	var admissions atomic.Int64
	c.beforeViewAdmit = func(ViewKey) {
		if admissions.Add(1) == 1 {
			close(borrowed)
			<-pause
		}
	}
	observed := make(chan struct{}, 4)
	var wrong atomic.Int64
	paused := asyncView(c, observedViewContext{context.Background(), observed}, k, func() (*View, error) { wrong.Add(1); return simpleView() })
	signalView(t, borrowed)
	signalView(t, observed) // initial admission select
	releaseOld()
	receiveView(t, old)
	sentinel := errors.New("shared ordinary error")
	current := asyncView(c, context.Background(), k, func() (*View, error) { close(newEntered); <-newGate; return nil, sentinel })
	signalView(t, newEntered)
	unpause()
	signalView(t, observed) // now joined, not installed again
	if st := c.Stats(); st.BuildingViews != 1 || st.ViewOwners != 1 {
		t.Fatal(st)
	}
	c.mu.Lock()
	slots := len(c.viewSem)
	c.mu.Unlock()
	if slots != 1 {
		t.Fatal("borrowed follower did not return its slot", slots)
	}
	releaseNew()
	if r := receiveView(t, current); r.err != sentinel {
		t.Fatal(r)
	}
	if r := receiveView(t, paused); r.err != sentinel || wrong.Load() != 0 {
		t.Fatal("borrowed caller failed to join", r)
	}
	finishViews(t, c)
}

func TestViewIncarnationCanceledWorkersKeepTheirSlots(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	k := ViewKey{Snap: viewFixture(t, c), Last: 50}
	oldGate, releaseOld := viewGate()
	newGate, releaseNew := viewGate()
	t.Cleanup(func() { releaseOld(); releaseNew(); finishViews(t, c) })
	oldEntered, newEntered := make(chan struct{}), make(chan struct{})
	oldCtx, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	newCtx, cancelNew := context.WithCancel(context.Background())
	defer cancelNew()
	old := asyncView(c, oldCtx, k, func() (*View, error) { close(oldEntered); <-oldGate; return simpleView() })
	signalView(t, oldEntered)
	replaceViewParse(t, c, k)
	current := asyncView(c, newCtx, k, func() (*View, error) { close(newEntered); <-newGate; return simpleView() })
	signalView(t, newEntered)
	cancelOld()
	cancelNew()
	for _, r := range []<-chan viewResult{old, current} {
		if got := receiveView(t, r); !errors.Is(got.err, context.Canceled) {
			t.Fatal(got)
		}
	}
	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	defer cancelQueued()
	calls := make(chan struct{}, 4)
	queued := asyncView(c, observedViewContext{queuedCtx, calls}, viewKey("third"), simpleView)
	signalView(t, calls)
	cancelQueued()
	if r := receiveView(t, queued); !errors.Is(r.err, context.Canceled) {
		t.Fatal(r)
	}
	if st := c.Stats(); st.BuildingViews != 2 || st.ViewOwners != 1 {
		t.Fatal("abandoned workers lost admission bounds", st)
	}
	releaseOld()
	releaseNew()
	finishViews(t, c)
	c.mu.Lock()
	c.removeFullLocked(k.Snap.Path)
	c.mu.Unlock()
	if st := c.Stats(); st.ViewOwners != 0 {
		t.Fatal("unreferenced authority remained", st)
	}
}
