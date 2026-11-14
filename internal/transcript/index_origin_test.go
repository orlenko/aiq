package transcript

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/orlenko/aiq/internal/diagnostic"
)

func indexOriginUnknown(t *testing.T) diagnostic.Origin {
	t.Helper()
	o := panicOriginCaught(panicOriginExternalLeaf)
	if _, _, known := o.Location(); !o.Present() || known {
		t.Fatal("external head acquired an allowed caller")
	}
	return o
}
func indexOriginJoin(done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-time.After(3 * time.Second):
		return errors.New("scan did not complete")
	}
}
func indexOriginState(s *scan) (any, scanObservation, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, s.observation, s.running
}

// A warm refresh is held before its actual run, so done is captured before
// publication can clear running. No request context enters the worker.
func indexOriginPublish(x *Index, s *scan, run func(any, *diagnostic.Holder) any) error {
	gate := make(chan struct{})
	_, _, err := x.readObserved(context.Background(), s, func(p any, h *diagnostic.Holder) any { <-gate; return run(p, h) })
	_, _, done := indexOriginState(s)
	close(gate)
	if err != nil {
		return err
	}
	if done == nil {
		return errors.New("warm refresh was not launched")
	}
	return indexOriginJoin(done)
}

func indexOriginWork(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
func indexOriginRollout(t *testing.T, root, tag, id string) string {
	t.Helper()
	return writeFile(t, filepath.Join(root, "rollout-2026-01-01-"+tag+"-"+id+".jsonl"), "invented metadata")
}
func indexOriginHead(dir, bad, mode string, calls *atomic.Int64) func(string) (codexPayload, bool) {
	return func(path string) (codexPayload, bool) {
		calls.Add(1)
		if path == bad {
			switch mode {
			case "known":
				panicOriginLeafA()
			case "unknown":
				panicOriginExternalLeaf()
			}
		}
		return codexPayload{Cwd: dir, Source: []byte(`"cli"`)}, true
	}
}

type indexOriginValueContext struct {
	context.Context
	observe func()
}

func (c indexOriginValueContext) Value(k any) any { c.observe(); return c.Context.Value(k) }

func TestIndexOriginCompletedAttemptTuple(t *testing.T) {
	a := panicOriginA(t)
	b := panicOriginCaught(func() { panicOriginLeafB() })
	panicOriginWant(t, b, "internal/transcript/panic_leaf_test.go", 10)
	for _, mode := range []string{"known", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			x := NewIndex(Roots{CodexSessions: []string{t.TempDir()}})
			clk := newClock()
			x.now = clk.Now
			dir := indexOriginWork(t)
			good := indexOriginRollout(t, x.r.CodexSessions[0], "good", idA)
			bad := indexOriginRollout(t, x.r.CodexSessions[0], "bad", idB)
			_ = good
			var heads atomic.Int64
			healthy := func(p any, h *diagnostic.Holder) any {
				return x.scanCodexWithMeta(nil, h, indexOriginHead(dir, bad, "none", &heads))
			}
			d0, o0, e := x.readObserved(context.Background(), &x.codex, healthy)
			if e != nil || d0 == nil || o0 != (scanObservation{attemptGen: 1, dataGen: 1, outcome: scanHealthy}) {
				t.Fatal("initial healthy tuple")
			}
			clk.Add(x.ttl)
			entered, gate := make(chan struct{}), make(chan struct{})
			var release sync.Once
			open := func() { release.Do(func() { close(gate) }) }
			var done <-chan struct{}
			t.Cleanup(func() {
				open()
				if done != nil {
					if e := indexOriginJoin(done); e != nil {
						t.Error(e)
					}
				}
			})
			_, _, e = x.readObserved(context.Background(), &x.codex, func(p any, h *diagnostic.Holder) any {
				x.scanCodexWithMeta(nil, h, indexOriginHead(dir, bad, mode, &heads))
				close(entered)
				<-gate
				panicOriginLeafB()
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			signalView(t, entered)
			_, _, done = indexOriginState(&x.codex)
			base, h0 := diagnostic.WithHolder(context.Background())
			var locked atomic.Bool
			ctx := indexOriginValueContext{Context: base, observe: func() {
				if !x.codex.mu.TryLock() {
					locked.Store(true)
				} else {
					x.codex.mu.Unlock()
				}
			}}
			old, oldObs, e := x.readObserved(ctx, &x.codex, healthy)
			if e != nil || old != d0 || oldObs != o0 || h0.Origin().Present() {
				t.Fatal("old reader borrowed pending fault/generation")
			}
			open()
			if e := indexOriginJoin(done); e != nil {
				t.Fatal(e)
			}
			want := a
			if mode == "unknown" {
				want = indexOriginUnknown(t)
			}
			fresh, h1 := diagnostic.WithHolder(context.Background())
			latestCtx := indexOriginValueContext{Context: fresh, observe: func() {
				if !x.codex.mu.TryLock() {
					locked.Store(true)
				} else {
					x.codex.mu.Unlock()
				}
			}}
			d1, o1, e := x.readObserved(latestCtx, &x.codex, healthy)
			if e != nil || d1 != d0 || o1 != (scanObservation{attemptGen: 2, dataGen: 1, outcome: scanFallbackOld, origin: want}) || h1.Origin() != want || locked.Load() {
				t.Fatal("aborted attempt lost first head origin or retained-data generation/unlock")
			}
			clk.Add(x.ttl)
			partial := func(p any, h *diagnostic.Holder) any {
				return x.scanCodexWithMeta(nil, h, indexOriginHead(dir, bad, mode, &heads))
			}
			if e := indexOriginPublish(x, &x.codex, partial); e != nil {
				t.Fatal(e)
			}
			d2, o2, _ := indexOriginState(&x.codex)
			if d2 == d0 || o2 != (scanObservation{attemptGen: 3, dataGen: 3, outcome: scanPartial, origin: want}) {
				t.Fatal("head fault published as healthy")
			}
			clk.Add(x.ttl)
			if e := indexOriginPublish(x, &x.codex, func(any, *diagnostic.Holder) any { panicOriginLeafB(); return nil }); e != nil {
				t.Fatal(e)
			}
			d3, o3, _ := indexOriginState(&x.codex)
			if d3 != d2 || o3 != (scanObservation{attemptGen: 4, dataGen: 3, outcome: scanFallbackOld, origin: b}) {
				t.Fatal("aborted attempt borrowed prior partial-data origin")
			}
			clk.Add(x.ttl)
			if e := indexOriginPublish(x, &x.codex, healthy); e != nil {
				t.Fatal(e)
			}
			_, o4, _ := indexOriginState(&x.codex)
			if o4 != (scanObservation{attemptGen: 5, dataGen: 5, outcome: scanHealthy}) || o1.origin != want || oldObs != o0 || o2.origin != want {
				t.Fatal("healthy clear mutated old captured observations")
			}
		})
	}
	// The first failed attempt has no data object on which to store provenance.
	x := NewIndex(Roots{})
	ctx, h := diagnostic.WithHolder(context.Background())
	d, o, e := x.readObserved(ctx, &x.codex, func(any, *diagnostic.Holder) any { panicOriginLeafA(); return nil })
	if e != nil || d != nil || o != (scanObservation{attemptGen: 1, outcome: scanFallbackEmpty, origin: a}) || h.Origin() != a {
		t.Fatal("cold nil fallback erased origin")
	}
}

type indexOriginErrOrderContext struct {
	context.Context
	holder *diagnostic.Holder
	want   diagnostic.Origin
	calls  atomic.Int64
	wrong  atomic.Bool
}

func (c *indexOriginErrOrderContext) Err() error {
	c.calls.Add(1)
	if c.holder.Origin() != c.want {
		c.wrong.Store(true)
	}
	return c.Context.Err()
}

func TestIndexOriginColdCancelAvailableOnly(t *testing.T) {
	a := panicOriginA(t)
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			// Both ready select cases are legal. Require actual cancel-branch
			// coverage, rather than passing on success-branch observations only.
			for attempt := 0; attempt < 64; attempt++ {
				root := t.TempDir()
				x := NewIndex(Roots{CodexSessions: []string{root}})
				bad := indexOriginRollout(t, root, "pending-fault", idB)
				dir := indexOriginWork(t)
				var heads atomic.Int64
				pending := make(chan struct{})
				base, h := diagnostic.WithHolder(context.Background())
				parent, cancel := context.WithCancel(base)
				wantAtErr := diagnostic.Origin{}
				if ready {
					wantAtErr = a
				}
				checked := &indexOriginErrOrderContext{Context: parent, holder: h, want: wantAtErr}
				ctx := &panicOriginGateContext{Context: checked, entered: make(chan struct{}), release: make(chan struct{})}
				gate, release := viewGate()
				var open sync.Once
				unblock := func() { open.Do(func() { close(ctx.release) }) }
				type result struct {
					data        any
					observation scanObservation
					err         error
				}
				out := make(chan result, 1)
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					d, o, e := x.readObserved(ctx, &x.codex, func(_ any, faults *diagnostic.Holder) any {
						x.scanCodexWithMeta(nil, faults, indexOriginHead(dir, bad, "known", &heads))
						close(pending)
						<-gate
						panicOriginLeafB()
						return nil
					})
					out <- result{d, o, e}
				}()
				var done <-chan struct{}
				t.Cleanup(func() {
					cancel()
					unblock()
					release()
					wg.Wait()
					if done != nil {
						if e := indexOriginJoin(done); e != nil {
							t.Error(e)
						}
					}
				})
				signalView(t, ctx.entered)
				signalView(t, pending)
				_, _, done = indexOriginState(&x.codex)
				if ready {
					release()
					if e := indexOriginJoin(done); e != nil {
						t.Fatal(e)
					}
				}
				cancel()
				unblock()
				wg.Wait()
				r := <-out
				if ready {
					if r.data != nil || (r.err != nil && r.err != context.Canceled) || r.observation != (scanObservation{attemptGen: 1, outcome: scanFallbackEmpty, origin: a}) || h.Origin() != a {
						t.Fatal("ready abort origin lost to cancellation")
					}
				} else {
					if r.data != nil || r.err != context.Canceled || r.observation != (scanObservation{}) || h.Origin().Present() {
						t.Fatal("unfinished scan supplied pending fields")
					}
					release()
					if e := indexOriginJoin(done); e != nil {
						t.Fatal(e)
					}
					if h.Origin().Present() {
						t.Fatal("detached scan retained canceled caller holder")
					}
				}
				if checked.wrong.Load() {
					t.Fatal("ctx.Err evaluated before selected origin was noted")
				}
				if !ready || checked.calls.Load() > 0 {
					t.Logf("cold ready=%v: cancellation Err branch observed after %d attempt(s)", ready, attempt+1)
					return
				}
			}
			t.Fatal("ready select did not exercise cancellation branch within bounded attempts")
		})
	}
}

