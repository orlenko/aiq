package transcript

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// Invented short values deliberately borrow much larger string allocations.
func cacheOwnedBorrow(value string) string {
	const padding = 64 << 10
	backing := strings.Repeat("p", padding) + value + strings.Repeat("q", padding)
	return backing[padding : padding+len(value)]
}

func cacheOwnedFixture(path string) *Session {
	b := cacheOwnedBorrow
	at := time.Date(2026, 1, 2, 9, 0, 0, 123, time.FixedZone("fixture", 3600))
	items := []TurnItem{
		{Kind: ItemSteer, Text: b(" steer é \n"), MsgID: b("message-a"), At: at},
		{Kind: ItemAnswer, Text: b("answer 日本語"), MsgID: b("message-b"), At: at.Add(time.Second), Promotes: true, Provisional: true, Supersedes: true},
		{Text: b("unused item"), MsgID: b("unused id")},
	}
	steers, earlier := []string{b(" steer \t"), "", b("unused steer")}, []string{b("earlier é"), "", b("unused earlier")}
	emptyItems, emptySteers, emptyEarlier := []TurnItem{{Text: b("empty spare item")}}, []string{b("empty spare steer")}, []string{b("empty spare earlier")}
	turns := []Turn{
		{Prompt: b(" prompt é \n"), Source: Notice, Steers: steers[:2], Reply: b(" reply \t"), ReplyAt: at.Add(2 * time.Second), Earlier: earlier[:2],
			Open: true, Started: at, Ended: at.Add(3 * time.Second), Tools: 7, Items: items[:2], Skipped: 3, PromptSkipped: true},
		{Prompt: "", Items: emptyItems[:0], Steers: emptySteers[:0], Earlier: emptyEarlier[:0]},
		{},
		{Prompt: b("unused turn"), Items: []TurnItem{{Text: b("nested unused")}}},
	}
	return &Session{Provider: b("fixture-provider"), ID: b("invented-session"), Path: b(path), Cwd: b("/invented/work"), Title: b(" title é "), Branch: b("fixture-branch"), Model: b("fixture-model"),
		Started: at, Updated: at.Add(time.Hour), Turns: turns[:3], Worker: true, Bypass: true, Unfinished: true, Skipped: 2}
}

type cacheOwnedSliceState[T any] struct {
	Len, Cap int
	Values   []T
}

func cacheOwnedFullSlice[T any](in []T) cacheOwnedSliceState[T] {
	var values []T
	if in != nil {
		values = make([]T, cap(in))
		copy(values, in[:cap(in)])
	}
	return cacheOwnedSliceState[T]{len(in), cap(in), values}
}

type cacheOwnedTurnState struct {
	Header          Turn
	Items           cacheOwnedSliceState[TurnItem]
	Steers, Earlier cacheOwnedSliceState[string]
}

type cacheOwnedInputState struct {
	Header   Session
	Len, Cap int
	Turns    []cacheOwnedTurnState
}

func cacheOwnedSnapshot(s *Session) cacheOwnedInputState {
	header := *s
	header.Turns = nil
	out := cacheOwnedInputState{Header: header, Len: len(s.Turns), Cap: cap(s.Turns)}
	for _, turn := range s.Turns[:cap(s.Turns)] {
		item := cacheOwnedTurnState{Items: cacheOwnedFullSlice(turn.Items), Steers: cacheOwnedFullSlice(turn.Steers), Earlier: cacheOwnedFullSlice(turn.Earlier)}
		turn.Items, turn.Steers, turn.Earlier = nil, nil, nil
		item.Header = turn
		out.Turns = append(out.Turns, item)
	}
	return out
}

func cacheOwnedStrings(s *Session) []string {
	values := []string{s.Provider, s.ID, s.Path, s.Cwd, s.Title, s.Branch, s.Model}
	for _, turn := range s.Turns {
		values = append(values, turn.Prompt, turn.Reply)
		values = append(values, turn.Steers...)
		values = append(values, turn.Earlier...)
		for _, item := range turn.Items {
			values = append(values, item.Text, item.MsgID)
		}
	}
	return values
}

func cacheOwnedCheckSlice[T any](t *testing.T, name string, original, owned []T) {
	t.Helper()
	if (original == nil) != (owned == nil) || len(original) != len(owned) || cap(owned) != len(owned) {
		t.Fatalf("%s shape/nilness/used capacity changed", name)
	}
	// This also detects zero-length slices retaining the original array even
	// when their append capacity was reduced to zero. Zerobase sharing is OK.
	if original != nil && cap(original) > 0 && unsafe.SliceData(original) == unsafe.SliceData(owned) {
		t.Fatalf("%s retained parser array", name)
	}
}

