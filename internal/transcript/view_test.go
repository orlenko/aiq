package transcript

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// viewBuilder builds views of a fixed size and counts the builds.
type viewBuilder struct {
	n       atomic.Int64
	bytes   int64
	settled bool
}

func (b *viewBuilder) build(etag string) func() (*View, error) {
	return func() (*View, error) {
		b.n.Add(1)
		return &View{Value: etag, ETag: etag, Bytes: b.bytes, Settled: b.settled}, nil
	}
}

func viewFixture(t *testing.T, c *Cache) Snapshot {
	t.Helper()
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	_, snap, err := c.Session(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// Two followers of one session pay one build; two with different from_key
// each keep their own delta ETag, and a delta answers 304 only to its own.
func TestViewTwoFollowersOneBuild(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	k := ViewKey{Snap: viewFixture(t, c), Last: 50, Markup: true}
	b := &viewBuilder{bytes: 100, settled: true}
	var producer *View
	for i := 0; i < 2; i++ {
		v, err := c.View(k, b.build("full"))
		if err != nil || v.ETag != "full" {
			t.Fatal(v, err)
		}
		producer = v
	}
	if b.n.Load() != 1 {
		t.Errorf("%d builds for two followers", b.n.Load())
	}
	c.NoteDelta(k, producer, "k1", "d1")
	c.NoteDelta(k, producer, "k2", "d2")
	switch {
	case !c.NotModified(k, "k1", "d1") || !c.NotModified(k, "k2", "d2"):
		t.Error("a follower's own delta ETag did not answer from a stat")
	case c.NotModified(k, "k1", "d2") || c.NotModified(k, "k2", "d1"):
		t.Error("a follower was answered with another follower's delta")
	case !c.NotModified(k, "", "full") || c.NotModified(k, "", "d1"):
		t.Error("the full window's ETag was not the one compared")
	}
	for i := 0; i < maxDeltas+2; i++ {
		c.NoteDelta(k, producer, fmt.Sprint("n", i), "x")
	}
	if c.NotModified(k, "k1", "d1") {
		t.Errorf("more than %d delta ETags kept", maxDeltas)
	}
}

// The stale flip of a bare Claude session, which changes no file, builds a
// new view through the cache key.
func TestViewStaleFlipRebuilds(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	k := ViewKey{Snap: viewFixture(t, c), Last: 50}
	b := &viewBuilder{bytes: 100}
	c.View(k, b.build("live"))
	k.Stale = true
	if v, _ := c.View(k, b.build("stale")); v.ETag != "stale" || b.n.Load() != 2 {
		t.Errorf("stale flip gave %q after %d builds", v.ETag, b.n.Load())
	}
}

// An unsettled view, such as one whose last turn is open, never answers from
// a stat, whatever its ETag.
func TestViewOpenTurnNeverAnswersFromAStat(t *testing.T) {
	c, _ := testCache(t, &fakeParse{open: true}, nil)
	snap := viewFixture(t, c)
	s, _, _ := c.Session(context.Background(), snap.Path)
	if Settled(s, false) {
		t.Fatal("an open last turn counted as settled")
	}
	k := ViewKey{Snap: snap, Last: 50}
	c.View(k, func() (*View, error) { return &View{ETag: "e", Bytes: 1, Settled: Settled(s, false)}, nil })
	if c.NotModified(k, "", "e") {
		t.Error("an open turn answered 304 from a stat")
	}
	closed := &Session{Turns: []Turn{{Items: []TurnItem{{Kind: ItemAnswer, Provisional: true}}}}}
	if Settled(closed, false) || Settled(&Session{}, true) || !Settled(&Session{Turns: []Turn{{}}}, false) {
		t.Error("Settled misreads a provisional answer, a held-back reply or a closed turn")
	}
}

// An oversized view is held for the floor and never admitted; its ETag
// record answers 304 from a stat once settled, never while unsettled, and
// records stay within the count bound.
func TestViewOversizedHeldWithItsRecord(t *testing.T) {
	c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewBytes, cfg.viewCount = 1000, 3 })
	snap := viewFixture(t, c)
	unsettled := ViewKey{Snap: snap, Last: 10000}
	b := &viewBuilder{bytes: 5000}
	c.View(unsettled, b.build("u"))
	c.View(unsettled, b.build("u"))
	if b.n.Load() != 1 {
		t.Errorf("%d builds inside the floor", b.n.Load())
	}
	if st := c.Stats(); st.ViewBytes != 0 || st.OversizedViews != 1 {
		t.Errorf("oversized view admitted: %+v", st)
	}
	if c.NotModified(unsettled, "", "u") {
		t.Error("an oversized, unsettled view answered 304 from a stat")
	}
	clk.Add(2 * time.Second)
	c.View(unsettled, b.build("u"))
	if b.n.Load() != 2 || c.Stats().OversizedViews != 2 {
		t.Errorf("after the floor: %d builds, %d oversized", b.n.Load(), c.Stats().OversizedViews)
	}

	settled := ViewKey{Snap: snap, Last: 10000, Markup: true}
	sb := &viewBuilder{bytes: 5000, settled: true}
	producer, err := c.View(settled, sb.build("s"))
	if err != nil {
		t.Fatal(err)
	}
	c.NoteDelta(settled, producer, "k", "ds")
	clk.Add(2 * time.Second)
	if !c.NotModified(settled, "", "s") || !c.NotModified(settled, "k", "ds") {
		t.Error("a settled oversized view did not answer from its ETag record")
	}
	if sb.n.Load() != 1 {
		t.Errorf("the record's 304 rebuilt the view (%d builds)", sb.n.Load())
	}
	for i := 0; i < 5; i++ {
		c.View(ViewKey{Snap: snap, Last: i}, sb.build("o"))
	}
	if st := c.Stats(); st.Views > 3 {
		t.Errorf("%d views and records under a bound of 3", st.Views)
	}
}

// A build that panics answers "could not be shown" and is not built again
// for that snapshot, window and markup; another markup still builds.
func TestViewRenderPanicQuarantined(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	snap := viewFixture(t, c)
	var n atomic.Int64
	boom := func() (*View, error) { n.Add(1); panic("renderer bug") }
	for i := 0; i < 2; i++ {
		if _, err := c.View(ViewKey{Snap: snap, Last: 50, Lease: fmt.Sprint(i)}, boom); !errors.Is(err, ErrUnshowable) {
			t.Fatal(err)
		}
	}
	if n.Load() != 1 {
		t.Errorf("%d builds of a quarantined view", n.Load())
	}
	b := &viewBuilder{bytes: 1}
	if _, err := c.View(ViewKey{Snap: snap, Last: 50, Markup: true}, b.build("m")); err != nil {
		t.Error(err)
	}
}

// Following a growing session keeps the view cache inside its bounds and
// only the newest snapshot's views per window.
func TestViewCacheBoundedUnderFollow(t *testing.T) {
	c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.viewBytes, cfg.viewCount = 2000, 8 })
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	b := &viewBuilder{bytes: 300, settled: true}
	for i := 0; i < 200; i++ {
		appendFile(t, path, "y")
		clk.Add(2 * time.Second)
		_, snap, err := c.Session(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		for _, last := range []int{50, 10000} {
			c.View(ViewKey{Snap: snap, Last: last}, b.build(fmt.Sprint(i)))
		}
		st := c.Stats()
		if st.Views > 8 || st.ViewBytes > 2000 {
			t.Fatalf("poll %d: %d views, %d bytes", i, st.Views, st.ViewBytes)
		}
		if st.Views > 2 {
			t.Fatalf("poll %d: %d views of one session's two windows", i, st.Views)
		}
	}
}