type indexOriginStepContext struct {
	context.Context
	n    atomic.Int64
	step func() error
}

func (c *indexOriginStepContext) Err() error {
	if c.n.Add(1) == 2 {
		if e := c.step(); e != nil {
			return e
		}
	}
	return c.Context.Err()
}

func TestIndexOriginMemoHitUsesActualCandidateObservation(t *testing.T) {
	a := panicOriginA(t)
	u := indexOriginUnknown(t)
	b := panicOriginCaught(func() { panicOriginLeafB() })
	for _, tc := range []struct{ old, next string }{{"known", "none"}, {"unknown", "none"}, {"none", "known"}, {"none", "unknown"}} {
		t.Run(tc.old+"-to-"+tc.next, func(t *testing.T) {
			r, _ := findFixture(t)
			dir := indexOriginWork(t)
			x := NewIndex(r)
			clk := newClock()
			x.now = clk.Now
			first := indexOriginRollout(t, r.CodexSessions[0], "first", idA)
			bad := indexOriginRollout(t, r.CodexSessions[0], "fault", idB)
			setMtime(t, first, t0)
			var heads, parses atomic.Int64
			run := func(mode string) func(any, *diagnostic.Holder) any {
				return func(p any, h *diagnostic.Holder) any {
					return x.scanCodexWithMeta(nil, h, indexOriginHead(dir, bad, mode, &heads))
				}
			}
			_, g0, e := x.readObserved(context.Background(), &x.codex, run(tc.old))
			if e != nil {
				t.Fatal(e)
			}
			second := indexOriginRollout(t, r.CodexSessions[1], "second", idA)
			setMtime(t, second, t0.Add(time.Hour))
			want := diagnostic.Origin{}
			outcome := scanHealthy
			if tc.next != "none" {
				want = a
				outcome = scanPartial
			}
			if tc.next == "unknown" {
				want = u
			}
			wantObservation := scanObservation{attemptGen: 2, dataGen: 2, outcome: outcome, origin: want}
			readerEntered, readerGate := make(chan struct{}), make(chan struct{})
			var release sync.Once
			unblock := func() { release.Do(func() { close(readerGate) }) }
			var hooks atomic.Int64
			x.beforeMemoLookup = func() {
				if hooks.Add(1) == 1 {
					close(readerEntered)
					<-readerGate
				}
			}
			parse := func(context.Context, string) (Summary, error) { parses.Add(1); return Summary{}, nil }
			rbase, rh := diagnostic.WithHolder(context.Background())
			var afterCopy sync.Once
			var readerStepErr error
			var locked atomic.Bool
			rctx := indexOriginValueContext{Context: rbase, observe: func() {
				if !x.mu.TryLock() {
					locked.Store(true)
				} else {
					x.mu.Unlock()
				}
				afterCopy.Do(func() {
					clk.Add(x.ttl)
					readerStepErr = indexOriginPublish(x, &x.codex, func(any, *diagnostic.Holder) any { panicOriginLeafB(); return nil })
				})
			}}
			type result struct {
				hit Hit
				err error
			}
			reader, writer := make(chan result, 1), make(chan result, 1)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); h, e := x.Find(rctx, "codex", idA, parse); reader <- result{h, e} }()
			t.Cleanup(func() {
				unblock()
				wg.Wait()
				_, _, pending := indexOriginState(&x.codex)
				if pending != nil {
					if e := indexOriginJoin(pending); e != nil {
						t.Error(e)
					}
				}
			})
			signalView(t, readerEntered)
			wbase, wh := diagnostic.WithHolder(context.Background())
			diagnostic.Note(wbase, b) // unrelated earlier request fault MUST NOT become memo provenance
			wctx := &indexOriginStepContext{Context: wbase, step: func() error { clk.Add(x.ttl); return indexOriginPublish(x, &x.codex, run(tc.next)) }}
			wg.Add(1)
			go func() { defer wg.Done(); h, e := x.Find(wctx, "codex", idA, parse); writer <- result{h, e} }()
			var w result
			select {
			case w = <-writer:
			case <-time.After(3 * time.Second):
				t.Fatal("writer did not finish new-source miss")
			}
			wantHit := Hit{Path: second, Root: r.CodexSessions[1], Account: "codex/acct-b"}
			if w.err != nil || w.hit != wantHit || wh.Origin() != b {
				t.Fatal("writer changed selected copy or aggregate holder")
			}
			x.mu.Lock()
			entry := x.memo["codex\x00"+idA]
			memoCount := len(x.memo)
			x.mu.Unlock()
			if entry.hit != wantHit || entry.gen != g0.attemptGen || entry.gen != 1 || entry.observation != wantObservation || memoCount != 1 || entry.at != clk.Now() {
				t.Fatal("legacy validity tag and actual source provenance were conflated")
			}
			beforeWalks, beforeHeads, beforeParses := x.Walks(), heads.Load(), parses.Load()
			unblock()
			var rr result
			select {
			case rr = <-reader:
			case <-time.After(3 * time.Second):
				t.Fatal("late old-tag reader did not finish")
			}
			wg.Wait()
			if rr.err != nil || rr.hit != wantHit || rh.Origin() != want || readerStepErr != nil || locked.Load() {
				t.Fatal("memo hit borrowed older/newer/aggregate origin or noted while locked")
			}
			if x.Walks() != beforeWalks || heads.Load() != beforeHeads || parses.Load() != beforeParses || beforeParses != 0 || hooks.Load() != 2 {
				t.Fatal("memo HIT did extra scan/head/parse/discovery work")
			}
			// Nonempty G1 metadata triggers a controlled G2 completion AFTER entry
			// copy in Value; its B must not replace the copied G1 value.
			if want.Present() {
				_, latest, _ := indexOriginState(&x.codex)
				if latest.attemptGen != 3 || latest.origin != b || entry.observation != wantObservation {
					t.Fatal("controlled after-copy generation was not distinct")
				}
			}
		})
	}
}

