package transcript

import (
	"container/list"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

const inspectionID = "00000001-0000-4000-8000-000000000001"

func inspectionParse(path, provider, id string) *parsed {
	return &parsed{snap: Snapshot{Path: path, Dev: 11, Ino: 12, Size: 13, Mtime: 14},
		sess: &Session{Provider: provider, ID: id, Path: path}, bytes: 512,
		at: time.Unix(1800000000, 0)}
}

func inspectionPut(c *Cache, p *parsed) {
	c.full[p.snap.Path] = c.fullLRU.PushFront(p)
	c.fullBytes += p.bytes
}

// Freeze record values as well as pointer identities: retaining the original
// maps or pointers alone would miss in-place mutation by an inspection.
func inspectionValue(v any) any {
	switch p := v.(type) {
	case *parsed:
		if p == nil {
			return (*parsed)(nil)
		}
		var s Session
		if p.sess != nil {
			s = *p.sess
			s.Turns = append([]Turn(nil), s.Turns...)
			for i := range s.Turns {
				s.Turns[i].Steers = append([]string(nil), s.Turns[i].Steers...)
				s.Turns[i].Earlier = append([]string(nil), s.Turns[i].Earlier...)
				s.Turns[i].Items = append([]TurnItem(nil), s.Turns[i].Items...)
			}
		}
		return []any{p, *p, s}
	case *row:
		return []any{p, *p}
	case *viewEntry:
		x := *p
		x.deltas = append([]delta(nil), x.deltas...)
		return []any{p, x, inspectionValue(p.view), inspectionValue(p.owner)}
	case *View:
		if p == nil {
			return (*View)(nil)
		}
		return []any{p, *p}
	case *viewOwner:
		if p == nil {
			return (*viewOwner)(nil)
		}
		return []any{p, *p}
	case *brokenView:
		return []any{p, *p, inspectionValue(p.owner)}
	case *flight:
		return []any{p, *p, inspectionClosed(p.done)}
	case *viewFlight:
		return []any{p, *p, inspectionClosed(p.done), inspectionValue(p.owner)}
	default:
		return v
	}
}

func inspectionClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func inspectionList(l *list.List) []any {
	var out []any
	for el := l.Front(); el != nil; el = el.Next() {
		out = append(out, []any{el, el.Prev(), el.Next(), inspectionValue(el.Value)})
	}
	return out
}

func inspectionRaw(c *Cache) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	full, rows := map[string]any{}, map[string]any{}
	for k, v := range c.full {
		full[k] = []any{v, inspectionValue(v.Value)}
	}
	for k, v := range c.rows {
		rows[k] = []any{v, inspectionValue(v.Value)}
	}
	q, qnodes, flights := map[string]Snapshot{}, map[string]*list.Element{}, map[string]any{}
	for k, v := range c.quarantine {
		q[k] = v
	}
	for k, v := range c.quarantineNodes {
		qnodes[k] = v
	}
	for k, v := range c.flights {
		flights[k] = inspectionValue(v)
	}
	views, broken, owners, building := map[ViewKey]any{}, map[viewShape]any{}, map[string]any{}, map[viewBuildKey]any{}
	for k, v := range c.views {
		views[k] = []any{v, inspectionValue(v.Value)}
	}
	for k, v := range c.broken {
		broken[k] = []any{v, inspectionValue(v.Value)}
	}
	for k, v := range c.viewOwners {
		owners[k] = inspectionValue(v)
	}
	for k, v := range c.building {
		building[k] = inspectionValue(v)
	}
	var heldView any
	if c.heldView != nil {
		heldView = []any{c.heldView, *c.heldView, inspectionValue(c.heldView.view)}
	}
	return map[string]any{
		"full": full, "rows": rows, "quarantine": q, "qnodes": qnodes, "flights": flights,
		"views": views, "broken": broken, "owners": owners, "building": building,
		"lists": []any{inspectionList(c.fullLRU), inspectionList(c.rowLRU), inspectionList(c.quarantineLRU), inspectionList(c.viewLRU), inspectionList(c.brokenLRU)},
		"held":  inspectionValue(c.held), "heldView": heldView,
		"bytes":    []int64{c.fullBytes, c.rowBytes, c.viewBytes},
		"channels": []any{c.changed, inspectionClosed(c.changed), c.viewChanged, inspectionClosed(c.viewChanged), c.sem, len(c.sem), cap(c.sem), c.viewSem, len(c.viewSem), cap(c.viewSem)},
		"stats":    []int64{c.stats.parses.Load(), c.stats.holds.Load(), c.stats.preparing.Load(), c.stats.viewBuilds.Load(), c.stats.oversizedViews.Load(), c.stats.viewNanos.Load()},
	}
}

