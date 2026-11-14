package daemon

import (
	"sync"
	"testing"
	"time"
)

func TestNavObserverNormalClockRecoveryDatesPending(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := d.now()
	d.fault(true)
	o.Observe("key-00", a)
	e := d.wait(t, 0, "reset")
	d.wait(t, e.seq, "park")
	d.set(start.Add(10 * time.Second))
	d.fault(false)
	gate, open := navObsGate(t)
	entered := make(chan struct{})
	s.on(func(line string) {
		if line == `navigator: panic report on "key-01" at internal/daemon/nav_observer_leaf_test.go:3` {
			close(entered)
			<-gate
		}
	})
	done := navObsStart(t, func() { o.Observe("key-01", a) })
	t.Cleanup(func() { open(); navObsAwait(t, done, "recovery normal cleanup") })
	navObsAwait(t, entered, "healthy normal reservation")
	st := navObsRaw(o)
	if st.pending != 1 || !st.pendingAt.valid || st.pendingAt.at != start.Add(10*time.Second) || st.normal != 1 {
		t.Fatal("healthy normal read failed to date existing untimed batch")
	}
	d.set(start.Add(70 * time.Second))
	open()
	navObsAwait(t, done, "normal release")
	s.line(t, "navigator: 1 further panic reports suppressed")
	d.settle(t, o)
	if navObsRaw(o).pending != 0 || s.max.Load() != 1 {
		t.Fatal("due recovered batch was delayed or overlapped")
	}
}

func TestNavObserverConcurrentDistinctKeysConserveReports(t *testing.T) {
	o, d, s := navObsFixture(t)
	a := navObsOrigin(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, key := range navObsKeys() {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; o.Observe(key, a) }()
	}
	close(start)
	wg.Wait()
	d.settle(t, o)
	st := navObsRaw(o)
	if st.normal > 10 || st.normal < 1 || uint64(st.normal)+st.pending != 12 || len(s.all()) != st.normal || s.max.Load() != 1 {
		t.Fatalf("fanout ledger: normal=%d pending=%d reports=%d max=%d", st.normal, st.pending, len(s.all()), s.max.Load())
	}
	credited := 0
	for _, stamp := range st.keys {
		if stamp.valid {
			credited++
		}
	}
	if credited != st.normal {
		t.Fatal("busy/global denial consumed key credit")
	}
}
