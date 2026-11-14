package transcript

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/orlenko/aiq/internal/diagnostic"
)

func panicOriginCaught(fn func()) (o diagnostic.Origin) {
	defer func() {
		if v := recover(); v != nil {
			o = capturePanic(v)
		}
	}()
	fn()
	return
}

func panicOriginWant(t *testing.T, o diagnostic.Origin, file string, line uint32) {
	t.Helper()
	f, l, known := o.Location()
	if !o.Present() || !known || f != file || l != line {
		t.Fatalf("origin=%q:%d known=%v; want %q:%d", f, l, known, file, line)
	}
}

func panicOriginA(t *testing.T) diagnostic.Origin {
	t.Helper()
	o := panicOriginCaught(panicOriginLeafA)
	panicOriginWant(t, o, "internal/transcript/panic_leaf_test.go", 8)
	return o
}

type panicOriginHostile struct{ calls *int }

func (h panicOriginHostile) Error() string          { *h.calls++; panic("Error called") }
func (h panicOriginHostile) Is(error) bool          { *h.calls++; panic("Is called") }
func (h panicOriginHostile) Unwrap() error          { *h.calls++; panic("Unwrap called") }
func (h panicOriginHostile) Format(fmt.State, rune) { *h.calls++; panic("Format called") }

func TestPanicOriginClosedLegacyAdapterAndRepanic(t *testing.T) {
	o := panicOriginA(t)
	path := "fixture/quoted\"\n.jsonl"
	e := &unreadableError{path: path, origin: o}
	if ErrUnreadable != diagnostic.Base(diagnostic.Unreadable) || ErrInspection != diagnostic.Base(diagnostic.Inspection) {
		t.Fatal("fixed base aliases differ")
	}
	if e.Error() != "transcript could not be read: "+strconv.Quote(path) || !errors.Is(e, ErrUnreadable) || PanicOrigin(e) != o {
		t.Fatal("legacy quoted error/origin contract changed")
	}
	fixed := diagnostic.Failure(diagnostic.Unreadable, PanicOrigin(e))
	if fixed.Error() != "transcript could not be read" || !errors.Is(fixed, ErrUnreadable) || strings.Contains(fixed.Error(), "fixture") {
		t.Fatal("fixed rebase retained legacy path")
	}
	if got := panicOriginCaught(func() { panic(e) }); got != o {
		t.Fatal("trusted legacy re-panic lost first origin")
	}
	calls := 0
	hostile := panicOriginHostile{&calls}
	if PanicOrigin(hostile).Present() || PanicOrigin(fmt.Errorf("wrapped: %w", e)).Present() || calls != 0 {
		t.Fatal("extractor followed arbitrary methods/graphs")
	}
	if !panicOriginCaught(func() { panic(hostile) }).Present() || calls != 0 {
		t.Fatal("capture queried hostile panic")
	}
	var nilAdapter *unreadableError
	if PanicOrigin(nilAdapter).Present() || !panicOriginCaught(func() { panic(nilAdapter) }).Present() {
		t.Fatal("typed nil adapter handling")
	}
	if PanicOrigin(ErrUnreadable).Present() {
		t.Fatal("bare sentinel fabricated origin")
	}
}

