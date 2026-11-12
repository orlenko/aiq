package transcript

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"time"
)

// ErrUnshowable is a view that could not be built or published. Renderer
// failures suppress the same snapshot/window/markup until change or LRU eviction;
// internal publication failures are retryable and do not suppress the renderer.
var ErrUnshowable = errors.New("transcript: could not be shown")

// ViewKey is everything a built view depends on: snapshot, window, markup,
// lease identity supplied by the caller, held-back output and stale state.
type ViewKey struct {
	Snap   Snapshot
	Last   int
	Markup bool
	Start  int
	Lease  string
	Held   bool
	Stale  bool
}

// View is one immutable built window. Value belongs to the builder and must
// remain immutable after return; the cache copies the header, not Value.
type View struct {
	Value   any
	ETag    string
	Bytes   int64
	Settled bool
	owner   *viewOwner
	key     ViewKey
}

const maxDeltas = 8

type viewEntry struct {
	key     ViewKey
	view    *View
	etag    string
	settled bool
	deltas  []delta
	owner   *viewOwner
}
type delta struct{ fromKey, etag string }
type viewShape struct {
	snap   Snapshot
	last   int
	markup bool
	start  int
}

func shapeOf(k ViewKey) viewShape { return viewShape{k.Snap, k.Last, k.Markup, k.Start} }

// A nonzero-sized, opaque incarnation token. Parsed identity prevents equal-
// Snapshot ABA; standalone authority changes only at admitted installation.
type viewOwner struct {
	snap   Snapshot
	parsed *parsed
}
type brokenView struct {
	shape viewShape
	owner *viewOwner
}
type heldView struct {
	key  ViewKey
	view *View
	at   time.Time
}

// viewBuildKey distinguishes simultaneous incarnations of one logical view.
// Keep typed references so pointer reuse cannot resurrect a retired identity.
type viewBuildKey struct {
	key    ViewKey
	owner  *viewOwner
	parsed *parsed
}

type viewFlight struct {
	done  chan struct{}
	view  *View
	err   error
	owner *viewOwner
}
type viewState struct {
	views       map[ViewKey]*list.Element
	viewLRU     *list.List
	viewBytes   int64
	heldView    *heldView
	broken      map[viewShape]*list.Element
	brokenLRU   *list.List
	building    map[viewBuildKey]*viewFlight
	viewOwners  map[string]*viewOwner
	viewSem     chan struct{}
	viewChanged chan struct{}
	// Fixture seams run outside the lock before installation, and under it
	// before any fallible publication mutation, respectively.
	beforeViewAdmit   func(ViewKey)
	beforeViewPublish func()
}

func newViewState(width int) viewState {
	return viewState{
		views: map[ViewKey]*list.Element{}, viewLRU: list.New(),
		broken: map[viewShape]*list.Element{}, brokenLRU: list.New(),
		building: map[viewBuildKey]*viewFlight{}, viewOwners: map[string]*viewOwner{},
		viewSem: make(chan struct{}, width), viewChanged: make(chan struct{}),
	}
}

// View is ViewContext with no caller deadline. Builders must be independent:
// they must not synchronously call this Cache's View/ViewContext. Compose
// dependent formatting using pure helpers inside one admitted builder.
func (c *Cache) View(k ViewKey, build func() (*View, error)) (*View, error) {
	return c.ViewContext(context.Background(), k, build)
}

// ViewContext shares an exact-key build in the current parse incarnation, or
// waits cancelably for one of the
// process cache's builder slots. Waiting callers create no queue or owner.
// Admitted work holds its slot through publication even if all callers leave.
func (c *Cache) ViewContext(ctx context.Context, k ViewKey, build func() (*View, error)) (*View, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err, f, changed, found, _ := c.viewRole(ctx, k, build, false)
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		if found {
			return v, err
		}
		if f != nil {
			return waitView(ctx, f)
		}
		select {
		case c.viewSem <- struct{}{}:
		case <-changed:
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if c.beforeViewAdmit != nil {
			c.beforeViewAdmit(k)
		}
		v, err, f, _, found, launched := c.viewRole(ctx, k, build, true)
		if !launched {
			<-c.viewSem
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		if found {
			return v, err
		}
		if f != nil {
			return waitView(ctx, f)
		}
	}
}

// viewRole rechecks under the authoritative lock after borrowing a slot.
// The panic guard is for injected clocks before admission, not renderer blame.
func (c *Cache) viewRole(ctx context.Context, k ViewKey, build func() (*View, error), slot bool) (v *View, err error, f *viewFlight, changed <-chan struct{}, found, launched bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		if recover() != nil {
			v, err, f, found = nil, ErrUnshowable, nil, true
		}
	}()
	if err = ctx.Err(); err != nil {
		found = true
		return
	}
	c.heldLocked()
	if el := c.broken[shapeOf(k)]; el != nil {
		c.brokenLRU.MoveToFront(el)
		err, found = ErrUnshowable, true
		return
	}
	if el := c.views[k]; el != nil && el.Value.(*viewEntry).view != nil {
		c.viewLRU.MoveToFront(el)
		v, found = el.Value.(*viewEntry).view, true
		return
	}
	if h := c.heldViewLocked(); h != nil && h.key == k {
		v, found = h.view, true
		return
	}
	if f = c.joinViewLocked(k, c.viewParseLocked(k.Snap.Path)); f != nil {
		return
	}
	changed = c.viewChanged
	if !slot {
		return
	}
	// Expiry may invalidate old parsed authority. Do it before creating a token.
	c.heldLocked()
	if err = ctx.Err(); err != nil {
		found = true
		return
	}
	authority := c.viewParseLocked(k.Snap.Path)
	if f = c.joinViewLocked(k, authority); f != nil {
		return
	}
	owner := c.viewOwnerLocked(k.Snap)
	k = ownedViewKey(k)
	if owner != nil {
		k.Snap.Path = owner.snap.Path
	}
	f = &viewFlight{done: make(chan struct{}), owner: owner}
	bk := viewBuildKey{key: k, owner: owner, parsed: authority}
	c.building[bk] = f
	c.viewChangedLocked()
	launched = true
	go c.flyView(bk, f, build)
	return
}
func waitView(ctx context.Context, f *viewFlight) (*View, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return f.view, f.err
	}
}
func (c *Cache) viewChangedLocked() { close(c.viewChanged); c.viewChanged = make(chan struct{}) }
func ownedViewKey(k ViewKey) ViewKey {
	k.Snap.Path = strings.Clone(k.Snap.Path)
	k.Lease = strings.Clone(k.Lease)
	return k
}

