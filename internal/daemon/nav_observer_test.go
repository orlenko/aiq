package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
)

type navObsOp struct {
	seq   int
	kind  string
	delay time.Duration
	wake  int
}
type navObsPause struct{ entered, release chan struct{} }

// This bounded mailbox models Stop/Reset invalidating unread ticks. It is not
// a claim that Go's native timer channel has buffered capacity.
type navObsDriver struct {
	mu                                     sync.Mutex
	at                                     time.Time
	fail                                   bool
	pause                                  *navObsPause
	clockCheck                             bool
	badLock, badDelay                      bool
	armed                                  bool
	deadline                               time.Time
	factories, resets, stops, seq, dropped int
	ticks                                  chan time.Time
	ops                                    chan navObsOp
	observer                               atomic.Pointer[navObserver]
}

func navObsNewDriver() *navObsDriver {
	return &navObsDriver{at: time.Unix(1800000000, 0), ticks: make(chan time.Time, 1), ops: make(chan navObsOp, 8192)}
}
func (d *navObsDriver) record(kind string, delay time.Duration) {
	d.seq++
	wake := -1
	if o := d.observer.Load(); o != nil {
		wake = len(o.wake)
	}
	select {
	case d.ops <- navObsOp{d.seq, kind, delay, wake}:
	default:
		d.dropped++
	}
}
func (d *navObsDriver) factory() navObservationTimer {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.factories++
	return d
}
func (d *navObsDriver) Channel() <-chan time.Time {
	d.mu.Lock()
	d.record("park", 0)
	d.mu.Unlock()
	return d.ticks
}
func (d *navObsDriver) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.armed = false
	d.stops++
	select {
	case <-d.ticks:
	default:
	}
	d.record("stop", 0)
}
func (d *navObsDriver) Reset(delay time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if delay <= 0 {
		d.badDelay = true
	}
	select {
	case <-d.ticks:
	default:
	}
	d.armed = true
	d.deadline = d.at.Add(delay)
	d.resets++
	d.record("reset", delay)
}
func (d *navObsDriver) now() time.Time {
	d.mu.Lock()
	at, fail, pause, check := d.at, d.fail, d.pause, d.clockCheck
	d.pause = nil
	d.mu.Unlock()
	if check {
		if o := d.observer.Load(); o != nil {
			if !o.mu.TryLock() {
				d.mu.Lock()
				d.badLock = true
				d.mu.Unlock()
			} else {
				o.mu.Unlock()
			}
		}
	}
	if pause != nil {
		close(pause.entered)
		<-pause.release
	}
	if fail {
		panic("invented clock fault")
	}
	return at
}
func (d *navObsDriver) set(at time.Time) { d.mu.Lock(); d.at = at; d.mu.Unlock() }
func (d *navObsDriver) fault(fail bool)  { d.mu.Lock(); d.fail = fail; d.mu.Unlock() }
func (d *navObsDriver) mark() int        { d.mu.Lock(); defer d.mu.Unlock(); return d.seq }
func (d *navObsDriver) fire(at time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		return false
	}
	d.at = at
	d.armed = false
	select {
	case d.ticks <- at:
		d.record("fire", 0)
		return true
	default:
		return false
	}
}
func navObsAwait(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout: " + label)
	}
}
func (d *navObsDriver) wait(t *testing.T, after int, kind string) navObsOp {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case e := <-d.ops:
			if e.seq > after && e.kind == kind && (kind != "park" || e.wake == 0) {
				return e
			}
		case <-timer.C:
			d.mu.Lock()
			seq, f, r, s := d.seq, d.factories, d.resets, d.stops
			d.mu.Unlock()
			t.Fatalf("no %s progress: after=%d seq=%d factories=%d resets=%d stops=%d", kind, after, seq, f, r, s)
		}
	}
}
func (d *navObsDriver) settle(t *testing.T, o *navObserver) {
	t.Helper()
	d.mu.Lock()
	after := d.seq
	o.wakeWorker()
	d.mu.Unlock()
	d.wait(t, after, "park")
}