func inspectionCheck(t *testing.T, c *Cache, provider, id string, want CachedConversation, status CacheInspection) {
	t.Helper()
	before := inspectionRaw(c)
	got, state, err := c.InspectConversation(context.Background(), provider, id)
	if err != nil || state != status || got != want {
		t.Fatalf("inspection: status=%d err=%v; want status=%d, exact scalar payload match=%v", state, err, status, got == want)
	}
	if !reflect.DeepEqual(before, inspectionRaw(c)) {
		t.Fatal("inspection mutated raw cache state")
	}
}

// Seed every unrelated cache subsystem, including expired holds. The observer
// must not use normal lookup helpers, which perform maintenance on these.
func inspectionBackground(c *Cache, p *parsed) {
	r := &row{snap: Snapshot{Path: "row-only"}, sum: Summary{Provider: "claude", ID: inspectionID}, bytes: 67}
	c.rows[r.snap.Path] = c.rowLRU.PushFront(r)
	c.rowBytes = r.bytes
	q := Snapshot{Path: "quarantine-only", Size: 91}
	c.quarantine[q.Path] = q
	c.quarantineNodes[q.Path] = c.quarantineLRU.PushFront(q)
	o := &viewOwner{snap: p.snap, parsed: p}
	c.viewOwners[p.snap.Path] = o
	k := ViewKey{Snap: p.snap, Last: 3, Markup: true}
	v := &View{Value: "invented", ETag: "etag", Bytes: 71, Settled: true, owner: o, key: k}
	e := &viewEntry{key: k, view: v, etag: "etag", settled: true, owner: o, deltas: []delta{{"from", "old"}}}
	c.views[k] = c.viewLRU.PushFront(e)
	c.viewBytes = v.Bytes
	shape := shapeOf(k)
	c.broken[shape] = c.brokenLRU.PushFront(&brokenView{shape: shape, owner: o})
	c.heldView = &heldView{key: k, view: v, at: p.at.Add(-time.Hour)}
	c.flights["other-flight"] = &flight{snap: Snapshot{Path: "other-flight"}, done: make(chan struct{}), row: true}
	c.building[viewBuildKey{key: k, owner: o, parsed: p}] = &viewFlight{done: make(chan struct{}), owner: o}
	c.stats.parses.Store(4)
	c.stats.holds.Store(5)
	c.stats.preparing.Store(6)
	c.stats.viewBuilds.Store(7)
	c.stats.oversizedViews.Store(8)
	c.stats.viewNanos.Store(9)
}

func TestInspectConversationScalarFactsAndRawState(t *testing.T) {
	start := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("fixture", 3600))
	end := start.Add(-time.Second) // Preserve parser facts, including negative intervals.
	for _, provider := range []string{"claude", "codex", "agy"} {
		for _, tail := range []string{"absent", "zero", "closed", "open"} {
			t.Run(provider+"/"+tail, func(t *testing.T) {
				c := newCache(defaultCacheConfig)
				c.now = func() time.Time { t.Fatal("full-only inspection called clock"); return time.Time{} }
				p := inspectionParse("opaque-path", provider, inspectionID)
				p.sess.Unfinished = true
				want := CachedConversation{Snapshot: p.snap, Unfinished: true}
				if tail != "absent" {
					p.sess.Turns = []Turn{{Open: true, Started: start.Add(-time.Hour)}, {Open: tail == "open"}}
					want.HasTail, want.TailOpen = true, tail == "open"
					if tail != "zero" {
						p.sess.Turns[1].Started, p.sess.Turns[1].Ended = start, end
						want.TailStarted, want.TailEnded = start, end
					}
				}
				inspectionPut(c, p)
				inspectionBackground(c, p)
				inspectionCheck(t, c, provider, inspectionID, want, InspectionPresent)
			})
		}
	}
}

