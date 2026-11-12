package transcript

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ErrTooLarge is a transcript over the cache's ceiling: it is never parsed
// for the web view.
var ErrTooLarge = errors.New("transcript: too large for the web view")

// ErrPreparing is a parse that did not finish within a request's wait. The
// parse goes on, if it started, and a later request finds its result.
var ErrPreparing = errors.New("transcript: preparing")

// PreparingError is ErrPreparing with how long the request waited.
type PreparingError struct{ Waited time.Duration }

func (e *PreparingError) Error() string        { return fmt.Sprintf("%v (waited %s)", ErrPreparing, e.Waited) }
func (e *PreparingError) Is(target error) bool { return target == ErrPreparing }

// Snapshot is one state of a transcript: its path, and the file's identity,
// size and modification time as a stat gave them before it was opened.
type Snapshot struct {
	Path     string
	Dev, Ino uint64
	Size     int64
	Mtime    int64 // Unix nanoseconds
}

// Stat is the snapshot of the transcript at path now.
func Stat(path string) (Snapshot, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Snapshot{}, err
	}
	if !fi.Mode().IsRegular() {
		return Snapshot{}, fmt.Errorf("%q is not a regular file", path)
	}
	dev, ino := fileID(fi)
	return Snapshot{Path: path, Dev: dev, Ino: ino, Size: fi.Size(), Mtime: fi.ModTime().UnixNano()}, nil
}

// cacheConfig holds a Cache's bounds; tests shrink them.
type cacheConfig struct {
	parseBytes int64         // estimated bytes the full parses may hold
	parseCount int           // full parses kept
	rowCount   int           // list rows kept
	viewBytes  int64         // estimated bytes the built views may hold
	viewCount  int           // views and ETag records kept
	ceiling    int64         // the largest file parsed
	floor      time.Duration // the shortest time between two parses of a file
	wait       time.Duration // how long a request waits for a parse
	width      int           // parses at once
}

// The bounds until real-workload figures set them.
var defaultCacheConfig = cacheConfig{
	parseBytes: 64 << 20,
	parseCount: 32,
	rowCount:   4096,
	viewBytes:  16 << 20,
	viewCount:  64,
	ceiling:    128 << 20,
	floor:      2 * time.Second,
	wait:       5 * time.Second,
	width:      2,
}

// Cache is the navigator's way to transcripts. Every parse goes through its
// entry: the parse holds one of a few slots shared by the whole process,
// runs the recovering parser and is keyed by the snapshot taken before it
// opened the file; a snapshot whose parse panicked is not parsed again until
// the file changes. It keeps full parses, list rows and built views, each
// bounded. Cached sessions are shared: whoever reads one copies what it
// changes.
type Cache struct {
	cfg   cacheConfig
	sem   chan struct{}
	now   func() time.Time
	parse func(path string) (*Session, error) // optional fixture override; nil uses boundedParse

	mu         sync.Mutex
	full       map[string]*list.Element // by path, the newest parse: *parsed
	fullLRU    *list.List
	fullBytes  int64
	held       *parsed // an oversized parse not admitted, for the floor
	quarantine map[string]Snapshot
	flights    map[string]*flight
	changed    chan struct{} // registry changes wake caller-owned admission waits
	// beforeAdmit is a test barrier after slot acquisition, before installation.
	beforeAdmit func(path string)
	rows        map[string]*list.Element // by path: *row
	rowLRU      *list.List
	viewState

	stats cacheStats
}

// parsed is one full parse.
type parsed struct {
	snap      Snapshot
	sess      *Session
	bytes     int64
	at        time.Time // when the parse finished
	oversized bool      // larger than the whole budget
}

// row is one list row, or the parse that found no session.
type row struct {
	snap Snapshot
	sum  Summary
	err  error
}

// flight is a parse running for one file, detached from the requests that
// wait for it. done closes once sess or err is set.
type flight struct {
	snap Snapshot
	done chan struct{}
	sess *Session
	err  error
	// Admission roles are guarded by Cache.mu until terminal publication.
	full, row bool
	sum       Summary
	rowErr    error
}