type navObsSink struct {
	mu          sync.Mutex
	lines       []string
	behavior    func(string)
	active, max atomic.Int64
	entered     chan string
	overflow    bool
}

func navObsNewSink() *navObsSink { return &navObsSink{entered: make(chan string, 128)} }
func (s *navObsSink) write(format string, args ...any) {
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for m := s.max.Load(); n > m && !s.max.CompareAndSwap(m, n); m = s.max.Load() {
	}
	line := fmt.Sprintf(format, args...)
	s.mu.Lock()
	if len(s.lines) < 128 {
		s.lines = append(s.lines, line)
	} else {
		s.overflow = true
	}
	fn := s.behavior
	s.mu.Unlock()
	select {
	case s.entered <- line:
	default:
	}
	if fn != nil {
		fn(line)
	}
}
func (s *navObsSink) on(fn func(string)) { s.mu.Lock(); s.behavior = fn; s.mu.Unlock() }
func (s *navObsSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}
func (s *navObsSink) line(t *testing.T, want string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-s.entered:
			if line == want {
				return
			}
		case <-timer.C:
			t.Fatalf("sink did not receive %q", want)
		}
	}
}

type navObsState struct {
	pending                    uint64
	overflow, busy, closed     bool
	normal                     int
	pendingAt, summaryAt, high navObservationStamp
	keys                       []navObservationStamp
	catalog, mapSize           int
	idle, done                 chan struct{}
}

func navObsRaw(o *navObserver) navObsState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return navObsState{o.pending, o.overflow, o.busy, o.closed, o.normalCount, o.pendingAt, o.summaryAt, o.high, append([]navObservationStamp(nil), o.keyAt...), len(o.catalog), len(o.keys), o.idle, o.done}
}
func navObsKeys() []string {
	var keys []string
	for i := 0; i < 12; i++ {
		keys = append(keys, fmt.Sprintf("key-%02d", i))
	}
	return keys
}
func navObsOrigin(t *testing.T) diagnostic.Origin {
	t.Helper()
	var origin diagnostic.Origin
	func() {
		defer func() {
			if v := recover(); v != nil {
				origin = diagnostic.Capture(v)
			}
		}()
		navObserverLeaf()
	}()
	file, line, known := origin.Location()
	if !known || file != "internal/daemon/nav_observer_leaf_test.go" || line != 3 {
		t.Fatal("known observer leaf was lost")
	}
	return origin
}
func navObsUnknown() diagnostic.Origin {
	var o diagnostic.Origin
	func() {
		defer func() {
			if v := recover(); v != nil {
				o = diagnostic.Capture(v)
			}
		}()
		strings.Repeat("fixture", -1)
	}()
	return o
}
func navObsFixture(t *testing.T) (*navObserver, *navObsDriver, *navObsSink) {
	t.Helper()
	d, s := navObsNewDriver(), navObsNewSink()
	o := newNavObserver(navObsKeys(), s.write, d.now, d.factory)
	d.observer.Store(o)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !o.Close(ctx) {
			t.Error("observer cleanup incomplete after fixture releases")
		}
		d.mu.Lock()
		bad := d.badDelay || d.badLock || d.dropped != 0 || d.factories != 1
		d.mu.Unlock()
		if bad {
			t.Error("driver contract/factory/clock-lock failure")
		}
	})
	d.settle(t, o)
	return o, d, s
}
func navObsStart(t *testing.T, fn func()) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	return done
}
func navObsGate(t *testing.T) (chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	return gate, open
}
func navObsPending(t *testing.T, o *navObserver, want uint64) {
	t.Helper()
	if got := navObsRaw(o).pending; got != want {
		t.Fatalf("pending=%d want=%d", got, want)
	}
}
func navObsFire(t *testing.T, d *navObsDriver, at time.Time) int {
	t.Helper()
	mark := d.mark()
	if !d.fire(at) {
		t.Fatal("expected armed timer before controlled fire")
	}
	return mark
}

