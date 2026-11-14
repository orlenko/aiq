package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clock is a time a test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1_800_000_000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeParse stands in for the parsers: it counts parses and how many run at
// once, can be made to wait for release or to panic, and gives each file a
// session of size bytes (by default its file size).
type fakeParse struct {
	n, active, most atomic.Int64
	release         chan struct{} // nil: parses do not wait
	started         chan string   // if set, hears each parse as it starts
	panics          sync.Map      // path → true
	size            sync.Map      // path → int: bytes of prompt text
	open            bool          // the session's last turn is open
}

func (f *fakeParse) parse(path string) (*Session, error) {
	f.n.Add(1)
	now := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		m := f.most.Load()
		if now <= m || f.most.CompareAndSwap(m, now) {
			break
		}
	}
	if f.started != nil {
		f.started <- path
	}
	if f.release != nil {
		<-f.release
	}
	if _, ok := f.panics.Load(path); ok {
		panic("a parser bug quoting the transcript")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	n := int(fi.Size())
	if v, ok := f.size.Load(path); ok {
		n = v.(int)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return &Session{Provider: "claude", ID: id, Path: path, Cwd: "/w",
		Turns: []Turn{{Prompt: strings.Repeat("p", n), Open: f.open}}}, nil
}

func testCache(t *testing.T, f *fakeParse, set func(*cacheConfig)) (*Cache, *clock) {
	t.Helper()
	cfg := defaultCacheConfig
	cfg.wait = 2 * time.Second
	if set != nil {
		set(&cfg)
	}
	c := newCache(cfg)
	clk := newClock()
	c.now, c.parse = clk.Now, f.parse
	return c, clk
}

func writeFile(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// appendFile grows a file and moves its modification time on, so its
// snapshot changes even on a coarse clock.
func appendFile(t *testing.T, path, text string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(text)
	f.Close()
	next := fi.ModTime().Add(time.Second)
	os.Chtimes(path, next, next)
}

// Concurrent requests for one file share one parse.
func TestCacheOneParseForConcurrentRequests(t *testing.T) {
	f := &fakeParse{release: make(chan struct{})}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = c.Session(context.Background(), path)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(f.release)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.n.Load(); n != 1 {
		t.Errorf("%d parses for ten requests, want 1", n)
	}
}

// A request that gives up does not cancel the parse it waited for: the
// parse finishes, is cached, and the retry finds it without a second parse.
func TestCacheAbortLeavesTheParseToFinish(t *testing.T) {
	f := &fakeParse{release: make(chan struct{}), started: make(chan string, 1)}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, _, err := c.Session(ctx, path)
		done <- err
	}()
	<-f.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted request: %v", err)
	}
	close(f.release)
	for i := 0; ; i++ {
		s, _, err := c.Session(context.Background(), path)
		if err == nil && s != nil {
			break
		}
		if i > 100 {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.n.Load(); n != 1 {
		t.Errorf("%d parses, want the one the aborted request started", n)
	}
}

// With both slots held, a detail request answers "preparing" within the
// cache's wait, saying how long it waited, and starts nothing.
func TestCachePreparingBehindHeldSlots(t *testing.T) {
	f := &fakeParse{release: make(chan struct{}), started: make(chan string, 2)}
	c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.wait = 200 * time.Millisecond })
	c.now = time.Now
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		go c.Session(context.Background(), writeFile(t, filepath.Join(dir, name+".jsonl"), "x"))
		<-f.started
	}
	start := time.Now()
	_, _, err := c.Session(context.Background(), writeFile(t, filepath.Join(dir, "c.jsonl"), "x"))
	var p *PreparingError
	if !errors.As(err, &p) || !errors.Is(err, ErrPreparing) {
		t.Fatalf("request behind two held slots: %v", err)
	}
	if took := time.Since(start); took > time.Second || p.Waited < 200*time.Millisecond {
		t.Errorf("answered after %s, waited %s", took, p.Waited)
	}
	close(f.release)
	time.Sleep(50 * time.Millisecond)
	if n := f.n.Load(); n != 2 {
		t.Errorf("%d parses, want only the two that held slots", n)
	}
}

// Ten requests for ten distinct large files that all time out run at most
// two parses, and nothing is left queued once those end.
func TestCacheTimedOutRequestsQueueNothing(t *testing.T) {
	f := &fakeParse{release: make(chan struct{})}
	c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.wait = 50 * time.Millisecond })
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		path := writeFile(t, filepath.Join(dir, fmt.Sprintf("big%d.jsonl", i)), "x")
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := c.Session(context.Background(), path); !errors.Is(err, ErrPreparing) {
				t.Errorf("%s: %v", path, err)
			}
		}()
	}
	wg.Wait()
	close(f.release)
	time.Sleep(100 * time.Millisecond)
	if n := f.n.Load(); n > 2 {
		t.Errorf("%d parses ran for ten timed-out requests, want at most 2", n)
	}
	c.mu.Lock()
	left := len(c.flights)
	c.mu.Unlock()
	if left != 0 || len(c.sem) != 0 {
		t.Errorf("%d parses in flight and %d slots held after all ended", left, len(c.sem))
	}
}

