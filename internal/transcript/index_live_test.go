package transcript

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveIndexWaitsForOneFreshScanAndWaitersCanCancel(t *testing.T) {
	x := NewLiveIndex(Roots{})
	if x.ttl != 2*time.Second || !x.fresh || NewIndex(Roots{}).ttl != DiscoveryTTL || NewIndex(Roots{}).fresh {
		t.Fatal("live policy changed legacy discovery")
	}
	s := scan{data: "old", at: time.Now().Add(-3 * time.Second), gen: 1}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	run := func(any) any { calls.Add(1); close(started); <-release; return "new" }
	type result struct {
		data any
		err  error
	}
	done := make(chan result, 1)
	go func() { d, err := x.read(context.Background(), &s, run); done <- result{d, err} }()
	<-started
	select {
	case <-done:
		t.Fatal("live lookup answered stale data during refresh")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := x.read(ctx, &s, run); err != context.Canceled {
		t.Fatal("fresh scan waiter ignored cancellation")
	}
	close(release)
	select {
	case out := <-done:
		if out.err != nil || out.data != "new" {
			t.Fatal("fresh publication not returned", out)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh reader did not join")
	}
	if calls.Load() != 1 {
		t.Fatal("waiter duplicated provider scan")
	}
	if d, err := x.read(context.Background(), &s, run); err != nil || d != "new" || calls.Load() != 1 {
		t.Fatal("live index did not reuse completed scan")
	}
}

func TestLiveIndexFindsNewerCodexCopyOnFirstExpiredRead(t *testing.T) {
	r, _ := findFixture(t)
	x := NewLiveIndex(r)
	clk := newClock()
	x.now = clk.Now
	cache := NewCache()
	name := "rollout-2026-09-15T21-08-05-" + idA + ".jsonl"
	first := filepath.Join(r.CodexSessions[0], "2026", "09", "15", name)
	writeLines(t, first, codexFixture("/w", idA)...)
	setMtime(t, first, t0)
	if hit, err := x.Find(context.Background(), "codex", idA, cache.Summary); err != nil || hit.Path != first {
		t.Fatal("initial copy failed", hit, err)
	}
	newer := filepath.Join(r.CodexSessions[1], "2026", "09", "16", name)
	writeLines(t, newer, codexFixture("/w", idA)...)
	setMtime(t, newer, t0.Add(time.Hour))
	clk.Add(2 * time.Second)
	if hit, err := x.Find(context.Background(), "codex", idA, cache.Summary); err != nil || hit.Path != newer {
		t.Fatal("first expired live read returned old copy", hit, err)
	}
	if x.Walks() != 2 || cache.Stats().Parses != 0 {
		t.Fatal("copy lookup did unexpected work")
	}
}