func cacheOwnedCheckGraph(t *testing.T, original, owned *Session) {
	t.Helper()
	if owned == nil || !reflect.DeepEqual(original, owned) {
		t.Fatal("owned session lost values")
	}
	a, b := cacheOwnedStrings(original), cacheOwnedStrings(owned)
	for i := range a {
		if len(a[i]) > 0 && unsafe.StringData(a[i]) == unsafe.StringData(b[i]) {
			t.Fatalf("string field %d retained borrowed backing", i)
		}
	}
	if owned == original {
		t.Fatal("owned session reused callback header")
	}
	cacheOwnedCheckSlice(t, "turns", original.Turns, owned.Turns)
	for i := range original.Turns {
		x, y := original.Turns[i], owned.Turns[i]
		cacheOwnedCheckSlice(t, "items", x.Items, y.Items)
		cacheOwnedCheckSlice(t, "steers", x.Steers, y.Steers)
		cacheOwnedCheckSlice(t, "earlier", x.Earlier, y.Earlier)
		if x.Started != y.Started || x.Ended != y.Ended || x.ReplyAt != y.ReplyAt {
			t.Fatal("turn time values/location changed")
		}
	}
	if original.Started != owned.Started || original.Updated != owned.Updated {
		t.Fatal("session time values/location changed")
	}
	runtime.KeepAlive(original)
}

func TestCacheOwnedPublicationWaitersAndHits(t *testing.T) {
	for _, rowLeader := range []bool{false, true} {
		t.Run(map[bool]string{false: "detail-led", true: "row-led-detail-late"}[rowLeader], func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			input := cacheOwnedFixture(path)
			// Independently allocated expected graph, including every spare slot.
			expected := cacheOwnedSnapshot(cacheOwnedFixture(path))
			c, _ := testCache(t, &fakeParse{}, nil)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			var calls atomic.Int64
			c.parse = func(string) (*Session, error) { calls.Add(1); close(started); <-release; return input, nil }
			leader := make(chan sharedCallResult, 1)
			go func() { leader <- sharedCall(c, context.Background(), path, !rowLeader) }()
			awaitFlightSignal(t, started)
			followers := make(chan sharedCallResult, 2)
			for i := 0; i < 2; i++ {
				ctx := &observedFlightContext{Context: context.Background(), seen: make(chan struct{})}
				go func() { followers <- sharedCall(c, ctx, path, true) }()
				awaitFlightSignal(t, ctx.seen)
			}
			finish()
			first, second, lead := awaitSharedCall(t, followers), awaitSharedCall(t, followers), awaitSharedCall(t, leader)
			if first.err != nil || second.err != nil || lead.err != nil || first.sess != second.sess {
				t.Fatal("shared flight publication failed")
			}
			if !rowLeader && lead.sess != first.sess {
				t.Fatal("detail leader received a different owned graph")
			}
			if rowLeader && lead.sum.Provider != input.Provider {
				t.Fatal("late detail join changed row outcome")
			}
			cacheOwnedCheckGraph(t, input, first.sess)
			hit, _, err := c.Session(context.Background(), cacheOwnedBorrow(path))
			if err != nil || hit != first.sess || calls.Load() != 1 {
				t.Fatal("cache hit lost shared owned-result identity")
			}
			c.mu.Lock()
			resident := c.full[path].Value.(*parsed)
			retained := resident.sess == first.sess && unsafe.StringData(resident.snap.Path) == unsafe.StringData(first.sess.Path)
			cleaned := len(c.flights) == 0 && len(c.sem) == 0
			c.mu.Unlock()
			if !retained || !cleaned {
				t.Fatal("resident ownership or terminal cleanup failed")
			}
			if !reflect.DeepEqual(cacheOwnedSnapshot(input), expected) {
				t.Fatal("callback graph/full-capacity sentinels mutated")
			}
		})
	}
}

func TestCacheOwnedConcurrentDistinctFlights(t *testing.T) {
	root := t.TempDir()
	paths := []string{writeFile(t, filepath.Join(root, "a.jsonl"), "a"), writeFile(t, filepath.Join(root, "b.jsonl"), "b")}
	input := cacheOwnedFixture("/invented/shared-callback.jsonl")
	expected := cacheOwnedSnapshot(cacheOwnedFixture("/invented/shared-callback.jsonl"))
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.width = 2 })
	started, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	c.parse = func(string) (*Session, error) { started <- struct{}{}; <-release; return input, nil }
	results := make(chan sharedCallResult, 2)
	for _, path := range paths {
		go func() { results <- sharedCall(c, context.Background(), path, true) }()
	}
	awaitFlightSignal(t, started)
	awaitFlightSignal(t, started)
	finish()
	a, b := awaitSharedCall(t, results), awaitSharedCall(t, results)
	if a.err != nil || b.err != nil || a.sess == b.sess {
		t.Fatal("distinct flights shared header or failed")
	}
	cacheOwnedCheckGraph(t, input, a.sess)
	cacheOwnedCheckGraph(t, input, b.sess)
	if unsafe.SliceData(a.sess.Turns) == unsafe.SliceData(b.sess.Turns) || unsafe.SliceData(a.sess.Turns[0].Items) == unsafe.SliceData(b.sess.Turns[0].Items) || unsafe.SliceData(a.sess.Turns[0].Steers) == unsafe.SliceData(b.sess.Turns[0].Steers) || unsafe.SliceData(a.sess.Turns[0].Earlier) == unsafe.SliceData(b.sess.Turns[0].Earlier) {
		t.Fatal("distinct flights shared mutable nested arrays")
	}
	if !reflect.DeepEqual(cacheOwnedSnapshot(input), expected) {
		t.Fatal("concurrent cloning mutated shared callback graph")
	}
}

