package transcript

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
)

// This exercises the library callback bridge. The real detail mixed row/full
// witness and outer HTTP delivery require their separate integration fixture.
func TestViewDiagnosticsBridgeFirstOriginNormalAndUnwind(t *testing.T) {
	a, unknown, b := viewOriginA(t), viewOriginUnknown(t), viewOriginB(t)
	for _, origin := range []diagnostic.Origin{a, unknown} {
		for _, mode := range []string{"success", "ordinary", "ordinary-clock", "panic", "panic-clock", "publication", "returned"} {
			t.Run(fmt.Sprintf("known=%v/%s", origin == a, mode), func(t *testing.T) {
				c, clock := testCache(t, &fakeParse{}, nil)
				ctx, caller := diagnostic.WithHolder(context.Background())
				var calls atomic.Int64
				if mode == "ordinary-clock" || mode == "panic-clock" {
					c.now = func() time.Time {
						if calls.Add(1) == 2 {
							return viewOriginClockB()
						}
						return clock.Now()
					}
				}
				if mode == "publication" {
					c.beforeViewPublish = func() { viewOriginClockB() }
				}
				partial := &View{Value: &struct{ label string }{"invented"}, ETag: "literal", Bytes: 7, Settled: true}
				ordinary := errors.New("invented selection changed")
				returned := diagnostic.Failure(diagnostic.Inspection, b)
				var worker *diagnostic.Holder
				key := viewKey("diagnostic-operation")
				v, err := c.ViewContextWithDiagnostics(ctx, key, func(h *diagnostic.Holder) (*View, error) {
					worker = h
					if h == nil || h == caller || h.Origin().Present() {
						return nil, errors.New("invalid operation holder")
					}
					localCtx, local := diagnostic.WithHolder(context.Background())
					// No recovery here: this defer bridges normal and panic unwind
					// before the cache's existing build guard chooses its carrier.
					defer func() { h.Note(local.Origin()) }()
					diagnostic.Note(localCtx, origin)
					switch mode {
					case "ordinary", "ordinary-clock":
						return partial, ordinary
					case "returned":
						return partial, returned
					case "panic", "panic-clock":
						viewOriginClockB()
					}
					return partial, nil
				})
				finishViews(t, c)
				if worker == nil || worker.Origin() != origin || caller.Origin() != origin {
					t.Fatal("normal/unwind bridge lost first known/unknown A")
				}
				st := c.Stats()
				switch mode {
				case "ordinary", "ordinary-clock", "returned":
					want := ordinary
					if mode == "returned" {
						want = returned
					}
					if v != partial || err != want || st.Views != 0 || st.FailedViews != 0 {
						t.Fatal("ordinary pair/classification/retention changed")
					}
				case "success":
					if err != nil || v == partial || v == nil || v.Value != partial.Value || v.ETag != "literal" || v.Bytes != 7 || v.owner == nil || v.key != key || st.Views != 1 || st.ViewBytes != 7 || st.FailedViews != 0 {
						t.Fatal("healthy view header/ownership/accounting changed")
					}
				default:
					if v != nil || !errors.Is(err, ErrUnshowable) || err.Error() != "transcript: could not be shown" || PanicOrigin(err) != origin {
						t.Fatal("guard selected B carrier after bridged A")
					}
					wantBroken := 0
					if mode == "panic" {
						wantBroken = 1
					}
					if st.Views != 0 || st.FailedViews != wantBroken {
						t.Fatal("internal timing/publication fault changed renderer blame")
					}
				}
				if st.BuildingViews != 0 || len(c.viewSem) != 0 || len(c.quarantine) != 0 {
					t.Fatal("bridge leaked worker/slot or quarantined a transcript")
				}
				// Completed snapshots hold Origin VALUES. Further callback use is
				// forbidden; no pending Holder pointer is stored in the flight.
				if mode == "success" || mode == "panic" {
					pollCtx, poll := diagnostic.WithHolder(context.Background())
					var rebuilt atomic.Bool
					got, e := c.ViewContextWithDiagnostics(pollCtx, key, func(*diagnostic.Holder) (*View, error) {
						rebuilt.Store(true)
						return simpleView()
					})
					if rebuilt.Load() {
						t.Fatal("hit/broken replay invoked another builder")
					}
					if mode == "success" {
						if got != v || e != nil || poll.Origin().Present() {
							t.Fatal("healthy hit retained operation diagnostics")
						}
					} else if got != nil || !errors.Is(e, ErrUnshowable) || PanicOrigin(e) != origin || poll.Origin() != origin {
						t.Fatal("broken replay lost bridged origin")
					}
				}
			})
		}
	}
}

