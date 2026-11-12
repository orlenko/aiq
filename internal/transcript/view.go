package transcript

import (
	"container/list"
	"errors"
	"time"
)

// ErrUnshowable is a view whose build panicked: the snapshot is not shown
// that way again until the file changes.
var ErrUnshowable = errors.New("transcript: could not be shown")

// ViewKey is everything a built view depends on: the snapshot, the window
// (last, window start), markup, the lease it was built with (its id, account
// and pane, joined by the caller), whether anything was held back, and the
// stale state, which for a bare Claude session changes neither the snapshot
// nor the lease.
type ViewKey struct {
	Snap   Snapshot
	Last   int
	Markup bool
	Start  int
	Lease  string
	Held   bool
	Stale  bool
}

// View is one built full window. Value is the builder's; Bytes estimates
// what it holds; Settled says it may answer from a stat (see Settled).
type View struct {
	Value   any
	ETag    string
	Bytes   int64
	Settled bool
}

// maxDeltas is how many delta ETags a view keeps.
const maxDeltas = 8

// viewEntry is a cached view, or for an oversized view the ETag record kept
// once the view itself is dropped (view nil).
type viewEntry struct {
	key     ViewKey
	view    *View
	etag    string
	settled bool
	deltas  []delta // oldest first
}

type delta struct{ fromKey, etag string }

// viewGroup is what only the newest snapshot keeps views of.
type viewGroup struct {
	path   string
	last   int
	markup bool
	start  int
}

type viewShape struct {
	snap   Snapshot
	last   int
	markup bool
}

type heldView struct {
	key  ViewKey
	view *View
	at   time.Time
}

type viewFlight struct {
	done chan struct{}
	view *View
	err  error
}

type viewState struct {
	views     map[ViewKey]*list.Element // *viewEntry
	viewLRU   *list.List
	viewBytes int64
	heldView  *heldView
	broken    map[viewShape]bool
	building  map[ViewKey]*viewFlight
}

func newViewState() viewState {
	return viewState{
		views: map[ViewKey]*list.Element{}, viewLRU: list.New(),
		broken: map[viewShape]bool{}, building: map[ViewKey]*viewFlight{},
	}
}

// View is the view for k: the cached one, the oversized one held for the
// floor, or one build builds now, shared by concurrent callers. A view larger
// than the whole view budget is never admitted: it is held for the floor,
// its bytes counted, and an ETag record of it is kept. A build that panics
// answers ErrUnshowable, now and for the same snapshot, window and markup
// until the file changes.
func (c *Cache) View(k ViewKey, build func() (*View, error)) (*View, error) {
	shape := viewShape{k.Snap, k.Last, k.Markup}
	c.mu.Lock()
	if c.broken[shape] {
		c.mu.Unlock()
		return nil, ErrUnshowable
	}
	if el := c.views[k]; el != nil && el.Value.(*viewEntry).view != nil {
		c.viewLRU.MoveToFront(el)
		c.mu.Unlock()
		return el.Value.(*viewEntry).view, nil
	}
	if h := c.heldViewLocked(); h != nil && h.key == k {
		c.mu.Unlock()
		return h.view, nil
	}
	if f := c.building[k]; f != nil {
		c.mu.Unlock()
		<-f.done
		return f.view, f.err
	}
	f := &viewFlight{done: make(chan struct{})}
	c.building[k] = f
	c.mu.Unlock()

	start := c.now()
	f.view, f.err = buildView(build)
	c.stats.viewBuilds.Add(1)
	c.stats.viewNanos.Add(int64(c.now().Sub(start)))

	c.mu.Lock()
	delete(c.building, k)
	switch {
	case errors.Is(f.err, ErrUnshowable):
		c.broken[shape] = true
	case f.err == nil:
		c.admitViewLocked(k, f.view)
	}
	c.mu.Unlock()
	close(f.done)
	return f.view, f.err
}

func buildView(build func() (*View, error)) (v *View, err error) {
	defer func() {
		if recover() != nil {
			v, err = nil, ErrUnshowable
		}
	}()
	v, err = build()
	if err == nil && v == nil {
		err = ErrUnshowable
	}
	return v, err
}

func (c *Cache) heldViewLocked() *heldView {
	if c.heldView != nil && c.now().Sub(c.heldView.at) >= c.cfg.floor {
		c.heldView = nil
	}
	return c.heldView
}