func TestInspectConversationHeldFloorAndAlias(t *testing.T) {
	for _, age := range []time.Duration{-time.Hour, 2*time.Second - time.Nanosecond, 2 * time.Second, 2*time.Second + time.Nanosecond, time.Hour} {
		for _, full := range []bool{false, true} {
			t.Run(age.String()+"/full="+map[bool]string{false: "no", true: "yes"}[full], func(t *testing.T) {
				c := newCache(defaultCacheConfig)
				p := inspectionParse("held-path", "claude", inspectionID)
				calls := 0
				c.now = func() time.Time { calls++; return p.at.Add(age) }
				c.held = p
				inspectionBackground(c, p)
				if full {
					inspectionPut(c, p)
				}
				status, want := InspectionMissing, CachedConversation{}
				if full || age < 2*time.Second {
					status, want = InspectionPresent, CachedConversation{Snapshot: p.snap}
				}
				inspectionCheck(t, c, "claude", inspectionID, want, status)
				wantCalls := 1
				if full {
					wantCalls = 0
				}
				if calls != wantCalls {
					t.Fatalf("clock calls=%d want=%d", calls, wantCalls)
				}
			})
		}
	}
}

func TestInspectConversationSelectionAndQuarantine(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		second, held, expired, quarantine bool
		want                              CacheInspection
	}{
		{"one", false, false, false, false, InspectionPresent},
		{"old-good-new-failed", false, false, false, true, InspectionUnusable},
		{"two-full", true, false, false, false, InspectionAmbiguous},
		{"two-full-quarantined", true, false, false, true, InspectionAmbiguous},
		{"full-held", true, true, false, false, InspectionAmbiguous},
		{"full-held-quarantined", true, true, false, true, InspectionAmbiguous},
		{"expired-held-duplicate", true, true, true, false, InspectionPresent},
	} {
		for _, reverse := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/forward", true: "/reverse"}[reverse], func(t *testing.T) {
				c := newCache(defaultCacheConfig)
				a, b := inspectionParse("a", "claude", inspectionID), inspectionParse("b", "claude", inspectionID)
				// Equal inode facts do not choose or collapse distinct retained copies.
				c.now = func() time.Time {
					if tc.expired {
						return b.at.Add(2 * time.Second)
					}
					return b.at
				}
				if reverse && tc.second && !tc.held {
					inspectionPut(c, b)
				}
				inspectionPut(c, a)
				if tc.second {
					if tc.held {
						c.held = b
					} else if !reverse {
						inspectionPut(c, b)
					}
				}
				if tc.quarantine {
					q := a.snap
					q.Size++
					c.quarantine[a.snap.Path] = q
					c.quarantineNodes[a.snap.Path] = c.quarantineLRU.PushFront(q)
				}
				want := CachedConversation{}
				if tc.want == InspectionPresent {
					want.Snapshot = a.snap
				}
				// Repeat to exercise map traversal independently of insertion order.
				for i := 0; i < 12; i++ {
					inspectionCheck(t, c, "claude", inspectionID, want, tc.want)
				}
			})
		}
	}
}

func TestInspectConversationNoScopeFromOtherTables(t *testing.T) {
	for _, table := range []string{"cold", "row", "view", "quarantine", "flight", "unrelated-held", "other-provider", "filename-is-not-id"} {
		t.Run(table, func(t *testing.T) {
			c := newCache(defaultCacheConfig)
			c.now = func() time.Time { t.Fatal("unrelated data called clock"); return time.Time{} }
			p := inspectionParse(inspectionID+".jsonl", "claude", inspectionID)
			switch table {
			case "row":
				r := &row{snap: p.snap, sum: Summary{Provider: "claude", ID: inspectionID}}
				c.rows[p.snap.Path] = c.rowLRU.PushFront(r)
			case "view":
				o := &viewOwner{snap: p.snap, parsed: p}
				c.viewOwners[p.snap.Path] = o
				k := ViewKey{Snap: p.snap}
				c.views[k] = c.viewLRU.PushFront(&viewEntry{key: k, view: &View{Value: p.sess}, owner: o})
			case "quarantine":
				c.quarantine[p.snap.Path] = p.snap
				c.quarantineNodes[p.snap.Path] = c.quarantineLRU.PushFront(p.snap)
			case "flight":
				c.flights[p.snap.Path] = &flight{snap: p.snap, sess: p.sess, done: make(chan struct{}), row: true}
			case "unrelated-held":
				p.sess.ID = "00000002-0000-4000-8000-000000000002"
				c.held = p
			case "other-provider":
				p.sess.Provider = "codex"
				inspectionPut(c, p)
			case "filename-is-not-id":
				p.sess.ID = "different"
				inspectionPut(c, p)
			}
			inspectionCheck(t, c, "claude", inspectionID, CachedConversation{}, InspectionMissing)
		})
	}
}

