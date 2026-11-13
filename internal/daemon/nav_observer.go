package daemon

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
)

const navObservationPeriod = time.Minute
const navObservationFallback = "unregistered"

// Drivers are instance-owned. Timer methods are nonblocking, do not panic and
// Stop drains an unread tick. Only run accesses the one driver it creates.
type navObservationTimer interface {
	Channel() <-chan time.Time
	Reset(time.Duration)
	Stop()
}
type navRealObservationTimer struct{ *time.Timer }

func (t navRealObservationTimer) Channel() <-chan time.Time { return t.C }
func (t navRealObservationTimer) Stop() {
	if !t.Timer.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}
func (t navRealObservationTimer) Reset(d time.Duration) { t.Timer.Reset(d) }

type navObservationStamp struct {
	at    time.Time
	valid bool
}
type navEmission struct {
	key               string
	origin            diagnostic.Origin
	count             uint64
	summary, overflow bool
}

// All retained keys come from the frozen catalog, never requests. One emission
// can be outstanding; a blocked sink is deliberately not made interruptible.
type navObserver struct {
	mu               sync.Mutex
	catalog          []string
	keys             map[string]int
	keyAt            []navObservationStamp
	normalAt         [10]time.Time
	normalCount      int
	summaryAt        navObservationStamp
	pending          uint64
	overflow         bool
	pendingAt        navObservationStamp
	high             navObservationStamp
	busy, closed     bool
	idle             chan struct{}
	stop, done, wake chan struct{}
	now              func() time.Time
	sink             func(string, ...any)
}

func newNavObserver(keys []string, sink func(string, ...any), now func() time.Time, timer func() navObservationTimer) *navObserver {
	if now == nil {
		now = time.Now
	}
	if sink == nil {
		sink = func(string, ...any) {}
	}
	if timer == nil {
		timer = func() navObservationTimer { return navRealObservationTimer{time.NewTimer(time.Hour)} }
	}
	o := &navObserver{keys: map[string]int{}, idle: make(chan struct{}), stop: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1), now: now, sink: sink}
	for _, key := range append([]string{navObservationFallback}, keys...) {
		if _, exists := o.keys[key]; exists {
			continue
		}
		key = strings.Clone(key)
		o.keys[key] = len(o.catalog)
		o.catalog = append(o.catalog, key)
	}
	o.keyAt = make([]navObservationStamp, len(o.catalog))
	close(o.idle)
	go o.run(timer)
	return o
}

func (o *navObserver) wakeWorker() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}
func (o *navObserver) readClock() (at time.Time, ok bool) {
	defer func() {
		if recover() != nil {
			at, ok = time.Time{}, false
		}
	}()
	return o.now(), true
}
func (o *navObserver) clampLocked(at time.Time) time.Time {
	if o.high.valid && at.Before(o.high.at) {
		return o.high.at
	}
	o.high = navObservationStamp{at, true}
	return at
}
func (o *navObserver) suppressLocked(at time.Time, known bool) {
	if o.pending == math.MaxUint64 {
		o.overflow = true
	} else {
		o.pending++
	}
	if !o.pendingAt.valid && known {
		o.pendingAt = navObservationStamp{at, true}
	}
}
func (o *navObserver) reserveLocked() {
	o.busy = true
	o.idle = make(chan struct{})
}

func (o *navObserver) Observe(key string, origin diagnostic.Origin) {
	if o == nil || !origin.Present() {
		return
	}
	o.mu.Lock()
	closed := o.closed
	o.mu.Unlock()
	if closed {
		return
	}
	at, clockOK := o.readClock()
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	if !clockOK {
		o.suppressLocked(time.Time{}, false)
		o.mu.Unlock()
		o.wakeWorker()
		return
	}
	at = o.clampLocked(at)
	if o.pending != 0 && !o.pendingAt.valid {
		o.pendingAt = navObservationStamp{at, true}
	}
	n := 0
	for _, previous := range o.normalAt[:o.normalCount] {
		if at.Sub(previous) < navObservationPeriod {
			o.normalAt[n] = previous
			n++
		}
	}
	o.normalCount = n
	index := o.keys[key] // absent keys collapse to the catalog's first entry
	last := o.keyAt[index]
	if o.busy || o.normalCount == len(o.normalAt) || last.valid && at.Sub(last.at) < navObservationPeriod {
		o.suppressLocked(at, true)
		o.mu.Unlock()
		o.wakeWorker()
		return
	}
	o.keyAt[index] = navObservationStamp{at, true}
	o.normalAt[o.normalCount] = at
	o.normalCount++
	o.reserveLocked()
	record := navEmission{key: o.catalog[index], origin: origin}
	o.mu.Unlock()
	o.emit(record)
}