type cacheStats struct {
	parses, holds, preparing, viewBuilds, oversizedViews, viewNanos atomic.Int64
}

// CacheStats is what a Cache has done and holds.
type CacheStats struct {
	Parses         int64 // parses run, list and detail
	Holds          int64 // oversized parses held rather than admitted
	Preparing      int64 // requests answered ErrPreparing
	ViewBuilds     int64 // views built
	OversizedViews int64 // views built too large to admit
	ViewTime       time.Duration
	Parsed         int   // full parses kept
	ParsedBytes    int64 // their estimated size
	Rows           int   // list rows kept
	Views          int   // views and ETag records kept
	ViewBytes      int64 // the views' estimated size
}

// NewCache is a Cache with the navigator's bounds.
func NewCache() *Cache { return newCache(defaultCacheConfig) }

func newCache(cfg cacheConfig) *Cache {
	return &Cache{
		cfg:        cfg,
		sem:        make(chan struct{}, cfg.width),
		now:        time.Now,
		full:       map[string]*list.Element{},
		fullLRU:    list.New(),
		quarantine: map[string]Snapshot{},
		flights:    map[string]*flight{},
		changed:    make(chan struct{}),
		rows:       map[string]*list.Element{},
		rowLRU:     list.New(),
		viewState:  newViewState(),
	}
}

// Stats reports the cache's counters and sizes.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Parses: c.stats.parses.Load(), Holds: c.stats.holds.Load(), Preparing: c.stats.preparing.Load(),
		ViewBuilds: c.stats.viewBuilds.Load(), OversizedViews: c.stats.oversizedViews.Load(),
		ViewTime: time.Duration(c.stats.viewNanos.Load()),
		Parsed:   c.fullLRU.Len(), ParsedBytes: c.fullBytes, Rows: c.rowLRU.Len(),
		Views: c.viewLRU.Len(), ViewBytes: c.viewBytes,
	}
}

// Session is the parse of the transcript at path, with the snapshot it was
// parsed from. Concurrent requests for one file share one parse, which runs
// detached from them once it holds a parse slot; a request that waits longer
// than the cache's wait gets a *PreparingError. A request that gives up
// before a slot was free leaves nothing queued; one that gives up later
// leaves the parse to finish and be cached. Within the reparse floor a
// request gets the file's last parse, whatever its snapshot. A file over the
// ceiling answers ErrTooLarge without a parse, one whose parse panicked
// ErrUnreadable until it changes.
func (c *Cache) Session(ctx context.Context, path string) (*Session, Snapshot, error) {
	start := c.now()
	expires := time.Now().Add(c.cfg.wait)
	timer := time.NewTimer(c.cfg.wait)
	defer timer.Stop()
	preparing := func() error {
		c.stats.preparing.Add(1)
		return &PreparingError{Waited: c.now().Sub(start)}
	}
	stopped := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(expires) {
			return preparing()
		}
		return nil
	}

	snap, err := Stat(path)
	if err != nil {
		return nil, Snapshot{}, err
	}
	if snap.Size > c.cfg.ceiling {
		return nil, snap, ErrTooLarge
	}
	var f *flight
	for {
		c.mu.Lock()
		p, running, err := c.findLocked(snap)
		if p != nil || err != nil {
			c.mu.Unlock()
			return sessionOf(p, snap, err)
		}
		if err := stopped(); err != nil {
			c.mu.Unlock()
			return nil, Snapshot{}, err
		}
		if running != nil {
			running.full = true
			f = running
			c.mu.Unlock()
			break
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case c.sem <- struct{}{}:
		case <-changed:
			continue
		case <-timer.C:
			return nil, Snapshot{}, preparing()
		case <-ctx.Done():
			return nil, Snapshot{}, ctx.Err()
		}
		if c.beforeAdmit != nil {
			c.beforeAdmit(path)
		}
		// Another request may have started or finished the parse meanwhile.
		c.mu.Lock()
		p, running, err = c.findLocked(snap)
		if p != nil || err != nil {
			c.mu.Unlock()
			<-c.sem
			return sessionOf(p, snap, err)
		}
		if err := stopped(); err != nil {
			c.mu.Unlock()
			<-c.sem
			return nil, Snapshot{}, err
		}
		if running == nil {
			f = &flight{snap: snap, done: make(chan struct{}), full: true}
			c.flights[path] = f
			c.flightChangedLocked()
			go c.fly(f) // holds the slot until it ends
		} else {
			running.full = true
			f = running
			<-c.sem
		}
		c.mu.Unlock()
		break
	}
	select {
	case <-f.done:
		return f.sess, f.snap, f.err
	case <-timer.C:
		return nil, Snapshot{}, preparing()
	case <-ctx.Done():
		return nil, Snapshot{}, ctx.Err()
	}
}

