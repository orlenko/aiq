package transcript

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
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
	parseBytes      int64         // estimated bytes the full parses may hold
	parseCount      int           // full parses kept
	rowCount        int           // list rows kept
	rowBytes        int64         // estimated bytes the list rows may hold
	quarantineCount int           // parser failures kept
	viewBytes       int64         // estimated bytes the built views may hold
	viewCount       int           // views and ETag records kept
	viewFailures    int           // renderer failures kept
	viewWidth       int           // view builders at once
	ceiling         int64         // the largest file parsed
	floor           time.Duration // the shortest time between two parses of a file
	wait            time.Duration // how long a request waits for a parse
	width           int           // parses at once
}

// The bounds until real-workload figures set them.
var defaultCacheConfig = cacheConfig{
	parseBytes:      64 << 20,
	parseCount:      32,
	rowCount:        4096,
	rowBytes:        16 << 20,
	quarantineCount: 4096,
	viewBytes:       16 << 20,
	viewCount:       64,
	viewFailures:    64,
	viewWidth:       2,
	ceiling:         128 << 20,
	floor:           2 * time.Second,
	wait:            5 * time.Second,
	width:           2,
}

// Cache is the navigator's way to transcripts. Every parse goes through its
// entry: the parse holds one of a few slots shared by the whole process,
// runs the recovering parser and is keyed by the snapshot taken before it
// opened the file; a snapshot whose parse panicked is not parsed again until
// the file changes or its bounded failure record is evicted. It keeps full
// parses, list rows and built views, each bounded. Cached sessions are shared:
// whoever reads one copies what it
// changes.
type Cache struct {
	cfg   cacheConfig
	sem   chan struct{}
	now   func() time.Time
	parse func(path string) (*Session, error) // optional fixture override; nil uses boundedParse

	mu              sync.Mutex
	full            map[string]*list.Element // by path, the newest parse: *parsed
	fullLRU         *list.List
	fullBytes       int64
	held            *parsed // an oversized parse not admitted, for the floor
	quarantine      map[string]quarantineRecord
	quarantineLRU   *list.List // Snapshot, newest first
	quarantineNodes map[string]*list.Element
	flights         map[string]*flight
	changed         chan struct{} // registry changes wake caller-owned admission waits
	// beforeAdmit is a test barrier after slot acquisition, before installation.
	beforeAdmit func(path string)
	rows        map[string]*list.Element // by path: *row
	rowLRU      *list.List
	rowBytes    int64
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
	snap  Snapshot
	sum   Summary
	err   error
	bytes int64
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
	origin    diagnostic.Origin // immutable after done; never a caller's holder
}

type quarantineRecord struct {
	Snapshot
	origin diagnostic.Origin
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
	RowBytes       int64 // their estimated size
	Quarantined    int   // parser failure records kept
	FailedViews    int   // renderer failure records kept
	ViewOwners     int   // referenced view path incarnations
	BuildingViews  int   // admitted view builders
	Views          int   // views and ETag records kept
	ViewBytes      int64 // the views' estimated size
}

// NewCache is a Cache with the navigator's bounds.
func NewCache() *Cache { return newCache(defaultCacheConfig) }

func newCache(cfg cacheConfig) *Cache {
	return &Cache{
		cfg:             cfg,
		sem:             make(chan struct{}, cfg.width),
		now:             time.Now,
		full:            map[string]*list.Element{},
		fullLRU:         list.New(),
		quarantine:      map[string]quarantineRecord{},
		quarantineLRU:   list.New(),
		quarantineNodes: map[string]*list.Element{},
		flights:         map[string]*flight{},
		changed:         make(chan struct{}),
		rows:            map[string]*list.Element{},
		rowLRU:          list.New(),
		viewState:       newViewState(cfg.viewWidth),
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
		RowBytes: c.rowBytes, Quarantined: len(c.quarantine),
		FailedViews: len(c.broken), ViewOwners: len(c.viewOwners), BuildingViews: len(c.building),
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
// ErrUnreadable until it changes or its failure record is evicted.
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
			diagnostic.Note(ctx, PanicOrigin(err))
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
			diagnostic.Note(ctx, PanicOrigin(err))
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
		diagnostic.Note(ctx, f.origin)
		return f.sess, f.snap, f.err
	case <-timer.C:
		noteParseFlight(ctx, f)
		return nil, Snapshot{}, preparing()
	case <-ctx.Done():
		noteParseFlight(ctx, f)
		return nil, Snapshot{}, ctx.Err()
	}
}