// The release guard is installed before formatting or invoking the sink.
func (o *navObserver) emit(record navEmission) {
	defer func() {
		o.mu.Lock()
		o.busy = false
		close(o.idle)
		o.mu.Unlock()
		o.wakeWorker()
	}()
	defer func() { recover() }()
	if record.summary {
		if record.overflow {
			o.sink("navigator: at least %d further panic reports suppressed", record.count)
		} else {
			o.sink("navigator: %d further panic reports suppressed", record.count)
		}
		return
	}
	if file, line, known := record.origin.Location(); known {
		o.sink("navigator: panic report on %q at %s:%d", record.key, file, line)
	} else {
		o.sink("navigator: panic report on %q at unknown", record.key)
	}
}

func (o *navObserver) summaryPlan(at time.Time, shutdown bool) (record navEmission, emit bool, delay time.Duration, arm bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed && !shutdown || o.pending == 0 {
		return
	}
	at = o.clampLocked(at)
	if !o.pendingAt.valid {
		o.pendingAt = navObservationStamp{at, true}
	}
	if o.busy {
		return
	}
	due := o.pendingAt.at.Add(navObservationPeriod)
	if o.summaryAt.valid && o.summaryAt.at.Add(navObservationPeriod).After(due) {
		due = o.summaryAt.at.Add(navObservationPeriod)
	}
	if at.Before(due) {
		return navEmission{}, false, due.Sub(at), true
	}
	record = navEmission{count: o.pending, summary: true, overflow: o.overflow}
	o.pending, o.overflow, o.pendingAt = 0, false, navObservationStamp{}
	o.summaryAt = navObservationStamp{at, true}
	o.reserveLocked()
	return record, true, 0, false
}
func (o *navObserver) step() (delay time.Duration, arm bool) {
	// A failed clock/step does not erase the pending batch or stop the worker.
	defer func() {
		if recover() != nil {
			delay, arm = navObservationPeriod, true
		}
	}()
	o.mu.Lock()
	work := !o.closed && o.pending != 0 && !o.busy
	o.mu.Unlock()
	if !work {
		return
	}
	at := o.now() // outside all owned locks; this iteration catches a clock fault
	record, emit, delay, arm := o.summaryPlan(at, false)
	if emit {
		o.emit(record)
	}
	return delay, arm
}
func (o *navObserver) finish() {
	defer func() { recover() }()
	if at, ok := o.readClock(); ok {
		if record, emit, _, _ := o.summaryPlan(at, true); emit {
			o.emit(record)
		}
	}
	o.mu.Lock()
	o.pending, o.overflow, o.pendingAt = 0, false, navObservationStamp{}
	o.mu.Unlock()
}
func (o *navObserver) run(factory func() navObservationTimer) {
	defer close(o.done)
	timer := factory()
	defer timer.Stop()
	for {
		delay, arm := o.step()
		timer.Stop()
		if arm {
			timer.Reset(delay)
		}
		select {
		case <-o.stop:
			o.finish()
			return
		case <-o.wake:
		case <-timer.Channel():
		}
	}
}

// Close returns true only after the worker AND any admitted sink returned.
// Cancellation does not interrupt a sink, join it or create a waiter goroutine.
func (o *navObserver) Close(ctx context.Context) bool {
	if o == nil {
		return true
	}
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.stop)
	}
	o.mu.Unlock()
	select {
	case <-o.done:
	case <-ctx.Done():
		return false
	}
	o.mu.Lock()
	idle := o.idle
	o.mu.Unlock()
	select {
	case <-idle:
		return true
	case <-ctx.Done():
		return false
	}
}

// No production acquisition or shutdown is wired in this component increment.
// Final close and first acquisition linearize here; retirement is not an owner.
type navObserverOwner struct {
	mu       sync.Mutex
	observer *navObserver
	closed   bool
}

func (o *navObserverOwner) acquire(keys []string, sink func(string, ...any), now func() time.Time, timer func() navObservationTimer) *navObserver {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	if o.observer == nil {
		o.observer = newNavObserver(keys, sink, now, timer)
	}
	return o.observer
}
func (o *navObserverOwner) Close(ctx context.Context) bool {
	o.mu.Lock()
	o.closed = true
	observer := o.observer
	o.mu.Unlock()
	return observer.Close(ctx)
}