func TestViewDiagnosticsBridgeAbsentAndRepanicControls(t *testing.T) {
	a, b, unknown := viewOriginA(t), viewOriginB(t), viewOriginUnknown(t)
	for _, tc := range []struct {
		name  string
		build func(*diagnostic.Holder) (*View, error)
		want  diagnostic.Origin
		bare  bool
	}{
		{"nil-success", func(*diagnostic.Holder) (*View, error) { return nil, nil }, diagnostic.Origin{}, true},
		{"bare-refusal", func(*diagnostic.Holder) (*View, error) { return nil, ErrUnshowable }, diagnostic.Origin{}, true},
		{"raw-leaf", func(*diagnostic.Holder) (*View, error) { viewOriginLeafA(); return nil, nil }, a, false},
		{"repanic-known", func(*diagnostic.Holder) (*View, error) { panic(diagnostic.Failure(diagnostic.Unshowable, a)) }, a, false},
		{"repanic-unknown", func(*diagnostic.Holder) (*View, error) { panic(diagnostic.Failure(diagnostic.Unshowable, unknown)) }, unknown, false},
		{"legacy-adapter-repanic", func(*diagnostic.Holder) (*View, error) { panic(&unreadableError{path: "invented.jsonl", origin: a}) }, a, false},
		{"unknown-before-repanic", func(h *diagnostic.Holder) (*View, error) {
			h.Note(unknown)
			panic(diagnostic.Failure(diagnostic.Unshowable, b))
		}, unknown, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			ctx, h := diagnostic.WithHolder(context.Background())
			v, err := c.ViewContextWithDiagnostics(ctx, viewKey(tc.name), tc.build)
			finishViews(t, c)
			if v != nil || !errors.Is(err, ErrUnshowable) || PanicOrigin(err) != tc.want || h.Origin() != tc.want || err.Error() != "transcript: could not be shown" {
				t.Fatal("new entry point changed bare/closed/raw panic contract")
			}
			if tc.bare && err != ErrUnshowable {
				t.Fatal("originless refusal lost singleton identity")
			}
		})
	}
}

func TestViewDiagnosticsBridgeDetachedWaitersAndLegacyJoin(t *testing.T) {
	for _, unwind := range []bool{false, true} {
		for _, origin := range []diagnostic.Origin{viewOriginA(t), viewOriginUnknown(t)} {
			t.Run(fmt.Sprintf("unwind=%v/known=%v", unwind, origin == viewOriginA(t)), func(t *testing.T) {
				c, _ := testCache(t, &fakeParse{}, nil)
				key := viewKey("detached-diagnostics")
				base1, h1 := diagnostic.WithHolder(context.Background())
				ctx1, cancel1 := context.WithCancel(base1)
				ctx2, h2 := diagnostic.WithHolder(context.Background())
				ctx3, h3 := diagnostic.WithHolder(context.Background())
				gate, release := viewGate()
				entered := make(chan struct{})
				var wg sync.WaitGroup
				var worker *diagnostic.Holder
				var builds, wrong atomic.Int64
				start := func(fn func() (*View, error)) <-chan viewResult {
					out := make(chan viewResult, 1)
					wg.Add(1)
					go func() { defer wg.Done(); v, e := fn(); out <- viewResult{v, e} }()
					return out
				}
				t.Cleanup(func() { cancel1(); release(); wg.Wait(); finishViews(t, c) })
				ordinary := errors.New("invented stale selection")
				partial := &View{Value: "partial", Bytes: 3}
				first := start(func() (*View, error) {
					return c.ViewContextWithDiagnostics(ctx1, key, func(h *diagnostic.Holder) (*View, error) {
						builds.Add(1)
						worker = h
						close(entered)
						<-gate
						localCtx, local := diagnostic.WithHolder(context.Background())
						defer func() { h.Note(local.Origin()) }()
						diagnostic.Note(localCtx, origin)
						if unwind {
							viewOriginClockB()
						}
						return partial, ordinary
					})
				})
				signalView(t, entered)
				joined2 := &observedFlightContext{Context: ctx2, seen: make(chan struct{})}
				second := start(func() (*View, error) {
					return c.ViewContextWithDiagnostics(joined2, key, func(*diagnostic.Holder) (*View, error) { wrong.Add(1); return simpleView() })
				})
				signalView(t, joined2.seen)
				joined3 := &observedFlightContext{Context: ctx3, seen: make(chan struct{})}
				third := start(func() (*View, error) {
					return c.ViewContext(joined3, key, func() (*View, error) { wrong.Add(1); return simpleView() })
				})
				signalView(t, joined3.seen)
				if worker == nil || worker == h1 || worker == h2 || worker == h3 || worker.Origin().Present() || c.Stats().BuildingViews != 1 || len(c.viewSem) != 1 {
					t.Fatal("join retained caller holder/created another operation")
				}
				cancel1()
				r1 := receiveView(t, first)
				if r1.v != nil || r1.err != context.Canceled || h1.Origin().Present() {
					t.Fatal("unfinished canceled caller saw future metadata")
				}
				release()
				r2, r3 := receiveView(t, second), receiveView(t, third)
				wg.Wait()
				finishViews(t, c)
				for _, r := range []viewResult{r2, r3} {
					if unwind {
						if r.v != nil || !errors.Is(r.err, ErrUnshowable) || PanicOrigin(r.err) != origin {
							t.Fatal("surviving joiner lost unwind A carrier")
						}
					} else if r.v != partial || r.err != ordinary {
						t.Fatal("joined ordinary pointer/error pair changed")
					}
				}
				if h1.Origin().Present() || h2.Origin() != origin || h3.Origin() != origin || wrong.Load() != 0 || builds.Load() != 1 {
					t.Fatal("terminal value bridge lost/joined/retained wrong caller")
				}
			})
		}
	}
}