func TestInspectConversationOwnedPathAndRetiredTuple(t *testing.T) {
	c := newCache(defaultCacheConfig)
	large := strings.Repeat("x", 65536) + "owned-substring" + strings.Repeat("y", 65536)
	path := large[65536:65551]
	p := inspectionParse(path, "claude", inspectionID)
	p.sess.Turns = []Turn{{Open: true, Started: time.Unix(0, 0)}}
	inspectionPut(c, p)
	got, status, err := c.InspectConversation(context.Background(), "claude", inspectionID)
	if err != nil || status != InspectionPresent || got.Snapshot != p.snap || !got.HasTail || !got.TailOpen || got.TailStarted != time.Unix(0, 0) {
		t.Fatal("incorrect owned scalar observation")
	}
	if unsafe.StringData(got.Snapshot.Path) == unsafe.StringData(path) {
		t.Fatal("returned path retains borrowed string backing")
	}
	// An equal-Snapshot new parse is a distinct incarnation. Previously returned
	// scalars remain S1, and a later observation contains only S2's tail.
	q := inspectionParse(path, "claude", inspectionID)
	q.sess.Unfinished = true
	q.sess.Turns = []Turn{{Ended: time.Unix(19, 20)}}
	c.mu.Lock()
	c.full[path].Value = q
	c.mu.Unlock()
	want := CachedConversation{Snapshot: q.snap, Unfinished: true, HasTail: true, TailEnded: time.Unix(19, 20)}
	inspectionCheck(t, c, "claude", inspectionID, want, InspectionPresent)
	if got.Unfinished || !got.TailOpen || got.TailStarted != time.Unix(0, 0) || !got.TailEnded.IsZero() {
		t.Fatal("retirement changed earlier observation")
	}
	// Both immutable sessions share the exact Snapshot; never mix their tuples.
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.mu.Lock()
			c.full[path].Value = p
			c.mu.Unlock()
			c.mu.Lock()
			c.full[path].Value = q
			c.mu.Unlock()
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })
	want1 := CachedConversation{Snapshot: p.snap, HasTail: true, TailOpen: true, TailStarted: time.Unix(0, 0)}
	for i := 0; i < 200; i++ {
		v, s, e := c.InspectConversation(context.Background(), "claude", inspectionID)
		if e != nil || s != InspectionPresent || (v != want1 && v != want) {
			t.Fatal("mixed equal-Snapshot incarnation tuple")
		}
	}
}

type inspectionErrContext struct {
	context.Context
	n       atomic.Int64
	at      int64
	reached chan struct{}
	once    sync.Once
}

func (c *inspectionErrContext) Err() error {
	n := c.n.Add(1)
	err := c.Context.Err()
	if c.reached != nil {
		c.once.Do(func() { close(c.reached) })
	}
	if c.at > 0 && n >= c.at {
		return context.Canceled
	}
	return err
}