func TestNavObserverRollingLedgerAndSummaryFirst(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	keys := navObsKeys()
	start := d.now()
	o.Observe(keys[0], a)
	o.Observe(keys[0], a)
	for _, k := range keys[1:10] {
		o.Observe(k, a)
	}
	for i := 0; i < 3; i++ {
		o.Observe(keys[10], a)
	}
	o.Observe(keys[11], a)
	o.Observe("PRIVATE absent", diagnostic.Origin{})
	st := navObsRaw(o)
	if st.pending != 5 || st.normal != 10 || st.keys[11].valid || st.keys[12].valid || len(s.all()) != 10 {
		t.Fatal("initial global/per-key denial ledger")
	}
	d.set(start.Add(59999 * time.Millisecond))
	o.Observe(keys[10], a)
	navObsPending(t, o, 6)
	d.settle(t, o)
	gate, open := navObsGate(t)
	entered := make(chan struct{})
	s.on(func(line string) {
		if line == "navigator: 6 further panic reports suppressed" {
			close(entered)
			<-gate
		}
	})
	navObsFire(t, d, start.Add(time.Minute))
	navObsAwait(t, entered, "summary reservation")
	o.Observe(keys[10], a)
	st = navObsRaw(o)
	if st.pending != 1 || st.normal != 0 || st.keys[11].valid || !st.busy {
		t.Fatal("busy summary spent denied normal/key credit")
	}
	open()
	d.settle(t, o)
	o.Observe(keys[10], a)
	for _, k := range keys[:9] {
		o.Observe(k, a)
	}
	o.Observe(keys[11], a)
	st = navObsRaw(o)
	if st.normal != 10 || st.pending != 2 || st.keys[12].valid || len(s.all()) != 21 {
		t.Fatal("ten normals plus independent one summary ledger")
	}
	d.settle(t, o)
	mark := navObsFire(t, d, start.Add(2*time.Minute))
	s.line(t, "navigator: 2 further panic reports suppressed")
	d.wait(t, mark, "park")
	if st = navObsRaw(o); st.pending != 0 || st.summaryAt.at != start.Add(2*time.Minute) || s.max.Load() != 1 {
		t.Fatal("summary cooldown/batch reset")
	}
}

func TestNavObserverNormalFirstDueBusyReleaseWake(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := d.now()
	o.Observe("key-00", a)
	o.Observe("key-00", a)
	d.settle(t, o)
	gate, open := navObsGate(t)
	entered := make(chan struct{})
	s.on(func(line string) {
		if strings.Contains(line, `"key-01"`) {
			close(entered)
			<-gate
		}
	})
	d.set(start.Add(time.Minute))
	done := navObsStart(t, func() { o.Observe("key-01", a) })
	t.Cleanup(func() { open(); navObsAwait(t, done, "normal caller cleanup") })
	navObsAwait(t, entered, "blocked normal")
	o.Observe("key-02", a)
	d.settle(t, o)
	st := navObsRaw(o)
	d.mu.Lock()
	armed := d.armed
	d.mu.Unlock()
	if st.pending != 2 || st.summaryAt.valid || armed || st.keys[3].valid {
		t.Fatal("due busy summary took K/credit or armed a spinning timer")
	}
	open()
	navObsAwait(t, done, "normal release")
	s.line(t, "navigator: 2 further panic reports suppressed")
	d.settle(t, o)
	if navObsRaw(o).pending != 0 || s.max.Load() != 1 {
		t.Fatal("release wake lost due summary or overlapped sink")
	}
}

func TestNavObserverSummaryReleaseWakeBeforeWaitAndNextBatch(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := d.now()
	o.Observe("key-00", a)
	o.Observe("key-00", a)
	d.settle(t, o)
	gate, open := navObsGate(t)
	entered := make(chan struct{})
	var first atomic.Bool
	s.on(func(line string) {
		if strings.Contains(line, "further panic reports") && first.CompareAndSwap(false, true) {
			close(entered)
			<-gate
		}
	})
	navObsFire(t, d, start.Add(time.Minute))
	navObsAwait(t, entered, "blocked first summary")
	o.Observe("key-01", a)
	d.set(start.Add(2 * time.Minute))
	o.Observe("key-02", a)
	st := navObsRaw(o)
	if st.pending != 2 || !st.busy || st.keys[2].valid || st.keys[3].valid || s.active.Load() != 1 {
		t.Fatal("summary did not share the single permit/new batch")
	}
	// Worker itself releases this emission before its next Stop/select. Its
	// buffered release wake MUST survive without another tick or request.
	open()
	s.line(t, "navigator: 2 further panic reports suppressed")
	d.settle(t, o)
	if s.max.Load() != 1 || navObsRaw(o).pending != 0 || len(s.all()) != 3 {
		t.Fatal("pre-wait release wake lost/duplicated next batch")
	}
}