func TestIndexOriginDirectProvidersHaveNoScanProvenance(t *testing.T) {
	a := panicOriginA(t)
	b := panicOriginCaught(func() { panicOriginLeafB() })
	for _, provider := range []string{"claude", "agy"} {
		t.Run(provider, func(t *testing.T) {
			r, _ := findFixture(t)
			x := NewIndex(r)
			clk := newClock()
			x.now = clk.Now
			_, _, e := x.readObserved(context.Background(), &x.agy, func(any, *diagnostic.Holder) any { return &agyScan{byDir: map[string][]Candidate{}} })
			if e != nil {
				t.Fatal(e)
			}
			clk.Add(x.ttl)
			if e := indexOriginPublish(x, &x.agy, func(any, *diagnostic.Holder) any { panicOriginLeafB(); return nil }); e != nil {
				t.Fatal(e)
			}
			var wantHit Hit
			calls := 0
			if provider == "claude" {
				first := writeFile(t, filepath.Join(r.ClaudeProjects[0], "-w", idA+".jsonl"), "invented-first")
				second := writeFile(t, filepath.Join(r.ClaudeProjects[1], "-w", idA+".jsonl"), "invented-second")
				setMtime(t, first, t0)
				setMtime(t, second, t0)
				wantHit = Hit{Path: first, Root: r.ClaudeProjects[0]}
			} else {
				p := writeFile(t, AgyTranscript(r.AgyAppData[0], idA), "invented")
				wantHit = Hit{Path: p, Root: r.AgyAppData[0]}
			}
			parse := func(context.Context, string) (Summary, error) {
				calls++
				return Summary{}, &unreadableError{path: "invented", origin: a}
			}
			ctx, h := diagnostic.WithHolder(context.Background())
			hit, e := x.Find(ctx, provider, idA, parse)
			wantOrigin := diagnostic.Origin{}
			wantCalls := 0
			if provider == "claude" {
				wantOrigin = a
				wantCalls = 2
			}
			if e != nil || hit != wantHit || h.Origin() != wantOrigin || calls != wantCalls {
				t.Fatal("direct provider borrowed unrelated scan provenance or changed tie fallback")
			}
			x.mu.Lock()
			entry := x.memo[provider+"\x00"+idA]
			x.mu.Unlock()
			if entry.observation != (scanObservation{}) {
				t.Fatal("direct candidate lookup stored scan or tie-parser provenance")
			}
			fresh, fh := diagnostic.WithHolder(context.Background())
			again, e := x.Find(fresh, provider, idA, parse)
			if e != nil || again != hit || fh.Origin().Present() || calls != wantCalls || x.Walks() != 0 {
				t.Fatal("direct memo hit replayed fault or did new work")
			}
			_, obs, _ := indexOriginState(&x.agy)
			if obs.origin != b {
				t.Fatal("fixture's unrelated scan fault was not retained")
			}
		})
	}
}