// Authority observation never changes an LRU or allocates path ownership.
func (c *Cache) viewParseLocked(path string) *parsed {
	if el := c.full[path]; el != nil {
		return el.Value.(*parsed)
	}
	if c.held != nil && c.held.snap.Path == path {
		return c.held
	}
	return nil
}
func (c *Cache) joinViewLocked(k ViewKey, p *parsed) *viewFlight {
	for bk, f := range c.building {
		if bk.key != k || bk.parsed != p {
			continue
		}
		if p != nil && p.snap != k.Snap {
			if bk.owner == nil {
				return f
			}
			continue
		}
		o := bk.owner
		if o != nil && o.parsed == p && o.snap == k.Snap && c.currentViewOwnerLocked(k.Snap.Path, o) {
			return f
		}
	}
	return nil
}

func (c *Cache) viewOwnerLocked(snap Snapshot) *viewOwner {
	p := c.viewParseLocked(snap.Path)
	if p != nil && p.snap != snap {
		return nil
	}
	if o := c.viewOwners[snap.Path]; o != nil && o.snap == snap && o.parsed == p {
		return o
	}
	c.dropViewsLocked(snap.Path)
	snap.Path = strings.Clone(snap.Path)
	o := &viewOwner{snap: snap, parsed: p}
	c.viewOwners[snap.Path] = o
	return o
}
func (c *Cache) currentViewOwnerLocked(path string, o *viewOwner) bool {
	return o != nil && c.viewOwners[path] == o
}