func TestInspectConversationCancellation(t *testing.T) {
	for _, id := range []string{inspectionID, "invalid"} {
		c := newCache(defaultCacheConfig)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		v, s, e := c.InspectConversation(ctx, "claude", id)
		if !errors.Is(e, context.Canceled) || s != InspectionMissing || v != (CachedConversation{}) {
			t.Fatal("cancellation must win ordinary validation")
		}
	}
	for _, pair := range [][2]string{{"unknown", inspectionID}, {"claude", "short"}, {"CLAUDE", inspectionID}, {"claude", inspectionID + "\n"}} {
		c := newCache(defaultCacheConfig)
		c.mu.Lock()
		// Invalid validation must not acquire the held mutex.
		done := make(chan error, 1)
		go func() {
			v, s, e := c.InspectConversation(context.Background(), pair[0], pair[1])
			if s != InspectionMissing || v != (CachedConversation{}) {
				e = errors.New("nonzero invalid result")
			}
			done <- e
		}()
		select {
		case e := <-done:
			c.mu.Unlock()
			if !errors.Is(e, ErrBadID) {
				t.Fatal("invalid scope did not return ErrBadID")
			}
		case <-time.After(3 * time.Second):
			c.mu.Unlock()
			<-done
			t.Fatal("invalid validation acquired cache lock")
		}
	}
	c := newCache(defaultCacheConfig)
	p := inspectionParse("path", "claude", inspectionID)
	inspectionPut(c, p)
	ctx, cancel := context.WithCancel(context.Background())
	observed := &inspectionErrContext{Context: ctx, reached: make(chan struct{})}
	c.mu.Lock()
	done := make(chan error, 1)
	go func() {
		v, s, e := c.InspectConversation(observed, "claude", inspectionID)
		if v != (CachedConversation{}) || s != InspectionMissing {
			e = errors.New("nonzero canceled observation")
		}
		done <- e
	}()
	<-observed.reached
	cancel()
	c.mu.Unlock()
	if e := <-done; !errors.Is(e, context.Canceled) {
		t.Fatalf("blocked acquisition cancellation: %v", e)
	}
	// With one full entry these stop at traversal, after capture/before clone,
	// and after clone. There is no asynchronous cancellation or global hook.
	for _, at := range []int64{2, 3, 4, 5} {
		before := inspectionRaw(c)
		controlled := &inspectionErrContext{Context: context.Background(), at: at}
		v, s, e := c.InspectConversation(controlled, "claude", inspectionID)
		if !errors.Is(e, context.Canceled) || s != InspectionMissing || v != (CachedConversation{}) {
			t.Fatalf("cooperative check %d failed", at)
		}
		if !reflect.DeepEqual(before, inspectionRaw(c)) {
			t.Fatal("canceled inspection mutated cache")
		}
	}
}

func TestInspectConversationFaultRecovery(t *testing.T) {
	for _, fault := range []string{"clock-panic", "wrong-type", "nil-element", "nil-parsed", "nil-session", "wrong-map-key"} {
		t.Run(fault, func(t *testing.T) {
			c := newCache(defaultCacheConfig)
			p := inspectionParse("path", "claude", inspectionID)
			el := c.fullLRU.PushFront(p)
			c.full[p.snap.Path] = el
			switch fault {
			case "clock-panic":
				delete(c.full, p.snap.Path)
				c.fullLRU.Remove(el)
				c.held = p
				c.now = func() time.Time { panic("invented private clock detail") }
			case "wrong-type":
				el.Value = "invented private type detail"
			case "nil-element":
				c.full[p.snap.Path] = nil
			case "nil-parsed":
				el.Value = (*parsed)(nil)
			case "nil-session":
				p.sess = nil
			case "wrong-map-key":
				delete(c.full, p.snap.Path)
				c.full["wrong"] = el
			}
			v, s, e := c.InspectConversation(context.Background(), "claude", inspectionID)
			if e != ErrInspection || e.Error() != "transcript: cached inspection failed" || v != (CachedConversation{}) || s != InspectionMissing {
				t.Fatal("internal fault did not return fixed zero error")
			}
			// A follow-up lock and healthy inspection must work; faults must not
			// leave quarantine, poison, registry mutations or a locked mutex.
			if !c.mu.TryLock() {
				t.Fatal("fault leaked cache lock")
			}
			c.mu.Unlock()
			if len(c.quarantine) != 0 || len(c.flights) != 0 || len(c.broken) != 0 {
				t.Fatal("inspection fault poisoned cache")
			}
			c.full = map[string]*list.Element{}
			c.fullLRU.Init()
			c.held = nil
			p = inspectionParse("recovery", "claude", inspectionID)
			inspectionPut(c, p)
			inspectionCheck(t, c, "claude", inspectionID, CachedConversation{Snapshot: p.snap}, InspectionPresent)
		})
	}
	// Explicit invariant failure is exceptional, rather than an ordinary result
	// whose payload/status can be replaced by a later cancellation observation.
	c := newCache(defaultCacheConfig)
	c.full["bad"] = nil
	v, s, e := c.InspectConversation(&inspectionErrContext{Context: context.Background(), at: 4}, "claude", inspectionID)
	if e != ErrInspection || s != InspectionMissing || v != (CachedConversation{}) {
		t.Fatal("invariant failure lost its fixed-error precedence")
	}
}