func (c *Cache) flightChangedLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func sessionOf(p *parsed, snap Snapshot, err error) (*Session, Snapshot, error) {
	if err != nil {
		return nil, snap, err
	}
	return p.sess, p.snap, nil
}

// findLocked is what answers snap without a new parse: the quarantine, the
// cached parse (of this snapshot, or any within the floor), or the parse in
// flight.
func (c *Cache) findLocked(snap Snapshot) (*parsed, *flight, error) {
	if q, ok := c.quarantine[snap.Path]; ok {
		if q == snap {
			return nil, nil, &unreadableError{snap.Path}
		}
		delete(c.quarantine, snap.Path)
	}
	if el := c.full[snap.Path]; el != nil {
		p := el.Value.(*parsed)
		if p.snap == snap || c.now().Sub(p.at) < c.cfg.floor {
			c.fullLRU.MoveToFront(el)
			return p, nil, nil
		}
	}
	if h := c.heldLocked(); h != nil && h.snap.Path == snap.Path {
		return h, nil, nil
	}
	return nil, c.flights[snap.Path], nil
}

// fly runs f's parse on the slot its request took, and always completes f:
// a result or an error, done closed, the record gone, the slot freed.
func (c *Cache) fly(f *flight) {
	var s *Session
	var err error
	defer func() {
		if recover() != nil {
			s, err = nil, &unreadableError{f.snap.Path}
		}
		c.finishFlight(f, s, err)
	}()
	s, err = c.runParse(f.snap)
}

// finishFlight owns terminal publication. Roles cannot join between capturing
// them and retiring the record; cleanup runs even if cache publication panics.
func (c *Cache) finishFlight(f *flight, s *Session, err error) {
	c.mu.Lock()
	defer func() {
		delete(c.flights, f.snap.Path)
		close(f.done)
		c.flightChangedLocked()
		<-c.sem
		c.mu.Unlock()
	}()
	rowDone := false
	defer func() {
		if recover() != nil {
			f.sess, f.err = nil, &unreadableError{f.snap.Path}
			if !rowDone {
				f.sum, f.rowErr = Summary{}, f.err
			}
		}
	}()
	f.sess, f.err = s, err
	if err != nil {
		f.sess = nil // preserve the detail API's nil session on parse errors
	}
	if f.row {
		f.sum, f.rowErr = c.cacheSummaryLocked(f.snap, s, err)
		rowDone = true
	}
	if f.full && err == nil {
		c.admitLocked(f.snap, s)
	}
	if !f.full {
		f.sess = nil // a pure list never retains a directory of full sessions
	}
}

// runParse parses the file of snap on a slot the caller holds. A parse that
// panics quarantines the snapshot.
func (c *Cache) runParse(snap Snapshot) (s *Session, err error) {
	c.stats.parses.Add(1)
	defer func() {
		if recover() != nil {
			s, err = nil, c.quarantined(snap)
		}
	}()
	if c.parse != nil {
		s, err = c.parse(snap.Path)
	} else {
		s, err = boundedParse(snap, c.cfg.ceiling)
	}
	switch {
	case errors.Is(err, ErrUnreadable):
		return nil, c.quarantined(snap)
	case err == nil && s == nil:
		return nil, errNoSession
	}
	return s, err
}