func TestCacheOwnedEmptyTurns(t *testing.T) {
	for _, turns := range [][]Turn{nil, {}, make([]Turn, 0, 16)} {
		c, _ := testCache(t, &fakeParse{}, nil)
		input := &Session{Turns: turns}
		c.parse = func(string) (*Session, error) { return input, nil }
		path := writeFile(t, filepath.Join(t.TempDir(), "empty.jsonl"), "invented")
		got, _, err := c.Session(context.Background(), path)
		if err != nil {
			t.Fatal("empty session publication failed")
		}
		cacheOwnedCheckGraph(t, input, got)
	}
}

func TestCacheOwnedProductionAgy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invented-session", ".system_generated", "logs", "transcript.jsonl")
	content := strings.Repeat("padding ", 256<<10) + "<USER_REQUEST>  small é  </USER_REQUEST>" + strings.Repeat(" padding", 256<<10)
	line, err := json.Marshal(map[string]string{"type": "USER_INPUT", "content": content, "created_at": "2026-01-02T09:00:00Z"})
	if err != nil {
		t.Fatal("fixture encoding failed")
	}
	if len(line) >= MaxLine || int64(len(line)) >= defaultCacheConfig.ceiling {
		t.Fatal("fixture must exercise parsed, not skipped/oversized, data")
	}
	writeFile(t, path, string(line)+"\n")
	c, _ := testCache(t, &fakeParse{}, nil)
	c.parse = nil // Exercise the production bounded path/descriptor/AGY dispatch.
	got, _, err := c.Session(context.Background(), path)
	if err != nil || got == nil || got.Provider != "agy" || len(got.Turns) != 1 || got.Turns[0].Prompt != "small é" || got.Turns[0].PromptSkipped || got.Skipped != 0 || got.Turns[0].Skipped != 0 {
		t.Fatal("default bounded AGY dispatch did not preserve the short request")
	}
	observed, _ := testCache(t, &fakeParse{}, nil)
	callback := make(chan *Session, 1)
	// Supplemental observation invokes the same bounded parser and captures
	// its exact returned graph. It does not substitute for the default call.
	observed.parse = func(p string) (*Session, error) {
		snap, e := Stat(p)
		if e != nil {
			return nil, e
		}
		s, e := boundedParse(snap, observed.cfg.ceiling)
		callback <- s
		return s, e
	}
	owned, _, err := observed.Session(context.Background(), path)
	if err != nil {
		t.Fatal("observed bounded AGY parse failed")
	}
	original := <-callback
	cacheOwnedCheckGraph(t, original, owned)
	if !reflect.DeepEqual(got, owned) {
		t.Fatal("observer changed production parse values")
	}
	runtime.KeepAlive(content)
}

func cacheOwnedMapKey(c *Cache, path string) string {
	for key := range c.full {
		if key == path {
			return key
		}
	}
	return ""
}

