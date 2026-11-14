package transcript

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCachedSessionPeekDoesNoIOOrClockWork(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "invented.jsonl"), "invented")
	s, snap, err := c.Session(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	} // The peek must not stat or reopen it.
	c.now = func() time.Time { panic("peek called clock") }
	c.parse = func(string) (*Session, error) { panic("peek started parse") }
	got, current, ok := c.CachedSession(path)
	if !ok || got != s || current != snap || f.n.Load() != 1 {
		t.Fatal("peek reconstructed or lost the cached incarnation")
	}
	if _, _, ok := c.CachedSession(path + "-missing"); ok || f.n.Load() != 1 {
		t.Fatal("miss started work")
	}
	var absent *Cache
	if _, _, ok := absent.CachedSession(path); ok {
		t.Fatal("nil cache")
	}
}
func TestCachedSessionPeekEvictionAndEqualSnapshotIncarnations(t *testing.T) {
	f := &fakeParse{}
	c, clock := testCache(t, f, func(cfg *cacheConfig) { cfg.parseCount = 1 })
	root := t.TempDir()
	a := writeFile(t, filepath.Join(root, "a.jsonl"), "same")
	b := writeFile(t, filepath.Join(root, "b.jsonl"), "other")
	old, snap, err := c.Session(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	oldIdentity, ok := c.SessionIdentity(a, snap, old)
	if !ok || !oldIdentity.Valid() {
		t.Fatal("original identity missing")
	}
	if _, _, err = c.Session(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.CachedSession(a); ok {
		t.Fatal("evicted session reconstructed")
	}
	clock.Add(3 * time.Second)
	fresh, newSnap, err := c.Session(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if old == fresh || snap != newSnap {
		t.Fatal("fixture did not replace equal-stat parse")
	}
	newIdentity, ok := c.SessionIdentity(a, newSnap, fresh)
	if !ok || !newIdentity.Valid() || newIdentity == oldIdentity {
		t.Fatal("equal-stat ABA reused parse identity")
	}
	if _, ok := c.SessionIdentity(a, snap, old); ok {
		t.Fatal("old parse is still current")
	}
	got, _, ok := c.CachedSession(a)
	if !ok || got != fresh || got == old {
		t.Fatal("old incarnation returned")
	}
}
func TestCachedSessionPeekNeverWaitsOnAnInFlightParse(t *testing.T) {
	f := &fakeParse{started: make(chan string, 1), release: make(chan struct{})}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "pending.jsonl"), "invented")
	done := make(chan error, 1)
	go func() { _, _, err := c.Session(context.Background(), path); done <- err }()
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("parse did not start")
	}
	peek := make(chan bool, 1)
	go func() { _, _, ok := c.CachedSession(path); peek <- ok }()
	select {
	case ok := <-peek:
		if ok {
			t.Fatal("inflight result invented")
		}
	case <-time.After(time.Second):
		close(f.release)
		t.Fatal("peek joined a flight")
	}
	close(f.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.CachedSession(path); !ok {
		t.Fatal("completed parse missing")
	}
}

func TestCachedSessionPeekDoesNotPublishTheTransientHeldParse(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.parseBytes = 1000 })
	root := t.TempDir()
	a := writeFile(t, filepath.Join(root, "oversized-a.jsonl"), "a")
	b := writeFile(t, filepath.Join(root, "oversized-b.jsonl"), "b")
	f.size.Store(a, 8192)
	f.size.Store(b, 8192)
	if _, _, err := c.Session(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	s, _, err := c.Session(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || c.held == nil {
		t.Fatal("fixture did not produce held parse")
	}
	held := c.held
	identity, ok := c.SessionIdentity(b, held.snap, s)
	if !ok || !identity.Valid() {
		t.Fatal("held result has no correlation identity")
	}
	c.now = func() time.Time { panic("peek examined held expiry") }
	if _, _, ok := c.CachedSession(b); ok || c.held != held {
		t.Fatal("peek promoted, expired or returned held parse")
	}
}