func (c *Cache) quarantined(snap Snapshot) error {
	c.mu.Lock()
	c.quarantine[snap.Path] = snap
	c.mu.Unlock()
	return &unreadableError{snap.Path}
}

// heldLocked is the oversized parse held for the floor, while the floor lasts.
func (c *Cache) heldLocked() *parsed {
	if c.held != nil && c.now().Sub(c.held.at) >= c.cfg.floor {
		c.held = nil
	}
	return c.held
}

// admitLocked keeps a new parse. One larger than the whole budget is
// admitted alone, evicting the rest, unless another such parse is admitted
// already: then it is held, not admitted, for the floor, its bytes counting
// against the budget while held.
func (c *Cache) admitLocked(snap Snapshot, s *Session) {
	p := &parsed{snap: snap, sess: s, bytes: sessionBytes(s), at: c.now()}
	p.oversized = p.bytes > c.cfg.parseBytes
	if h := c.heldLocked(); h != nil && h.snap.Path == snap.Path {
		c.held = nil
	}
	if p.oversized {
		for el := c.fullLRU.Front(); el != nil; el = el.Next() {
			if q := el.Value.(*parsed); q.oversized && q.snap.Path != snap.Path {
				c.removeFullLocked(snap.Path)
				c.held = p
				c.stats.holds.Add(1)
				c.evictFullLocked()
				return
			}
		}
		for c.fullLRU.Len() > 0 {
			c.removeFullLocked(c.fullLRU.Back().Value.(*parsed).snap.Path)
		}
	}
	if el := c.full[snap.Path]; el != nil {
		c.fullBytes -= el.Value.(*parsed).bytes
		el.Value = p
		c.fullLRU.MoveToFront(el)
	} else {
		c.full[snap.Path] = c.fullLRU.PushFront(p)
	}
	c.fullBytes += p.bytes
	c.evictFullLocked()
}

func (c *Cache) evictFullLocked() {
	var held int64
	if h := c.heldLocked(); h != nil {
		held = h.bytes
	}
	for c.fullLRU.Len() > 0 {
		over := c.fullLRU.Len() > c.cfg.parseCount || c.fullBytes+held > c.cfg.parseBytes
		back := c.fullLRU.Back().Value.(*parsed)
		if !over || c.fullLRU.Len() == 1 && back.oversized {
			return
		}
		c.removeFullLocked(back.snap.Path)
	}
}

// removeFullLocked drops path's parse, and the views built from it.
func (c *Cache) removeFullLocked(path string) {
	el := c.full[path]
	if el == nil {
		return
	}
	c.fullBytes -= el.Value.(*parsed).bytes
	c.fullLRU.Remove(el)
	delete(c.full, path)
	c.dropViewsLocked(path)
}