func TestInspectConversationWarmDeleteReplaceAndEviction(t *testing.T) {
	f := &fakeParse{}
	c, clk := testCache(t, f, func(cfg *cacheConfig) { cfg.parseCount = 2 })
	dir := t.TempDir()
	a := writeFile(t, filepath.Join(dir, inspectionID+".jsonl"), "invented-a")
	_, snap, err := c.Session(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	b := writeFile(t, filepath.Join(dir, "00000002-0000-4000-8000-000000000002.jsonl"), "invented-b")
	if _, _, err := c.Session(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Hour)
	want := CachedConversation{Snapshot: snap, HasTail: true}
	for i := 0; i < 8; i++ {
		inspectionCheck(t, c, "claude", inspectionID, want, InspectionPresent)
	}
	writeFile(t, a, "replacement with new bytes")
	inspectionCheck(t, c, "claude", inspectionID, want, InspectionPresent)
	if f.n.Load() != 2 {
		t.Fatal("inspection parsed deleted/replaced transcript")
	}
	d := writeFile(t, filepath.Join(dir, "00000003-0000-4000-8000-000000000003.jsonl"), "invented-c")
	if _, _, err := c.Session(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.full[a]; ok {
		t.Fatal("inspection promoted the original LRU victim")
	}
	if _, ok := c.full[b]; !ok {
		t.Fatal("inspection changed later eviction choice")
	}
	inspectionCheck(t, c, "claude", inspectionID, CachedConversation{}, InspectionMissing)
}

func TestInspectConversationDoesNotJoinOrPromoteParseFlight(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), inspectionID+".jsonl"), "invented")
	started, release := make(chan string, 1), make(chan struct{})
	f := &fakeParse{started: started, release: release}
	c, _ := testCache(t, f, nil)
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan error, 1)
	go func() { defer wg.Done(); _, err := c.Summary(context.Background(), path); done <- err }()
	// This cleanup also runs after an assertion fails. Acquire every parse slot
	// after joining the caller, proving detached worker terminal release too.
	t.Cleanup(func() {
		finish()
		wg.Wait()
		for i := 0; i < cap(c.sem); i++ {
			c.sem <- struct{}{}
		}
		for i := 0; i < cap(c.sem); i++ {
			<-c.sem
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("parse did not reach barrier")
	}
	inspectionCheck(t, c, "claude", inspectionID, CachedConversation{}, InspectionMissing)
	c.mu.Lock()
	flight := c.flights[path]
	full, row := flight.full, flight.row
	c.mu.Unlock()
	if full || !row {
		t.Fatal("inspection promoted a list-only parse flight")
	}
	if f.n.Load() != 1 || f.active.Load() != 1 {
		t.Fatal("inspection launched or waited for parse work")
	}
	finish()
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	inspectionCheck(t, c, "claude", inspectionID, CachedConversation{}, InspectionMissing)
}

func TestInspectConversationDoesNotJoinViewBuild(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), inspectionID+".jsonl"), "invented")
	_, snap, err := c.Session(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan error, 1)
	go func() {
		defer wg.Done()
		_, err := c.View(ViewKey{Snap: snap, Last: 2}, func() (*View, error) {
			close(started)
			<-release
			return &View{Value: "invented", Bytes: 32, Settled: true}, nil
		})
		done <- err
	}()
	t.Cleanup(func() {
		finish()
		wg.Wait()
		for i := 0; i < cap(c.viewSem); i++ {
			c.viewSem <- struct{}{}
		}
		for i := 0; i < cap(c.viewSem); i++ {
			<-c.viewSem
		}
	})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("view did not reach barrier")
	}
	inspectionCheck(t, c, "claude", inspectionID, CachedConversation{Snapshot: snap, HasTail: true}, InspectionPresent)
	finish()
	wg.Wait()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.n.Load() != 1 {
		t.Fatal("inspection launched an extra parse")
	}
}