func TestPanicOriginParserReturnedAdapterAndQuarantine(t *testing.T) {
	known := panicOriginA(t)
	unknown := panicOriginCaught(panicOriginExternalLeaf)
	if _, _, k := unknown.Location(); !unknown.Present() || k {
		t.Fatal("external leaf was attributed to its caller")
	}
	for _, mode := range []string{"panic", "returned", "unknown", "bare"} {
		t.Run(mode, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), inspectionID+".jsonl"), "invented")
			c := newCache(defaultCacheConfig)
			var n atomic.Int64
			want := known
			if mode == "unknown" {
				want = unknown
			}
			if mode == "bare" {
				want = diagnostic.Origin{}
			}
			c.parse = func(string) (*Session, error) {
				n.Add(1)
				if mode == "panic" {
					panicOriginLeafA()
				}
				if mode == "bare" {
					return nil, ErrUnreadable
				}
				return nil, &unreadableError{path: path, origin: want}
			}
			for i := 0; i < 3; i++ {
				ctx, h := diagnostic.WithHolder(context.Background())
				s, snap, e := c.Session(ctx, path)
				if s != nil || !errors.Is(e, ErrUnreadable) || PanicOrigin(e) != want || h.Origin() != want || snap.Path != path {
					t.Fatal("parse/quarantine replay changed outcome or first origin")
				}
				ctx, h = diagnostic.WithHolder(context.Background())
				if _, e := c.Summary(ctx, path); !errors.Is(e, ErrUnreadable) || h.Origin() != want {
					t.Fatal("summary quarantine lost first origin")
				}
			}
			if n.Load() != 1 || c.Stats().Parses != 1 || len(c.quarantine) != 1 || c.Stats().Rows != 0 || c.Stats().Parsed != 0 {
				t.Fatal("quarantine replay performed extra work or retained row/session")
			}
			if c.cfg != defaultCacheConfig {
				t.Fatal("diagnostic changed resource defaults")
			}
		})
	}
	// ParseFile itself catches the real leaf; the caller never self-Notes.
	restore := SetParserForTest(func(string) (*Session, error) { panicOriginLeafA(); return nil, nil })
	defer restore()
	s, e := ParseFile("fixture.jsonl")
	if s != nil || !errors.Is(e, ErrUnreadable) || PanicOrigin(e) != known {
		t.Fatal("ParseFile did not preserve literal leaf A")
	}
}

func TestPanicOriginQuarantineFirstUntilReplacementEviction(t *testing.T) {
	a := panicOriginA(t)
	b := panicOriginCaught(func() { panicOriginLeafB() })
	panicOriginWant(t, b, "internal/transcript/panic_leaf_test.go", 10)
	cfg := defaultCacheConfig
	cfg.quarantineCount = 1
	c := newCache(cfg)
	snap := Snapshot{Path: "invented-a", Size: 1}
	c.quarantinedOrigin(snap, a)
	if e := c.quarantinedOrigin(snap, b); PanicOrigin(e) != a || c.quarantine[snap.Path].origin != a {
		t.Fatal("same Snapshot replaced first origin")
	}
	el := c.quarantineNodes[snap.Path]
	if el.Value.(Snapshot) != snap {
		t.Fatal("quarantine LRU ceased preserving Snapshot value")
	}
	snap.Size++
	if e := c.quarantinedOrigin(snap, b); PanicOrigin(e) != b {
		t.Fatal("changed Snapshot inherited old origin")
	}
	c.quarantinedOrigin(Snapshot{Path: "invented-b"}, a)
	if len(c.quarantine) != 1 || c.quarantineNodes[snap.Path] != nil {
		t.Fatal("origin escaped existing failure cap")
	}
	if e := c.quarantinedOrigin(snap, a); PanicOrigin(e) != a {
		t.Fatal("evicted marker retained old origin")
	}
	unknown := panicOriginCaught(panicOriginExternalLeaf)
	c.quarantinedOrigin(Snapshot{Path: "unknown"}, unknown)
	if e := c.quarantinedOrigin(Snapshot{Path: "unknown"}, a); PanicOrigin(e) != unknown {
		t.Fatal("known overwrote first unknown")
	}
	backing := strings.Repeat("x", 65536) + "borrowed-quarantine" + strings.Repeat("y", 65536)
	borrowed := backing[65536 : 65536+len("borrowed-quarantine")]
	ownedSnap := Snapshot{Path: borrowed, Size: 9}
	c.quarantined(ownedSnap) // a bare ErrUnreadable has no invented origin
	ownedPath := c.quarantine[borrowed].Path
	if unsafe.StringData(ownedPath) == unsafe.StringData(borrowed) {
		t.Fatal("initial quarantine retained borrowed path")
	}
	c.quarantinedOrigin(ownedSnap, a)
	for key, record := range c.quarantine {
		if record.origin != a || unsafe.StringData(key) != unsafe.StringData(ownedPath) || unsafe.StringData(record.Path) != unsafe.StringData(ownedPath) {
			t.Fatal("first-present origin replacement lost owned key/path backing")
		}
	}
	if unsafe.Sizeof(quarantineRecord{})-unsafe.Sizeof(Snapshot{}) != unsafe.Sizeof(diagnostic.Origin{}) {
		t.Fatal("unexpected quarantine metadata layout")
	}
	t.Logf("count-bounded layout bytes: Origin=%d Snapshot=%d quarantineRecord=%d flight=%d; caps unchanged", unsafe.Sizeof(diagnostic.Origin{}), unsafe.Sizeof(Snapshot{}), unsafe.Sizeof(quarantineRecord{}), unsafe.Sizeof(flight{}))
}