func TestNavObserverTimerRequestConservation(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := d.now()
	o.Observe("key-00", a)
	o.Observe("key-00", a)
	d.settle(t, o)
	d.set(start.Add(time.Minute))
	startRace := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-startRace; d.fire(start.Add(time.Minute)) }()
	go func() { defer wg.Done(); <-startRace; o.Observe("key-01", a) }()
	close(startRace)
	wg.Wait()
	d.settle(t, o)
	normal, taken := 0, uint64(0)
	summaries := 0
	for _, line := range s.all() {
		if strings.Contains(line, "further panic reports") {
			var n uint64
			if _, e := fmt.Sscanf(line, "navigator: %d further panic reports suppressed", &n); e != nil {
				t.Fatal(e)
			}
			taken += n
			summaries++
		} else {
			normal++
		}
	}
	st := navObsRaw(o)
	if uint64(normal)+taken+st.pending != 3 || summaries != 1 || st.normal > 10 || s.max.Load() != 1 {
		t.Fatal("timer/request duplicate reservation or lost report")
	}
}

func TestNavObserverClockFaultsSameTimerAndUnknownTime(t *testing.T) {
	for _, unknownTime := range []bool{false, true} {
		t.Run(fmt.Sprint(unknownTime), func(t *testing.T) {
			o, d, s := navObsFixture(t)
			a := navObsOrigin(t)
			start := d.now()
			if unknownTime {
				d.fault(true)
				o.Observe("key-00", a)
				e := d.wait(t, 0, "reset")
				if e.delay != time.Minute {
					t.Fatal("initial clock fault retry is not positive60s")
				}
				d.wait(t, e.seq, "park")
				if navObsRaw(o).pendingAt.valid {
					t.Fatal("failed initial read invented time")
				}
			} else {
				o.Observe("key-00", a)
				o.Observe("key-00", a)
				d.settle(t, o)
				d.fault(true)
			}
			for i := 1; i <= 2; i++ {
				mark := navObsFire(t, d, start.Add(time.Duration(i)*time.Minute))
				e := d.wait(t, mark, "reset")
				if e.delay != time.Minute {
					t.Fatal("clock fault did not reuse positive60s retry")
				}
				d.wait(t, e.seq, "park")
				navObsPending(t, o, 1)
			}
			d.fault(false)
			mark := navObsFire(t, d, start.Add(3*time.Minute))
			if unknownTime {
				e := d.wait(t, mark, "reset")
				if e.delay != time.Minute || navObsRaw(o).pendingAt.at != start.Add(3*time.Minute) || len(s.all()) != 0 {
					t.Fatal("first healthy read did not establish unknown pending time")
				}
				d.wait(t, e.seq, "park")
				mark = navObsFire(t, d, start.Add(4*time.Minute))
			}
			s.line(t, "navigator: 1 further panic reports suppressed")
			d.wait(t, mark, "park")
			navObsPending(t, o, 0)
			d.mu.Lock()
			factories := d.factories
			d.mu.Unlock()
			if factories != 1 {
				t.Fatal("clock fault created another timer/worker")
			}
		})
	}
	// Stop requires no healthy clock or retry tick.
	o, d, _ := navObsFixture(t)
	d.fault(true)
	o.Observe("key-00", navObsOrigin(t))
	e := d.wait(t, 0, "reset")
	d.wait(t, e.seq, "park")
	if !o.Close(context.Background()) {
		t.Fatal("clock failure prevented worker stop")
	}
}