// Summary is the list row of the transcript at path: from the row cache, or
// the cached parse, or else a parse that waits for a slot until ctx ends and
// then answers ErrBudget. Once admitted, a parse keeps its slot and fills the row
// cache even if its caller stops waiting. It is ListWith's parse on the navigator
// path. A list's parse fills the row cache only, so a long list cannot push the
// full parses out.
func (c *Cache) Summary(ctx context.Context, path string) (Summary, error) {
	if ctx.Err() != nil {
		return Summary{}, ErrBudget
	}
	snap, err := Stat(path)
	if err != nil {
		return Summary{}, err
	}
	if snap.Size > c.cfg.ceiling {
		return Summary{}, ErrTooLarge
	}
	var f *flight
	for {
		c.mu.Lock()
		sum, err, ok := c.rowLocked(snap)
		if ctx.Err() != nil {
			c.mu.Unlock()
			return Summary{}, ErrBudget
		}
		if ok {
			c.mu.Unlock()
			return sum, err
		}
		if running := c.flights[path]; running != nil {
			running.row = true
			f = running
			c.mu.Unlock()
			break
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case c.sem <- struct{}{}:
		case <-changed:
			continue
		case <-ctx.Done():
			return Summary{}, ErrBudget
		}
		if c.beforeAdmit != nil {
			c.beforeAdmit(path)
		}
		c.mu.Lock()
		sum, err, ok = c.rowLocked(snap)
		if ctx.Err() != nil {
			c.mu.Unlock()
			<-c.sem
			return Summary{}, ErrBudget
		}
		if ok {
			c.mu.Unlock()
			<-c.sem
			return sum, err
		}
		if running := c.flights[path]; running != nil {
			running.row = true
			f = running
			<-c.sem
		} else {
			f = &flight{snap: snap, done: make(chan struct{}), row: true}
			c.flights[path] = f
			c.flightChangedLocked()
			go c.fly(f)
		}
		c.mu.Unlock()
		break
	}
	select {
	case <-ctx.Done():
		return Summary{}, ErrBudget
	case <-f.done:
		if ctx.Err() != nil {
			return Summary{}, ErrBudget
		}
		return f.sum, f.rowErr
	}
}

// cacheSummaryLocked publishes a row without changing the parser's outcome.
// A row-publication failure must not quarantine a healthy parse or poison a
// detail waiter sharing that parse. The caller holds mu throughout publication.
func (c *Cache) cacheSummaryLocked(snap Snapshot, s *Session, err error) (sum Summary, rowErr error) {
	defer func() {
		if recover() != nil {
			sum, rowErr = Summary{}, &unreadableError{snap.Path}
		}
	}()
	if err != nil && !errors.Is(err, errNoSession) {
		return Summary{}, err
	}
	r := &row{snap: snap, err: err}
	if s != nil {
		r.sum = Summarize(s)
	}
	c.putRowLocked(r)
	return r.sum, r.err
}

// rowLocked answers snap's row without a parse, if it can.
func (c *Cache) rowLocked(snap Snapshot) (Summary, error, bool) {
	if q, ok := c.quarantine[snap.Path]; ok && q == snap {
		return Summary{}, &unreadableError{snap.Path}, true
	}
	if el := c.rows[snap.Path]; el != nil {
		if r := el.Value.(*row); r.snap == snap {
			c.rowLRU.MoveToFront(el)
			return r.sum, r.err, true
		}
	}
	if el := c.full[snap.Path]; el != nil {
		if p := el.Value.(*parsed); p.snap == snap {
			sum, err := c.cacheSummaryLocked(snap, p.sess, nil)
			return sum, err, true
		}
	}
	return Summary{}, nil, false
}

func (c *Cache) putRowLocked(r *row) {
	if el := c.rows[r.snap.Path]; el != nil {
		el.Value = r
		c.rowLRU.MoveToFront(el)
	} else {
		c.rows[r.snap.Path] = c.rowLRU.PushFront(r)
	}
	for c.rowLRU.Len() > c.cfg.rowCount {
		back := c.rowLRU.Back()
		delete(c.rows, back.Value.(*row).snap.Path)
		c.rowLRU.Remove(back)
	}
}

// sessionBytes estimates what a parse holds: its text, and a fixed cost for
// each turn and item.
func sessionBytes(s *Session) int64 {
	const sessionCost, turnCost, itemCost, stringCost = 512, 320, 96, 16
	n := int64(sessionCost + len(s.ID) + len(s.Path) + len(s.Cwd) + len(s.Title) + len(s.Branch) + len(s.Model))
	for i := range s.Turns {
		t := &s.Turns[i]
		n += int64(turnCost + len(t.Prompt) + len(t.Reply))
		for _, x := range t.Steers {
			n += int64(stringCost + len(x))
		}
		for _, x := range t.Earlier {
			n += int64(stringCost + len(x))
		}
		for _, it := range t.Items {
			n += int64(itemCost + len(it.Text) + len(it.MsgID))
		}
	}
	return n
}