func panicOriginFlight(c *Cache, snap Snapshot) *flight {
	f := &flight{snap: snap, done: make(chan struct{})}
	c.sem <- struct{}{}
	c.mu.Lock()
	c.flights[snap.Path] = f
	c.mu.Unlock()
	return f
}

func TestPanicOriginRowFaultFullSuccessAndLaterPublication(t *testing.T) {
	for _, later := range []bool{false, true} {
		c := newCache(defaultCacheConfig)
		c.cfg.rowCount = -1
		snap := Snapshot{Path: "invented-row-full"}
		s := &Session{Provider: "claude", ID: inspectionID, Path: snap.Path, Turns: []Turn{{Prompt: "p"}}}
		f := panicOriginFlight(c, snap)
		f.row, f.full = true, true
		var rowBeforeB error
		if later {
			c.now = func() time.Time { rowBeforeB = f.rowErr; return panicOriginLeafB() }
		}
		c.parse = func(string) (*Session, error) { return s, nil }
		c.fly(f) // actual parse/owned-clone/row/full terminal publication
		if !inspectionClosed(f.done) || len(c.sem) != 0 || len(c.flights) != 0 || len(c.quarantine) != 0 {
			t.Fatal("row/publication fault leaked slot/flight or quarantined healthy parse")
		}
		panicOriginWant(t, f.origin, "internal/transcript/cache.go", 796)
		if PanicOrigin(f.rowErr) != f.origin || !errors.Is(f.rowErr, ErrUnreadable) {
			t.Fatal("row origin lost before full publication")
		}
		if later {
			if rowBeforeB == nil || f.rowErr != rowBeforeB || f.sess != nil || !errors.Is(f.err, ErrUnreadable) || PanicOrigin(f.err) != f.origin {
				t.Fatal("later B overwrote first row fault A")
			}
		} else if f.sess == nil || f.err != nil || f.sess == s || c.Stats().Parsed != 1 {
			t.Fatal("row-only failure damaged full success/owned session")
		}
	}
}

type panicOriginGateContext struct {
	context.Context
	entered, release chan struct{}
	once             sync.Once
}

func (c *panicOriginGateContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Context.Done()
}