func (c *Cache) admitViewLocked(k ViewKey, v *View) {
	g := viewGroup{k.Snap.Path, k.Last, k.Markup, k.Start}
	for key, el := range c.views {
		if key.Snap != k.Snap && (viewGroup{key.Snap.Path, key.Last, key.Markup, key.Start}) == g {
			c.removeViewLocked(el)
		}
	}
	e := &viewEntry{key: k, view: v, etag: v.ETag, settled: v.Settled}
	if old := c.views[k]; old != nil {
		if o := old.Value.(*viewEntry); o.etag == v.ETag {
			e.deltas = o.deltas
		}
		c.removeViewLocked(old)
	}
	if v.Bytes > c.cfg.viewBytes {
		c.stats.oversizedViews.Add(1)
		c.heldView = &heldView{key: k, view: v, at: c.now()}
		e.view = nil
	} else {
		c.viewBytes += v.Bytes
	}
	c.views[k] = c.viewLRU.PushFront(e)
	c.evictViewsLocked()
}

func (c *Cache) evictViewsLocked() {
	var held int64
	if h := c.heldViewLocked(); h != nil {
		held = h.view.Bytes
	}
	for c.viewLRU.Len() > c.cfg.viewCount {
		c.removeViewLocked(c.viewLRU.Back())
	}
	for el := c.viewLRU.Back(); el != nil && c.viewBytes+held > c.cfg.viewBytes; {
		prev := el.Prev()
		if el.Value.(*viewEntry).view != nil {
			c.removeViewLocked(el)
		}
		el = prev
	}
}

func (c *Cache) removeViewLocked(el *list.Element) {
	e := el.Value.(*viewEntry)
	if e.view != nil {
		c.viewBytes -= e.view.Bytes
	}
	c.viewLRU.Remove(el)
	delete(c.views, e.key)
}

// dropViewsLocked drops the views and ETag records built from path's
// parses, as its parse leaves the cache.
func (c *Cache) dropViewsLocked(path string) {
	for k, el := range c.views {
		if k.Snap.Path == path {
			c.removeViewLocked(el)
		}
	}
	if c.heldView != nil && c.heldView.key.Snap.Path == path {
		c.heldView = nil
	}
	for s := range c.broken {
		if s.snap.Path == path {
			delete(c.broken, s)
		}
	}
}

// NotModified says a request presenting etag for the view k (fromKey "" for
// the full window, else a delta's from_key) can be answered 304 from a stat
// alone: the view, or the ETag record of an oversized one, is cached under k
// with k's snapshot, it is settled, and etag is the one it was served with.
func (c *Cache) NotModified(k ViewKey, fromKey, etag string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.views[k]
	if el == nil || etag == "" {
		return false
	}
	e := el.Value.(*viewEntry)
	if !e.settled {
		return false
	}
	if fromKey == "" {
		return etag == e.etag
	}
	for _, d := range e.deltas {
		if d.fromKey == fromKey {
			return d.etag == etag
		}
	}
	return false
}

// NoteDelta records the ETag of the delta cut from view k for fromKey, so a
// follower presenting it can be answered from a stat. A view keeps the
// newest few.
func (c *Cache) NoteDelta(k ViewKey, fromKey, etag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.views[k]
	if el == nil {
		return
	}
	e := el.Value.(*viewEntry)
	for i, d := range e.deltas {
		if d.fromKey == fromKey {
			e.deltas = append(e.deltas[:i], e.deltas[i+1:]...)
			break
		}
	}
	e.deltas = append(e.deltas, delta{fromKey, etag})
	if len(e.deltas) > maxDeltas {
		e.deltas = e.deltas[len(e.deltas)-maxDeltas:]
	}
}

// Settled says a view of s may answer from a stat: its last turn is closed,
// no answer is provisional and nothing was held back (heldBack). The fourth
// condition, the lease the view was built with, is in its ViewKey.
func Settled(s *Session, heldBack bool) bool {
	if heldBack {
		return false
	}
	if n := len(s.Turns); n > 0 && s.Turns[n-1].Open {
		return false
	}
	for i := range s.Turns {
		for _, it := range s.Turns[i].Items {
			if it.Provisional {
				return false
			}
		}
	}
	return true
}