// Cancellation may observe already completed work, never pending fields or a
// future failure. The worker neither stores nor receives this caller's context.
func noteParseFlight(ctx context.Context, f *flight) {
	select {
	case <-f.done:
		diagnostic.Note(ctx, f.origin)
	default:
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
	if c.quarantinedLocked(snap) {
		return nil, nil, &unreadableError{path: strings.Clone(snap.Path), origin: c.quarantine[snap.Path].origin}
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
	var faults diagnostic.Latch
	defer func() {
		if value := recover(); value != nil {
			faults.Note(capturePanic(value))
			s, err = nil, &unreadableError{path: strings.Clone(f.snap.Path), origin: faults.Origin()}
		}
		c.finishFlight(f, s, err, faults.Origin())
	}()
	s, err = c.runParse(f.snap)
	faults.Note(PanicOrigin(err))
	if err == nil && s != nil {
		s = ownedCachedSession(s)
	}
}

// ownedCachedSession detaches the retained graph from parser backing storage.
// A short substring or an empty slice must not keep discarded records alive.
// This runs outside mu and parser recovery, under the admitted parse slot.
func ownedCachedSession(s *Session) *Session {
	out := *s
	out.Provider, out.ID = strings.Clone(s.Provider), strings.Clone(s.ID)
	out.Path, out.Cwd = strings.Clone(s.Path), strings.Clone(s.Cwd)
	out.Title, out.Branch, out.Model = strings.Clone(s.Title), strings.Clone(s.Branch), strings.Clone(s.Model)
	if s.Turns != nil {
		out.Turns = make([]Turn, len(s.Turns))
		copy(out.Turns, s.Turns)
		for i := range out.Turns {
			t := &out.Turns[i]
			t.Prompt, t.Reply = strings.Clone(t.Prompt), strings.Clone(t.Reply)
			t.Steers = ownedCachedStrings(t.Steers)
			t.Earlier = ownedCachedStrings(t.Earlier)
			if t.Items != nil {
				items := make([]TurnItem, len(t.Items))
				copy(items, t.Items)
				for j := range items {
					items[j].Text = strings.Clone(items[j].Text)
					items[j].MsgID = strings.Clone(items[j].MsgID)
				}
				t.Items = items
			}
		}
	}
	return &out
}

func ownedCachedStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, text := range in {
		out[i] = strings.Clone(text)
	}
	return out
}

// finishFlight owns terminal publication. Roles cannot join between capturing
// them and retiring the record; cleanup runs even if cache publication panics.
func (c *Cache) finishFlight(f *flight, s *Session, err error, origin diagnostic.Origin) {
	var faults diagnostic.Latch
	faults.Note(origin)
	faults.Note(PanicOrigin(err))
	c.mu.Lock()
	defer func() {
		f.origin = faults.Origin()
		delete(c.flights, f.snap.Path)
		close(f.done)
		c.flightChangedLocked()
		<-c.sem
		c.mu.Unlock()
	}()
	rowDone := false
	defer func() {
		if value := recover(); value != nil {
			faults.Note(capturePanic(value))
			f.sess, f.err = nil, &unreadableError{path: strings.Clone(f.snap.Path), origin: faults.Origin()}
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
		faults.Note(PanicOrigin(f.rowErr))
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
	var faults diagnostic.Latch
	c.stats.parses.Add(1)
	defer func() {
		if value := recover(); value != nil {
			faults.Note(capturePanic(value))
			s, err = nil, c.quarantinedOrigin(snap, faults.Origin())
		}
	}()
	if c.parse != nil {
		s, err = c.parse(snap.Path)
	} else {
		s, err = boundedParse(snap, c.cfg.ceiling)
	}
	faults.Note(PanicOrigin(err))
	switch {
	case errors.Is(err, ErrUnreadable):
		return nil, c.quarantinedOrigin(snap, PanicOrigin(err))
	case err == nil && s == nil:
		return nil, errNoSession
	}
	return s, err
}

func (c *Cache) quarantined(snap Snapshot) error {
	return c.quarantinedOrigin(snap, diagnostic.Origin{})
}

func (c *Cache) quarantinedOrigin(snap Snapshot, origin diagnostic.Origin) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.quarantinedLocked(snap) && c.cfg.quarantineCount > 0 {
		snap.Path = strings.Clone(snap.Path)
		c.quarantine[snap.Path] = quarantineRecord{Snapshot: snap, origin: origin}
		c.quarantineNodes[snap.Path] = c.quarantineLRU.PushFront(snap)
	}
	if q, ok := c.quarantine[snap.Path]; ok {
		if !q.origin.Present() && origin.Present() {
			q.origin = origin
			c.quarantine[q.Path] = q // retain the already owned map-key storage
		}
		origin = q.origin
	}
	for c.quarantineLRU.Len() > c.cfg.quarantineCount && c.quarantineLRU.Len() > 0 {
		c.removeQuarantineLocked(c.quarantineLRU.Back().Value.(Snapshot).Path)
	}
	return &unreadableError{path: strings.Clone(snap.Path), origin: origin}
}

// quarantinedLocked touches an exact failure, retiring a changed snapshot.
// Both list and detail lookups must maintain these same indexes.
func (c *Cache) quarantinedLocked(snap Snapshot) bool {
	q, ok := c.quarantine[snap.Path]
	if !ok {
		return false
	}
	if q.Snapshot != snap {
		c.removeQuarantineLocked(snap.Path)
		return false
	}
	c.quarantineLRU.MoveToFront(c.quarantineNodes[snap.Path])
	return true
}

func (c *Cache) removeQuarantineLocked(path string) {
	c.quarantineLRU.Remove(c.quarantineNodes[path])
	delete(c.quarantineNodes, path)
	delete(c.quarantine, path)
}

// heldLocked is the oversized parse held for the floor, while the floor lasts.
func (c *Cache) heldLocked() *parsed {
	if c.held != nil && c.now().Sub(c.held.at) >= c.cfg.floor {
		path := c.held.snap.Path
		c.held = nil
		c.dropViewsLocked(path)
	}
	return c.held
}

// admitLocked keeps a new parse. One larger than the whole budget is
// admitted alone, evicting the rest, unless another such parse is admitted
// already: then it is held, not admitted, for the floor, its bytes counting
// against the budget while held.
func (c *Cache) admitLocked(snap Snapshot, s *Session) {
	if snap.Path == s.Path {
		snap.Path = s.Path
	} else {
		snap.Path = strings.Clone(snap.Path)
	}
	p := &parsed{snap: snap, sess: s, bytes: sessionBytes(s), at: c.now()}
	if snap.Path != s.Path {
		p.bytes += int64(len(snap.Path))
	}
	h := c.heldLocked()
	// Even an equal snapshot can be a new parse incarnation. Do this after
	// fallible accounting/timing, before replacing any authoritative parse.
	c.dropViewsLocked(snap.Path)
	p.oversized = p.bytes > c.cfg.parseBytes
	if h != nil && h.snap.Path == snap.Path {
		c.held = nil
	}
	if p.oversized {
		for el := c.fullLRU.Front(); el != nil; el = el.Next() {
			if q := el.Value.(*parsed); q.oversized && q.snap.Path != snap.Path {
				c.removeFullLocked(snap.Path)
				if h := c.held; h != nil {
					c.held = nil
					c.dropViewsLocked(h.snap.Path)
				}
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
		delete(c.full, snap.Path)
		c.full[snap.Path] = el
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
			diagnostic.Note(ctx, PanicOrigin(err))
			return Summary{}, ErrBudget
		}
		if ok {
			c.mu.Unlock()
			diagnostic.Note(ctx, PanicOrigin(err))
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
			diagnostic.Note(ctx, PanicOrigin(err))
			return Summary{}, ErrBudget
		}
		if ok {
			c.mu.Unlock()
			<-c.sem
			diagnostic.Note(ctx, PanicOrigin(err))
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
		noteParseFlight(ctx, f)
		return Summary{}, ErrBudget
	case <-f.done:
		diagnostic.Note(ctx, f.origin)
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
	var faults diagnostic.Latch
	faults.Note(PanicOrigin(err))
	defer func() {
		if value := recover(); value != nil {
			faults.Note(capturePanic(value))
			sum, rowErr = Summary{}, &unreadableError{path: strings.Clone(snap.Path), origin: faults.Origin()}
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
	if c.quarantinedLocked(snap) {
		return Summary{}, &unreadableError{path: strings.Clone(snap.Path), origin: c.quarantine[snap.Path].origin}, true
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
	n := estimateRowBytes(r)
	if n > c.cfg.rowBytes {
		c.removeRowLocked(r.snap.Path)
		return // return the complete row to the caller without retaining a copy
	}
	owned := *r
	owned.bytes = n
	owned.snap.Path = strings.Clone(r.snap.Path)
	if r.sum.Path == r.snap.Path {
		owned.sum.Path = owned.snap.Path
	} else {
		owned.sum.Path = strings.Clone(r.sum.Path)
	}
	owned.sum.Provider = strings.Clone(r.sum.Provider)
	owned.sum.ID = strings.Clone(r.sum.ID)
	owned.sum.Cwd = strings.Clone(r.sum.Cwd)
	owned.sum.Label = strings.Clone(r.sum.Label)
	owned.sum.Title = strings.Clone(r.sum.Title)
	owned.sum.Model = strings.Clone(r.sum.Model)
	owned.sum.Branch = strings.Clone(r.sum.Branch)
	// Delete before reinserting: assigning an equal map key may retain the
	// old key's backing allocation, even when the new row owns its strings.
	c.removeRowLocked(r.snap.Path)
	c.rows[owned.snap.Path] = c.rowLRU.PushFront(&owned)
	c.rowBytes += n
	for c.rowLRU.Len() > c.cfg.rowCount || c.rowBytes > c.cfg.rowBytes {
		back := c.rowLRU.Back()
		if back == nil {
			// Preserve the supported negative-count publication fault seam,
			// after all retained rows and their bytes have been removed.
			panic("transcript: invalid row limit")
		}
		c.removeRowLocked(back.Value.(*row).snap.Path)
	}
}

func (c *Cache) removeRowLocked(path string) {
	if el := c.rows[path]; el != nil {
		c.rowBytes -= el.Value.(*row).bytes
		c.rowLRU.Remove(el)
		delete(c.rows, path)
	}
}

// estimateRowBytes includes conservative row/list/map overhead and owned
// UTF-8 string bytes. Snapshot.Path, its map key and an equal Summary.Path
// share one allocation. Estimate before cloning, including negative rows.
func estimateRowBytes(r *row) int64 {
	n := int64(512) + int64(len(r.snap.Path))
	if r.sum.Path != r.snap.Path {
		n += int64(len(r.sum.Path))
	}
	return n + int64(len(r.sum.Provider)) + int64(len(r.sum.ID)) +
		int64(len(r.sum.Cwd)) + int64(len(r.sum.Label)) +
		int64(len(r.sum.Title)) + int64(len(r.sum.Model)) + int64(len(r.sum.Branch))
}

// sessionBytes estimates what a parse holds: its text, and a fixed cost for
// each turn and item.
func sessionBytes(s *Session) int64 {
	const sessionCost, turnCost, itemCost, stringCost = 512, 320, 96, 16
	n := int64(sessionCost + len(s.Provider) + len(s.ID) + len(s.Path) + len(s.Cwd) + len(s.Title) + len(s.Branch) + len(s.Model))
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