func TestCacheOwnedAdmissionPathAndCharges(t *testing.T) {
	for _, tc := range []struct {
		snapshot, session, provider string
		bytes                       int64
	}{
		{"/lookup/key", "/lookup/key", "unit", 527},
		{"/payload/file", "/payload/file", "unit", 529},
		{"/lookup/key", "/payload/file", "unit", 540},
		{"/lookup/key", "/payload/file", "", 536},
	} {
		t.Run(tc.snapshot+tc.provider+tc.session, func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			request := cacheOwnedBorrow(tc.snapshot)
			s := &Session{Provider: strings.Clone(tc.provider), Path: strings.Clone(tc.session)}
			c.mu.Lock()
			defer c.mu.Unlock()
			c.admitLocked(Snapshot{Path: request}, s)
			node := c.full[tc.snapshot]
			p := node.Value.(*parsed)
			if p.bytes != tc.bytes || c.fullBytes != tc.bytes || c.fullLRU.Len() != 1 {
				t.Fatal("literal provider/distinct-path charge or resident totals incorrect")
			}
			if unsafe.StringData(p.snap.Path) == unsafe.StringData(request) {
				t.Fatal("resident retained caller snapshot backing")
			}
			if unsafe.StringData(cacheOwnedMapKey(c, tc.snapshot)) != unsafe.StringData(p.snap.Path) {
				t.Fatal("full map key did not share owned snapshot storage")
			}
			if tc.snapshot == tc.session && unsafe.StringData(p.snap.Path) != unsafe.StringData(s.Path) {
				t.Fatal("equal path was not shared with owned session")
			}
			oldKey := cacheOwnedMapKey(c, tc.snapshot)
			// Put another node in front, then replace this equal-valued key.
			c.admitLocked(Snapshot{Path: "/other"}, &Session{Path: "/other"})
			fresh := &Session{Provider: strings.Clone(tc.provider), Path: strings.Clone(tc.session)}
			replacementRequest := cacheOwnedBorrow(tc.snapshot)
			c.admitLocked(Snapshot{Path: replacementRequest, Mtime: 1}, fresh)
			p = c.full[tc.snapshot].Value.(*parsed)
			key := cacheOwnedMapKey(c, tc.snapshot)
			if c.full[tc.snapshot] != node || c.fullLRU.Front() != node || c.fullLRU.Len() != 2 {
				t.Fatal("replacement changed node or failed LRU movement")
			}
			if unsafe.StringData(key) == unsafe.StringData(oldKey) || unsafe.StringData(key) != unsafe.StringData(p.snap.Path) || unsafe.StringData(key) == unsafe.StringData(replacementRequest) {
				t.Fatal("equal full-map key was not rebound to newly owned path")
			}
			if tc.snapshot == tc.session && unsafe.StringData(key) != unsafe.StringData(fresh.Path) {
				t.Fatal("replacement equal path lost session sharing")
			}
			if p.bytes != tc.bytes || c.fullBytes != tc.bytes+518 {
				t.Fatal("replacement byte sum drifted")
			}
		})
	}
}

func TestCacheOwnedHeldPath(t *testing.T) {
	for _, equal := range []bool{true, false} {
		c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.parseBytes = 1 })
		sessionPath := "/payload/file"
		charge := int64(540)
		if equal {
			sessionPath = "/lookup/key"
			charge = 527
		}
		request := cacheOwnedBorrow("/lookup/key")
		s := &Session{Provider: "unit", Path: strings.Clone(sessionPath)}
		c.mu.Lock()
		c.admitLocked(Snapshot{Path: "/resident"}, &Session{Path: "/resident"})
		resident := c.full["/resident"]
		c.admitLocked(Snapshot{Path: request}, s)
		h := c.held
		valid := h != nil && h.sess == s && h.bytes == charge && h.oversized && c.full["/lookup/key"] == nil && c.full["/resident"] == resident && c.fullBytes == 521
		if valid {
			valid = unsafe.StringData(h.snap.Path) != unsafe.StringData(request)
			if equal {
				valid = valid && unsafe.StringData(h.snap.Path) == unsafe.StringData(s.Path)
			}
		}
		c.mu.Unlock()
		if !valid {
			t.Fatal("held path ownership, charge or resident policy failed")
		}
		clk.Add(c.cfg.floor + time.Second)
		c.mu.Lock()
		expired := c.heldLocked() == nil
		c.mu.Unlock()
		if !expired {
			t.Fatal("held ownership changed floor expiry")
		}
	}
}

var cacheOwnedBenchmarkSink *Session

func BenchmarkCacheOwnedPublication(b *testing.B) {
	for _, detail := range []bool{true, false} {
		name := "row-only"
		if detail {
			name = "detail"
		}
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "fixture.jsonl")
			if err := writeLinesErr(path, "invented"); err != nil {
				b.Fatal("benchmark fixture write failed")
			}
			input := cacheOwnedFixture(path)
			cfg := defaultCacheConfig
			cfg.parseCount, cfg.rowCount = 0, 0
			c := newCache(cfg)
			c.parse = func(string) (*Session, error) { return input, nil }
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if detail {
					s, _, err := c.Session(context.Background(), path)
					if err != nil {
						b.Fatal("detail benchmark failed")
					}
					cacheOwnedBenchmarkSink = s
				} else {
					if _, err := c.Summary(context.Background(), path); err != nil {
						b.Fatal("row-only benchmark failed")
					}
				}
			}
			b.StopTimer()
			if c.Stats().Parses != int64(b.N) || c.Stats().Parsed != 0 {
				b.Fatal("benchmark did not exercise each publication")
			}
			runtime.KeepAlive(input)
		})
	}
}