func TestPanicOriginTerminalCancellationAndNoFutureNote(t *testing.T) {
	a := panicOriginA(t)
	for _, detail := range []bool{false, true} {
		for _, ready := range []bool{false, true} {
			t.Run(fmt.Sprintf("detail=%v/ready=%v", detail, ready), func(t *testing.T) {
				path := writeFile(t, filepath.Join(t.TempDir(), inspectionID+".jsonl"), "invented")
				c := newCache(defaultCacheConfig)
				snap, e := Stat(path)
				if e != nil {
					t.Fatal(e)
				}
				f := panicOriginFlight(c, snap)
				parent, h := diagnostic.WithHolder(context.Background())
				ctx, cancel := context.WithCancel(parent)
				defer cancel()
				gate := &panicOriginGateContext{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
				var wg sync.WaitGroup
				wg.Add(1)
				done := make(chan error, 1)
				go func() {
					defer wg.Done()
					if detail {
						_, _, e := c.Session(gate, path)
						done <- e
					} else {
						_, e := c.Summary(gate, path)
						done <- e
					}
				}()
				var open sync.Once
				unblock := func() { open.Do(func() { close(gate.release) }) }
				completed := false
				complete := func() {
					if !completed {
						c.finishFlight(f, nil, &unreadableError{path: path, origin: a}, a)
						completed = true
					}
				}
				t.Cleanup(func() { cancel(); unblock(); wg.Wait(); complete() })
				select {
				case <-gate.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("waiter did not join installed flight")
				}
				if ready {
					complete()
				}
				cancel()
				unblock()
				wg.Wait()
				e = <-done
				if detail {
					if !errors.Is(e, context.Canceled) && !(ready && errors.Is(e, ErrUnreadable)) {
						t.Fatal("Session cancellation/ready selection changed")
					}
				} else if e != ErrBudget {
					t.Fatal("Summary did not preserve ErrBudget")
				}
				if ready {
					if h.Origin() != a {
						t.Fatal("ready terminal origin erased by cancellation")
					}
				} else {
					if h.Origin().Present() {
						t.Fatal("unfinished flight supplied pending origin")
					}
					complete()
					if h.Origin().Present() {
						t.Fatal("detached completion mutated abandoned caller holder")
					}
				}
				if len(c.flights) != 0 || len(c.sem) != 0 {
					t.Fatal("terminal fixture did not release registry/slot")
				}
			})
		}
	}
}

type panicOriginBudgetContext struct {
	context.Context
	c         *Cache
	n         atomic.Int64
	underLock atomic.Bool
}

func (c *panicOriginBudgetContext) Err() error {
	if c.n.Add(1) >= 2 {
		return context.Canceled
	}
	return nil
}
func (c *panicOriginBudgetContext) Value(k any) any {
	if !c.c.mu.TryLock() {
		c.underLock.Store(true)
	} else {
		c.c.mu.Unlock()
	}
	return c.Context.Value(k)
}

func TestPanicOriginCachedSummaryBudgetAfterUnlock(t *testing.T) {
	a := panicOriginA(t)
	c := newCache(defaultCacheConfig)
	path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
	snap, e := Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	c.quarantinedOrigin(snap, a)
	base, h := diagnostic.WithHolder(context.Background())
	ctx := &panicOriginBudgetContext{Context: base, c: c}
	s, e := c.Summary(ctx, path)
	if e != ErrBudget || s != (Summary{}) || h.Origin() != a || ctx.underLock.Load() {
		t.Fatal("cached summary origin lost or noted under cache lock")
	}
	if c.Stats().Parses != 0 || len(c.quarantine) != 1 {
		t.Fatal("budget handoff did extra parse/changed quarantine")
	}
}

func TestPanicOriginTiedCopyReducers(t *testing.T) {
	a := panicOriginA(t)
	unknown := panicOriginCaught(panicOriginExternalLeaf)
	for _, indexed := range []bool{false, true} {
		for _, mode := range []string{"panic", "returned", "unknown", "ParseFile", "cancel", "ordinary", "bare"} {
			t.Run(fmt.Sprintf("index=%v/%s", indexed, mode), func(t *testing.T) {
				r, _ := findFixture(t)
				first := filepath.Join(r.ClaudeProjects[0], "-w", inspectionID+".jsonl")
				second := filepath.Join(r.ClaudeProjects[1], "-w", inspectionID+".jsonl")
				writeFile(t, first, "invented-first")
				writeFile(t, second, "invented-second")
				setMtime(t, first, t0)
				setMtime(t, second, t0)
				ctx, h := diagnostic.WithHolder(context.Background())
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				calls := 0
				want := a
				if mode == "unknown" {
					want = unknown
				}
				if mode == "ordinary" || mode == "bare" {
					want = diagnostic.Origin{}
				}
				if mode == "ParseFile" {
					restore := SetParserForTest(func(string) (*Session, error) { panicOriginLeafA(); return nil, nil })
					defer restore()
				}
				parse := func(callCtx context.Context, path string) (Summary, error) {
					calls++
					switch mode {
					case "panic":
						panicOriginLeafA()
					case "ParseFile":
						return ParseSummary(callCtx, path)
					case "ordinary":
						return Summary{}, errors.New("ordinary invented failure")
					case "bare":
						return Summary{}, ErrUnreadable
					case "cancel":
						cancel()
					}
					return Summary{}, &unreadableError{path: path, origin: want}
				}
				var hit Hit
				var e error
				var index *Index
				if indexed {
					index = NewIndex(r)
					hit, e = index.Find(ctx, "claude", inspectionID, parse)
				} else {
					hit, e = FindByID(ctx, DirectWalk(r), "claude", inspectionID, parse)
				}
				if mode == "cancel" {
					if e != context.Canceled || hit != (Hit{}) || calls != 1 {
						t.Fatal("tie cancellation contract changed")
					}
				} else if e != nil || hit != (Hit{Path: first, Root: r.ClaudeProjects[0]}) || calls != 2 {
					t.Fatal("tie fallback Hit/order/callback count changed")
				}
				if h.Origin() != want {
					t.Fatal("shared recovered reducer omitted final result origin")
				}
				if indexed && mode != "cancel" {
					next, fresh := diagnostic.WithHolder(context.Background())
					again, e := index.Find(next, "claude", inspectionID, parse)
					if e != nil || again != hit || calls != 2 || fresh.Origin().Present() {
						t.Fatal("memo replay persisted request-local tie fault or reparsed")
					}
				}
			})
		}
	}
	// No-holder calls retain the exact joined result pair, without wrapping.
	ordinary := errors.New("ordinary")
	sum := Summary{ID: "fixture"}
	got, e := recovered(context.Background(), func(context.Context, string) (Summary, error) { return sum, ordinary }, "fixture")
	if got != sum || e != ordinary {
		t.Fatal("ordinary Summary/error identity changed")
	}
}

type panicOriginListSource struct{ Discovery }

func (panicOriginListSource) Candidates(context.Context, string) ([]Candidate, error) {
	return []Candidate{{Provider: "claude", Path: "invented-list.jsonl"}}, nil
}
func (panicOriginListSource) CodexNames(context.Context) map[string]string { return nil }

func TestPanicOriginJoinedListReducer(t *testing.T) {
	a := panicOriginA(t)
	for _, caught := range []bool{false, true} {
		ctx, h := diagnostic.WithHolder(context.Background())
		calls := 0
		result, err := ListWith(ctx, panicOriginListSource{}, t.TempDir(), func(context.Context, string) (Summary, error) {
			calls++
			if caught {
				panicOriginLeafA()
			}
			return Summary{}, &unreadableError{path: "invented-list.jsonl", origin: a}
		})
		if err != nil || calls != 1 || result.Scanned != 1 || result.Of != 1 || result.Partial || len(result.Rows) != 0 || len(result.Unreadable) != 1 || result.Unreadable[0] != "invented-list.jsonl" || h.Origin() != a {
			t.Fatal("joined ListWith reducer lost caught/returned origin or changed its result")
		}
	}
}

type panicOriginNoValue struct {
	context.Context
	calls atomic.Int64
}

func (c *panicOriginNoValue) Value(any) any {
	c.calls.Add(1)
	panic("inspection queried context Value")
}

func TestPanicOriginInspectionNoNoteAndRecovery(t *testing.T) {
	c := newCache(defaultCacheConfig)
	p := inspectionParse("invented-held", "claude", inspectionID)
	c.held = p
	c.now = panicOriginLeafB
	ctx := &panicOriginNoValue{Context: context.Background()}
	v, s, e := c.InspectConversation(ctx, "claude", inspectionID)
	if !errors.Is(e, ErrInspection) || e.Error() != "transcript: cached inspection failed" || v != (CachedConversation{}) || s != InspectionMissing || ctx.calls.Load() != 0 {
		t.Fatal("inspection panic changed fixed/zero/no-Value contract")
	}
	panicOriginWant(t, PanicOrigin(e), "internal/transcript/panic_leaf_test.go", 10)
	if !c.mu.TryLock() {
		t.Fatal("inspection leaked mutex")
	}
	c.held = nil
	inspectionPut(c, p)
	c.mu.Unlock()
	inspectionCheck(t, c, "claude", inspectionID, CachedConversation{Snapshot: p.snap}, InspectionPresent)
	if ctx.calls.Load() != 0 || len(c.quarantine) != 0 || c.Stats().Parses != 0 {
		t.Fatal("inspection performed diagnostic callbacks/parser/quarantine work")
	}
}