func TestNavObserverHighWaterSaturationAndPrivacy(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	u := navObsUnknown()
	if _, _, known := u.Location(); !u.Present() || known {
		t.Fatal("unknown fixture")
	}
	start := d.now()
	o.Observe("PRIVATE raw URI?token=not-a-credential", a)
	o.Observe("key-00", u)
	if got := s.all(); len(got) != 2 || got[0] != `navigator: panic report on "unregistered" at internal/daemon/nav_observer_leaf_test.go:3` || got[1] != `navigator: panic report on "key-00" at unknown` {
		t.Fatal("normal formatter leaked raw key or lost literal origin")
	}
	for i := 0; i < 1000; i++ {
		o.Observe(fmt.Sprintf("PRIVATE/%d?q=secret", i), a)
	}
	st := navObsRaw(o)
	if st.catalog != 13 || st.mapSize != 13 || st.pending != 1000 {
		t.Fatal("unknown keys grew catalog or changed K")
	}
	d.set(start.Add(2 * time.Minute))
	// Drain the now-due batch before choosing the normal-first ordering. A
	// summary may legitimately reserve the shared permit ahead of this request.
	d.settle(t, o)
	s.line(t, "navigator: 1000 further panic reports suppressed")
	o.Observe("key-00", a)
	d.set(start.Add(time.Minute))
	o.Observe("key-00", a)
	st = navObsRaw(o)
	if st.high.at != start.Add(2*time.Minute) || st.keys[1].at != st.high.at {
		t.Fatal("rollback minted key credit")
	}
	// Pin saturation as scalar boundary, not an impossible live event flood.
	o.mu.Lock()
	o.pending = math.MaxUint64 - 1
	o.overflow = false
	o.pendingAt = navObservationStamp{start.Add(2 * time.Minute), true}
	o.mu.Unlock()
	o.Observe("key-00", a)
	if navObsRaw(o).pending != math.MaxUint64 || navObsRaw(o).overflow {
		t.Fatal("exact max count mislabeled overflow")
	}
	o.Observe("key-00", a)
	if !navObsRaw(o).overflow {
		t.Fatal("counter wrapped instead of saturating")
	}
	d.set(start.Add(2 * time.Minute))
	d.settle(t, o)
	mark := navObsFire(t, d, start.Add(3*time.Minute))
	s.line(t, "navigator: at least 18446744073709551615 further panic reports suppressed")
	d.wait(t, mark, "park")
	st = navObsRaw(o)
	if st.pending != 0 || st.overflow || st.pendingAt.valid {
		t.Fatal("taking saturated batch failed to reset state")
	}
	for _, line := range s.all() {
		if strings.Contains(line, "PRIVATE") || strings.Contains(line, "token=") {
			t.Fatal("raw request input entered diagnostic sink")
		}
	}
}

type navObsHostile struct{ calls *atomic.Int64 }

func (h navObsHostile) Error() string          { h.calls.Add(1); panic("Error called") }
func (h navObsHostile) String() string         { h.calls.Add(1); panic("String called") }
func (h navObsHostile) Format(fmt.State, rune) { h.calls.Add(1); panic("Format called") }
func (h navObsHostile) Unwrap() error          { h.calls.Add(1); panic("Unwrap called") }