// A parse that panics completes its record: the waiting request and the
// next one are told the transcript could not be parsed, never "preparing",
// and the file is not parsed again until it changes. An open error and a
// file over the ceiling complete it too.
func TestCachePanicCompletesTheRecord(t *testing.T) {
	f := &fakeParse{}
	c, clk := testCache(t, f, func(cfg *cacheConfig) { cfg.ceiling = 100 })
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	f.panics.Store(path, true)
	for i := 0; i < 3; i++ {
		_, _, err := c.Session(context.Background(), path)
		if !errors.Is(err, ErrUnreadable) || strings.Contains(err.Error(), "parser bug") {
			t.Fatalf("request %d after a panic: %v", i, err)
		}
	}
	if n := f.n.Load(); n != 1 {
		t.Errorf("%d parses of a quarantined snapshot, want 1", n)
	}
	f.panics.Delete(path)
	clk.Add(3 * time.Second)
	appendFile(t, path, "y")
	if _, _, err := c.Session(context.Background(), path); err != nil {
		t.Errorf("after the file changed: %v", err)
	}

	gone := filepath.Join(t.TempDir(), "gone.jsonl")
	if _, _, err := c.Session(context.Background(), gone); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	big := writeFile(t, filepath.Join(t.TempDir(), "big.jsonl"), strings.Repeat("x", 101))
	before := f.n.Load()
	for i := 0; i < 2; i++ {
		if _, _, err := c.Session(context.Background(), big); !errors.Is(err, ErrTooLarge) {
			t.Errorf("file over the ceiling: %v", err)
		}
		if _, err := c.Summary(context.Background(), big); !errors.Is(err, ErrTooLarge) {
			t.Errorf("list row over the ceiling: %v", err)
		}
	}
	if f.n.Load() != before {
		t.Error("a file over the ceiling was parsed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.flights) != 0 {
		t.Errorf("%d records left in flight", len(c.flights))
	}
}

// An open error inside the parse completes the record with that error.
func TestCacheParseErrorCompletesTheRecord(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	c.parse = func(string) (*Session, error) { f.n.Add(1); return nil, os.ErrPermission }
	for i := 0; i < 2; i++ {
		if _, _, err := c.Session(context.Background(), path); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.flights) != 0 || len(c.sem) != 0 {
		t.Errorf("%d records and %d slots left", len(c.flights), len(c.sem))
	}
}

// Within the floor a changed file answers its last parse; after it, the
// change is parsed.
func TestCacheReparseFloor(t *testing.T) {
	f := &fakeParse{}
	c, clk := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	_, first, _ := c.Session(context.Background(), path)
	appendFile(t, path, "y")
	_, snap, _ := c.Session(context.Background(), path)
	if snap != first || f.n.Load() != 1 {
		t.Errorf("inside the floor: snapshot %+v after %d parses", snap, f.n.Load())
	}
	clk.Add(2 * time.Second)
	_, snap, _ = c.Session(context.Background(), path)
	if snap == first || f.n.Load() != 2 {
		t.Errorf("after the floor: snapshot unchanged or %d parses", f.n.Load())
	}
}

// The byte budget evicts the least recently used parse, and its views.
func TestCacheByteBudgetEvicts(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, nil)
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 3; i++ {
		p := writeFile(t, filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i)), "x")
		f.size.Store(p, 1000)
		paths = append(paths, p)
	}
	_, s0, _ := c.Session(context.Background(), paths[0])
	// Room for two and a half of these parses.
	c.cfg.parseBytes = c.Stats().ParsedBytes * 5 / 2
	c.View(ViewKey{Snap: s0, Last: 50}, func() (*View, error) { return &View{ETag: "e", Bytes: 1}, nil })
	c.Session(context.Background(), paths[1])
	c.Session(context.Background(), paths[2])
	st := c.Stats()
	if st.Parsed != 2 || st.ParsedBytes > c.cfg.parseBytes {
		t.Errorf("%d parses holding %d bytes under a %d-byte budget", st.Parsed, st.ParsedBytes, c.cfg.parseBytes)
	}
	if st.Views != 0 {
		t.Errorf("%d views kept after their parse was evicted", st.Views)
	}
	c.mu.Lock()
	_, kept := c.full[paths[0]]
	c.mu.Unlock()
	if kept {
		t.Error("the least recently used parse was kept")
	}
}