// Cleanup is registered before the first clock read. Renderer classification
// is separate from internal faults, and an ordinary partial/error pair survives
// a later timing fault unchanged.
func (c *Cache) flyView(bk viewBuildKey, f *viewFlight, build func() (*View, error)) {
	var v *View
	var err error
	var rendererFailure, ordinary bool
	defer func() {
		if recover() != nil && !ordinary {
			v, err = nil, ErrUnshowable
			rendererFailure = false
		}
		c.finishView(bk, f, v, err, rendererFailure, ordinary)
	}()
	start := c.now()
	v, err = buildView(build)
	rendererFailure = errors.Is(err, ErrUnshowable)
	ordinary = err != nil && !rendererFailure
	c.stats.viewBuilds.Add(1)
	c.stats.viewNanos.Add(int64(c.now().Sub(start)))
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
	return
}
func (c *Cache) finishView(bk viewBuildKey, f *viewFlight, v *View, err error, rendererFailure, ordinary bool) {
	k := bk.key
	c.mu.Lock()
	defer func() {
		if c.building[bk] == f {
			delete(c.building, bk)
		}
		close(f.done)
		c.viewChangedLocked()
		<-c.viewSem
		c.pruneViewOwnerLocked(k.Snap.Path)
		c.mu.Unlock()
	}()
	defer func() {
		if recover() != nil && !ordinary {
			f.view, f.err = nil, ErrUnshowable
		}
	}()
	f.view, f.err = v, err
	if err == nil {
		copy := *v
		copy.owner, copy.key = f.owner, k
		copy.ETag = strings.Clone(copy.ETag)
		f.view = &copy
	}
	if !c.currentViewOwnerLocked(k.Snap.Path, f.owner) {
		return
	}
	if ordinary {
		return
	}
	if c.beforeViewPublish != nil {
		c.beforeViewPublish()
	}
	switch {
	case rendererFailure:
		c.breakViewLocked(shapeOf(k), f.owner)
	case err == nil:
		c.admitViewLocked(k, f.view)
	}
}
func (c *Cache) heldViewLocked() *heldView {
	if c.heldView != nil {
		c.expireHeldViewLocked(c.now())
	}
	return c.heldView
}
func (c *Cache) expireHeldViewLocked(now time.Time) {
	if h := c.heldView; h != nil && now.Sub(h.at) >= c.cfg.floor {
		c.heldView = nil
		c.pruneViewOwnerLocked(h.key.Snap.Path)
	}
}
func (c *Cache) admitViewLocked(k ViewKey, v *View) {
	// All clock reads precede mutation. Fault injection does not promise rollback
	// after arbitrary map/list corruption.
	now := c.now()
	c.expireHeldViewLocked(now)
	e := &viewEntry{key: k, view: v, etag: v.ETag, settled: v.Settled, owner: v.owner}
	if old := c.views[k]; old != nil {
		if o := old.Value.(*viewEntry); o.etag == v.ETag && o.owner == v.owner {
			e.deltas = o.deltas
		}
		c.removeViewLocked(old)
	}
	if h := c.heldView; h != nil && (h.key == k || v.Bytes > c.cfg.viewBytes) {
		c.heldView = nil
		c.pruneViewOwnerLocked(h.key.Snap.Path)
	}
	if v.Bytes > c.cfg.viewBytes {
		c.stats.oversizedViews.Add(1)
		c.heldView = &heldView{key: k, view: v, at: now}
		e.view = nil
	} else {
		c.viewBytes += v.Bytes
	}
	c.views[k] = c.viewLRU.PushFront(e)
	c.evictViewsLocked()
}
func (c *Cache) evictViewsLocked() {
	var held int64
	if h := c.heldView; h != nil {
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
	c.pruneViewOwnerLocked(e.key.Snap.Path)
}
func (c *Cache) breakViewLocked(shape viewShape, owner *viewOwner) {
	if el := c.broken[shape]; el != nil {
		c.brokenLRU.MoveToFront(el)
		return
	}
	c.broken[shape] = c.brokenLRU.PushFront(&brokenView{shape: shape, owner: owner})
	for c.brokenLRU.Len() > c.cfg.viewFailures {
		c.removeBrokenViewLocked(c.brokenLRU.Back())
	}
}
func (c *Cache) removeBrokenViewLocked(el *list.Element) {
	b := el.Value.(*brokenView)
	delete(c.broken, b.shape)
	c.brokenLRU.Remove(el)
	c.pruneViewOwnerLocked(b.shape.snap.Path)
}

// Invalidating a parsed incarnation removes its retained authority, never its
// still-running flight. Late completions can return to callers but cannot retain.
func (c *Cache) dropViewsLocked(path string) {
	delete(c.viewOwners, path)
	for k, el := range c.views {
		if k.Snap.Path == path {
			c.removeViewLocked(el)
		}
	}
	if c.heldView != nil && c.heldView.key.Snap.Path == path {
		c.heldView = nil
	}
	for s, el := range c.broken {
		if s.snap.Path == path {
			c.removeBrokenViewLocked(el)
		}
	}
}

// Reference pruning uses raw fields, with no clock/expiry recursion. It removes
// only unreferenced current tokens, not all tokens ever used for a path.
func (c *Cache) pruneViewOwnerLocked(path string) {
	o := c.viewOwners[path]
	if o == nil {
		return
	}
	if o.parsed != nil {
		if el := c.full[path]; el != nil && el.Value.(*parsed) == o.parsed {
			return
		}
		if c.held == o.parsed {
			return
		}
	}
	for _, el := range c.views {
		if el.Value.(*viewEntry).owner == o {
			return
		}
	}
	if h := c.heldView; h != nil && h.view.owner == o {
		return
	}
	for _, el := range c.broken {
		if el.Value.(*brokenView).owner == o {
			return
		}
	}
	for _, f := range c.building {
		if f.owner == o {
			return
		}
	}
	delete(c.viewOwners, path)
}

// NotModified accepts only a settled current-incarnation full/delta ETag.
func (c *Cache) NotModified(k ViewKey, fromKey, etag string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.views[k]
	if el == nil || etag == "" {
		return false
	}
	e := el.Value.(*viewEntry)
	if !e.settled || !c.currentViewOwnerLocked(k.Snap.Path, e.owner) {
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

// NoteDelta requires the actual returned producing View. Equal-snapshot cache
// re-admission cannot turn an old response into authority to amend a new record.
func (c *Cache) NoteDelta(k ViewKey, producer *View, fromKey, etag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.views[k]
	if el == nil || producer == nil || producer.key != k {
		return
	}
	e := el.Value.(*viewEntry)
	if producer.owner != e.owner || !c.currentViewOwnerLocked(k.Snap.Path, e.owner) || producer.ETag != e.etag {
		return
	}
	for i, d := range e.deltas {
		if d.fromKey == fromKey {
			e.deltas = append(e.deltas[:i], e.deltas[i+1:]...)
			break
		}
	}
	e.deltas = append(e.deltas, delta{strings.Clone(fromKey), strings.Clone(etag)})
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