func TestNavObserverSinkPanicReentryAndBlockedNormalClose(t *testing.T) {
	{
		o, d, s := navObsFixture(t)
		a := navObsOrigin(t)
		var calls atomic.Int64
		s.on(func(string) { panic(navObsHostile{&calls}) })
		o.Observe("key-00", a)
		if calls.Load() != 0 || navObsRaw(o).busy || navObsRaw(o).normal != 1 {
			t.Fatal("sink panic invoked formatter/leaked permit/refunded credit")
		}
		s.on(nil)
		o.Observe("key-00", a)
		navObsPending(t, o, 1)
		d.settle(t, o)
	}
	{
		o, d, s := navObsFixture(t)
		a := navObsOrigin(t)
		var once atomic.Bool
		s.on(func(string) {
			if once.CompareAndSwap(false, true) {
				o.Observe("key-01", a)
			}
		})
		o.Observe("key-00", a)
		if len(s.all()) != 1 || navObsRaw(o).normal != 1 || navObsRaw(o).keys[2].valid || navObsRaw(o).pending != 1 {
			t.Fatal("reentrant sink waited or spent key/global credit")
		}
		d.settle(t, o)
	}
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := d.now()
	gate, open := navObsGate(t)
	entered := make(chan struct{})
	s.on(func(string) { close(entered); <-gate })
	done := navObsStart(t, func() { o.Observe("key-00", a) })
	t.Cleanup(func() { open(); navObsAwait(t, done, "blocked caller cleanup") })
	navObsAwait(t, entered, "blocked normal")
	for i := 1; i <= 48; i++ {
		d.set(start.Add(time.Duration(i) * time.Minute))
		o.Observe(fmt.Sprintf("key-%02d", i%12), a)
	}
	d.settle(t, o)
	st := navObsRaw(o)
	if st.pending != 48 || s.max.Load() != 1 || s.active.Load() != 1 || len(s.all()) != 1 {
		t.Fatal("busy observations queued emitters or lost K")
	}
	for _, stamp := range st.keys[2:] {
		if stamp.valid {
			t.Fatal("busy denial spent a previously unused key credit")
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if o.Close(canceled) {
		t.Fatal("blocked normal reported completed close")
	}
	navObsAwait(t, o.done, "workerDone before normal idle")
	select {
	case <-st.idle:
		t.Fatal("active emission idle closed before sink release")
	default:
	}
	for i := 0; i < 8; i++ {
		if o.Close(canceled) {
			t.Fatal("repeated canceled Close invented join")
		}
	}
	open()
	navObsAwait(t, done, "normal sink release")
	if !o.Close(context.Background()) {
		t.Fatal("close failed after sink release")
	}
	o.Observe("key-01", a)
	if len(s.all()) != 1 {
		t.Fatal("post-close observer emitted")
	}
}

func TestNavObserverBlockedSummaryCloseAndShutdownFlush(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := d.now()
	o.Observe("key-00", a)
	o.Observe("key-00", a)
	d.settle(t, o)
	gate, open := navObsGate(t)
	entered := make(chan struct{})
	s.on(func(line string) {
		if strings.Contains(line, "further panic reports") {
			close(entered)
			<-gate
		}
	})
	navObsFire(t, d, start.Add(time.Minute))
	navObsAwait(t, entered, "blocked summary")
	o.Observe("key-01", a)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if o.Close(canceled) {
		t.Fatal("blocked summary claimed worker join")
	}
	select {
	case <-o.done:
		t.Fatal("workerDone closed inside blocked summary")
	default:
	}
	open()
	if !o.Close(context.Background()) {
		t.Fatal("released summary did not join")
	}
	if len(s.all()) != 2 || s.max.Load() != 1 {
		t.Fatal("shutdown forced not-yet-due next batch")
	}
	for _, due := range []bool{false, true} {
		t.Run(fmt.Sprint(due), func(t *testing.T) {
			x, driver, sink := navObsFixture(t)
			epoch := driver.now()
			x.Observe("key-00", a)
			x.Observe("key-00", a)
			driver.settle(t, x)
			delta := 59999 * time.Millisecond
			if due {
				delta = time.Minute
			}
			driver.set(epoch.Add(delta))
			if !x.Close(context.Background()) {
				t.Fatal("shutdown close")
			}
			want := 1
			if due {
				want = 2
			}
			if len(sink.all()) != want {
				t.Fatal("shutdown failed exactly-one-already-due rule")
			}
		})
	}
}

func TestNavObserverClockPausedCloseRevalidates(t *testing.T) {
	o, d, s := navObsFixture(t)
	p := &navObsPause{entered: make(chan struct{}), release: make(chan struct{})}
	d.mu.Lock()
	d.pause = p
	d.clockCheck = true
	d.mu.Unlock()
	var once sync.Once
	open := func() { once.Do(func() { close(p.release) }) }
	t.Cleanup(open)
	a := navObsOrigin(t)
	done := navObsStart(t, func() { o.Observe("key-00", a) })
	t.Cleanup(func() { open(); navObsAwait(t, done, "paused clock caller cleanup") })
	navObsAwait(t, p.entered, "clock read outside mutex")
	// The caller's lock check already ran before entering the pause. A later
	// worker clock may overlap the OTHER goroutine's Close lock legitimately.
	d.mu.Lock()
	d.clockCheck = false
	d.mu.Unlock()
	if !o.Close(context.Background()) {
		t.Fatal("close while outside-lock clock paused")
	}
	open()
	navObsAwait(t, done, "paused read return")
	st := navObsRaw(o)
	if !st.closed || st.normal != 0 || st.pending != 0 || len(s.all()) != 0 {
		t.Fatal("stale clock read reserved after close")
	}
}

func TestNavObserverOwnerFreezeCloseAndAcquireRace(t *testing.T) {
	a := navObsOrigin(t)
	var owner navObserverOwner
	d, s := navObsNewDriver(), navObsNewSink()
	keys := []string{"frozen"}
	o := owner.acquire(keys, s.write, d.now, d.factory)
	d.observer.Store(o)
	d.settle(t, o)
	t.Cleanup(func() { owner.Close(context.Background()) })
	keys[0] = "PRIVATE changed"
	other := navObsNewSink()
	otherDriver := navObsNewDriver()
	again := owner.acquire([]string{"different"}, other.write, otherDriver.now, otherDriver.factory)
	if again != o {
		t.Fatal("owner reconstructed/reset observer")
	}
	o.Observe("frozen", a)
	again.Observe("frozen", a)
	if navObsRaw(o).pending != 1 || len(s.all()) != 1 || len(other.all()) != 0 || navObsRaw(o).catalog != 2 {
		t.Fatal("catalog/logger/credits did not freeze")
	}
	if s.all()[0] != `navigator: panic report on "frozen" at internal/daemon/nav_observer_leaf_test.go:3` {
		t.Fatal("mutating caller catalog changed a frozen log identity")
	}
	if !owner.Close(context.Background()) || owner.acquire(keys, s.write, d.now, d.factory) != nil {
		t.Fatal("final owner close resurrected observer")
	}
	var empty navObserverOwner
	unused := navObsNewDriver()
	if !empty.Close(context.Background()) || empty.acquire(keys, s.write, unused.now, unused.factory) != nil {
		t.Fatal("pre-acquire close failed")
	}
	unused.mu.Lock()
	made := unused.factories
	unused.mu.Unlock()
	if made != 0 {
		t.Fatal("preclosed owner constructed timer")
	}
	var disabled *navObserver
	disabled.Observe("key", a)
	if !disabled.Close(context.Background()) {
		t.Fatal("nil disabled observer not safe")
	}
	for round := 0; round < 8; round++ {
		var own navObserverOwner
		driver := navObsNewDriver()
		sink := navObsNewSink()
		start := make(chan struct{})
		results := make(chan *navObserver, 16)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results <- own.acquire(navObsKeys(), sink.write, driver.now, driver.factory)
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); <-start; own.Close(context.Background()) }()
		close(start)
		wg.Wait()
		close(results)
		var first *navObserver
		for got := range results {
			if got != nil {
				if first != nil && first != got {
					t.Fatal("acquire race built two observers")
				}
				first = got
			}
		}
		if !own.Close(context.Background()) || own.acquire(keys, s.write, driver.now, driver.factory) != nil {
			t.Fatal("acquire/close race restarted credits")
		}
		driver.mu.Lock()
		count := driver.factories
		driver.mu.Unlock()
		want := 0
		if first != nil {
			want = 1
		}
		if count != want {
			t.Fatal("acquire/close constructor count")
		}
	}
}

func TestNavObserverNativeTimerAdapterConsumedStopReset(t *testing.T) {
	timer := navRealObservationTimer{time.NewTimer(time.Hour)}
	defer timer.Stop()
	timer.Stop()
	timer.Reset(0)
	select {
	case <-timer.Channel():
	case <-time.After(3 * time.Second):
		t.Fatal("native adapter expiration missing")
	}
	// False native Stop after an already consumed tick must not block draining.
	timer.Stop()
	timer.Reset(time.Hour)
	timer.Reset(time.Hour)
	timer.Stop()
}