// One parse larger than the whole budget is admitted, alone; a second is
// held for the floor only, counted, and a poll after the floor parses again.
func TestCacheOneOversizedEntry(t *testing.T) {
	f := &fakeParse{}
	c, clk := testCache(t, f, func(cfg *cacheConfig) { cfg.parseBytes = 3000 })
	dir := t.TempDir()
	small := writeFile(t, filepath.Join(dir, "small.jsonl"), "x")
	big1 := writeFile(t, filepath.Join(dir, "big1.jsonl"), "x")
	big2 := writeFile(t, filepath.Join(dir, "big2.jsonl"), "x")
	f.size.Store(big1, 5000)
	f.size.Store(big2, 5000)
	c.Session(context.Background(), small)
	c.Session(context.Background(), big1)
	if st := c.Stats(); st.Parsed != 1 {
		t.Fatalf("%d parses kept beside an oversized one", st.Parsed)
	}
	c.Session(context.Background(), big2)
	st := c.Stats()
	if st.Parsed != 1 || st.Holds != 1 {
		t.Fatalf("second oversized: %d kept, %d holds", st.Parsed, st.Holds)
	}
	c.Session(context.Background(), big2)
	if n := f.n.Load(); n != 3 {
		t.Errorf("a poll inside the floor parsed again (%d parses)", n)
	}
	clk.Add(2 * time.Second)
	c.Session(context.Background(), big2)
	if n := f.n.Load(); n != 4 {
		t.Errorf("a poll after the floor did not parse again (%d parses)", n)
	}
	c.mu.Lock()
	_, admitted := c.full[big2]
	c.mu.Unlock()
	if admitted {
		t.Error("a second oversized parse was admitted")
	}
}

// A second list of an unchanged directory parses nothing; an append is
// parsed again on the next list.
func TestCacheSecondListParsesNothing(t *testing.T) {
	r, work := listFixture(t)
	c := NewCache()
	var n atomic.Int64
	c.parse = func(path string) (*Session, error) { n.Add(1); return ParseFile(path) }
	first, err := ListWith(context.Background(), DirectWalk(r), work, c.Summary)
	if err != nil {
		t.Fatal(err)
	}
	parsed := n.Load()
	second, err := ListWith(context.Background(), DirectWalk(r), work, c.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if n.Load() != parsed {
		t.Errorf("the second list parsed %d transcripts", n.Load()-parsed)
	}
	if len(first.Rows) != len(second.Rows) || len(first.Rows) == 0 {
		t.Errorf("rows %d then %d", len(first.Rows), len(second.Rows))
	}
	want, _ := ListWith(context.Background(), DirectWalk(r), work, ParseSummary)
	if fmt.Sprint(want.Rows) != fmt.Sprint(second.Rows) {
		t.Errorf("cached rows differ from parsed ones:\n%v\n%v", second.Rows, want.Rows)
	}
}

// A list's parse waits for a slot only until its context ends, then answers
// ErrBudget; at most two parses run at once across lists and details.
func TestCacheSlotsHoldUnderListAndDetail(t *testing.T) {
	f := &fakeParse{release: make(chan struct{})}
	c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.wait = 30 * time.Millisecond })
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		path := writeFile(t, filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i)), "x")
		wg.Add(2)
		go func() { defer wg.Done(); c.Session(context.Background(), path) }()
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			if _, err := c.Summary(ctx, path); err != nil && !errors.Is(err, ErrBudget) {
				t.Error(err)
			}
		}()
	}
	time.Sleep(80 * time.Millisecond)
	close(f.release)
	wg.Wait()
	if m := f.most.Load(); m > 2 {
		t.Errorf("%d parses ran at once", m)
	}
}

// A panicking list parse is quarantined like a detail one.
func TestCacheSummaryQuarantines(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, nil)
	path := writeFile(t, filepath.Join(t.TempDir(), "a.jsonl"), "x")
	f.panics.Store(path, true)
	for i := 0; i < 2; i++ {
		if _, err := c.Summary(context.Background(), path); !errors.Is(err, ErrUnreadable) {
			t.Fatal(err)
		}
		if _, _, err := c.Session(context.Background(), path); !errors.Is(err, ErrUnreadable) {
			t.Fatal(err)
		}
	}
	if n := f.n.Load(); n != 1 {
		t.Errorf("%d parses, want 1", n)
	}
}

// Many goroutines listing, reading and building views at once, under -race.
func TestCacheConcurrentUse(t *testing.T) {
	f := &fakeParse{}
	c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.parseBytes, cfg.viewCount = 4000, 4 })
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 8; i++ {
		paths = append(paths, writeFile(t, filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i)), strings.Repeat("x", 500*i)))
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				p := paths[(g+i)%len(paths)]
				c.Summary(context.Background(), p)
				_, snap, err := c.Session(context.Background(), p)
				if err != nil {
					continue
				}
				k := ViewKey{Snap: snap, Last: i % 3, Markup: g%2 == 0}
				v, _ := c.View(k, func() (*View, error) { return &View{ETag: "e", Bytes: 100, Settled: true}, nil })
				c.NoteDelta(k, v, fmt.Sprint(i), "d")
				c.NotModified(k, "", "e")
			}
		}()
	}
	wg.Wait()
	if st := c.Stats(); st.Views > 4 || st.ParsedBytes > 4000 && st.Parsed > 1 {
		t.Errorf("bounds broken: %+v", st)
	}
}