func TestIndexOriginActualIndexedAndDirectHeadGuards(t *testing.T) {
	a, u := panicOriginA(t), indexOriginUnknown(t)
	for _, mode := range []string{"known", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			dir := indexOriginWork(t)
			root := t.TempDir()
			good := indexOriginRollout(t, root, "good", idA)
			bad := indexOriginRollout(t, root, "bad", idB)
			x := NewIndex(Roots{CodexSessions: []string{root}})
			clk := newClock()
			x.now = clk.Now
			var calls atomic.Int64
			want := a
			if mode == "unknown" {
				want = u
			}
			ctx, h := diagnostic.WithHolder(context.Background())
			var locked atomic.Bool
			ctx = indexOriginValueContext{Context: ctx, observe: func() {
				if !x.codex.mu.TryLock() {
					locked.Store(true)
				} else {
					x.codex.mu.Unlock()
				}
			}}
			run := func(p any, f *diagnostic.Holder) any {
				return x.scanCodexWithMeta(p, f, indexOriginHead(dir, bad, mode, &calls))
			}
			data, obs, e := x.readObserved(ctx, &x.codex, run)
			cs := data.(*codexScan)
			if e != nil || obs != (scanObservation{attemptGen: 1, dataGen: 1, outcome: scanPartial, origin: want}) || h.Origin() != want || locked.Load() || len(cs.all) != 2 || len(cs.byCwd[dir]) != 1 || cs.byCwd[dir][0].Path != good || cs.seen[bad].classified || !cs.seen[good].classified || calls.Load() != 2 {
				t.Fatal("indexed guard changed failed-head omission/classification or origin")
			}
			clk.Add(x.ttl)
			healthy := func(p any, f *diagnostic.Holder) any {
				return x.scanCodexWithMeta(p, f, indexOriginHead(dir, bad, "none", &calls))
			}
			if e := indexOriginPublish(x, &x.codex, healthy); e != nil {
				t.Fatal(e)
			}
			data, next, _ := indexOriginState(&x.codex)
			if next != (scanObservation{attemptGen: 2, dataGen: 2, outcome: scanHealthy}) || len(data.(*codexScan).byCwd[dir]) != 2 || calls.Load() != 3 {
				t.Fatal("healthy retry did not clear fault/reuse successful head")
			}
			base, dh := diagnostic.WithHolder(context.Background())
			var active atomic.Int64
			var premature atomic.Bool
			var directCalls atomic.Int64
			directCtx := indexOriginValueContext{Context: base, observe: func() {
				if active.Load() != 0 {
					premature.Store(true)
				}
			}}
			head := indexOriginHead(dir, bad, mode, &directCalls)
			out := codexFilesWithMeta(directCtx, []string{root}, dir, time.Time{}, func(path string) (codexPayload, bool) { active.Add(1); defer active.Add(-1); return head(path) })
			if len(out) != 1 || out[0].Path != good || dh.Origin() != want || premature.Load() || active.Load() != 0 || directCalls.Load() != 2 {
				t.Fatal("direct guard did not join/omit failed head before noting")
			}
			fresh, fh := diagnostic.WithHolder(context.Background())
			out = codexFilesWithMeta(fresh, []string{root}, dir, time.Time{}, indexOriginHead(dir, bad, "none", &directCalls))
			if len(out) != 2 || fh.Origin().Present() {
				t.Fatal("direct healthy retry retained prior fault")
			}
		})
	}
	type oldMemo struct {
		hit Hit
		gen uint64
		at  time.Time
	}
	type oldScan struct {
		mu      sync.Mutex
		gen     uint64
		at      time.Time
		data    any
		running chan struct{}
	}
	if memoMax != 4096 || unsafe.Sizeof(memoEntry{})-unsafe.Sizeof(oldMemo{}) != unsafe.Sizeof(scanObservation{}) || unsafe.Sizeof(scan{})-unsafe.Sizeof(oldScan{}) != unsafe.Sizeof(scanObservation{}) {
		t.Fatal("observation escaped existing memo/provider slots or layout")
	}
	t.Logf("count-bounded layout bytes: observation=%d scan=%d memoEntry=%d Index=%d; two provider slots and memoMax=%d, no byte-budget claim", unsafe.Sizeof(scanObservation{}), unsafe.Sizeof(scan{}), unsafe.Sizeof(memoEntry{}), unsafe.Sizeof(Index{}), memoMax)
}
