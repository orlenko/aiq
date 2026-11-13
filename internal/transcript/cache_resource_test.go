package transcript

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func resourcePutRow(c *Cache, r *row) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putRowLocked(r)
}

func resourceRows(t *testing.T, c *Cache, count int, bytes int64) {
	t.Helper()
	st := c.Stats()
	if st.Rows != count || st.RowBytes != bytes {
		t.Fatalf("rows/bytes = %d/%d, want %d/%d", st.Rows, st.RowBytes, count, bytes)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.rows) != count || c.rowLRU.Len() != count {
		t.Fatal("row indexes differ")
	}
	var total int64
	for el := c.rowLRU.Front(); el != nil; el = el.Next() {
		r := el.Value.(*row)
		if c.rows[r.snap.Path] != el {
			t.Fatal("row node not indexed")
		}
		total += r.bytes
	}
	if total != bytes {
		t.Fatalf("row charges = %d, want %d", total, bytes)
	}
}

func TestCacheResourceDefaults(t *testing.T) {
	if defaultCacheConfig.rowBytes != 16<<20 || defaultCacheConfig.rowCount != 4096 ||
		defaultCacheConfig.quarantineCount != 4096 || defaultCacheConfig.viewFailures != 64 ||
		defaultCacheConfig.viewWidth != 2 {
		t.Fatalf("resource defaults: %+v", defaultCacheConfig)
	}
}

func TestCacheResourceRowBytesAndLRU(t *testing.T) {
	r := func(path, label string) *row {
		return &row{snap: Snapshot{Path: path}, sum: Summary{Path: path, Label: label}}
	}
	// Fixed overhead, one shared path allocation, and UTF-8 bytes, not runes.
	const charge = int64(512 + len("/a") + len("猫é"))
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.rowBytes = 2 * charge })
	a, b, d := r("/a", "猫é"), r("/b", "猫é"), r("/d", "猫é")
	resourcePutRow(c, a)
	resourcePutRow(c, b)
	resourceRows(t, c, 2, 2*charge) // equality admits both
	c.mu.Lock()
	_, err, ok := c.rowLocked(a.snap)
	c.mu.Unlock()
	if !ok || err != nil {
		t.Fatal("could not touch row")
	}
	resourcePutRow(c, d)
	resourceRows(t, c, 2, 2*charge)
	if c.rows[b.snap.Path] != nil || c.rows[a.snap.Path] == nil {
		t.Fatal("byte eviction ignored row recency")
	}
	resourcePutRow(c, r("/a", "x"))
	resourceRows(t, c, 2, charge+int64(512+len("/a")+1))
	resourcePutRow(c, r("/a", strings.Repeat("界", 400)))
	resourceRows(t, c, 1, charge)
	if c.rows["/a"] != nil {
		t.Fatal("oversized replacement kept the previous row")
	}
	// Count eviction operates independently of the byte ceiling.
	c.cfg.rowCount = 1
	resourcePutRow(c, r("/b", "x"))
	resourceRows(t, c, 1, int64(512+len("/b")+1))
	c.cfg.rowCount = 0
	resourcePutRow(c, a)
	resourceRows(t, c, 0, 0)
}

// resourceBorrow gives a short substring backed by a much larger allocation.
func resourceBorrow(s string) string {
	return (s + strings.Repeat("z", 1<<16))[:len(s)]
}

func TestCacheResourceRowOwnsStringsAndReplacementKeys(t *testing.T) {
	for _, matchingPath := range []bool{false, true} {
		t.Run(fmt.Sprintf("matching-path=%v", matchingPath), func(t *testing.T) {
			c, _ := testCache(t, &fakeParse{}, nil)
			path := resourceBorrow("/invented/猫.jsonl")
			sumPath := resourceBorrow("/invented/other.jsonl")
			if matchingPath {
				sumPath = resourceBorrow(path)
			}
			r := &row{snap: Snapshot{Path: path}, sum: Summary{
				Provider: resourceBorrow("claude"), ID: resourceBorrow("invented-id"),
				Path: sumPath, Cwd: resourceBorrow("/invented/work"), Label: resourceBorrow("完整 label"),
				Title: resourceBorrow("invented title"), Model: resourceBorrow("fixture-model"),
				Branch: resourceBorrow("fixture-branch"),
			}}
			resourcePutRow(c, r)
			stored := c.rows[path].Value.(*row)
			if stored.snap != r.snap || stored.sum != r.sum {
				t.Fatal("ownership copy changed the row")
			}
			if unsafe.StringData(stored.snap.Path) == unsafe.StringData(path) {
				t.Fatal("snapshot path still borrows its input")
			}
			got := []string{stored.sum.Provider, stored.sum.ID, stored.sum.Path, stored.sum.Cwd,
				stored.sum.Label, stored.sum.Title, stored.sum.Model, stored.sum.Branch}
			borrowed := []string{r.sum.Provider, r.sum.ID, r.sum.Path, r.sum.Cwd,
				r.sum.Label, r.sum.Title, r.sum.Model, r.sum.Branch}
			for i := range got {
				if unsafe.StringData(got[i]) == unsafe.StringData(borrowed[i]) {
					t.Errorf("summary field %d still borrows its input", i)
				}
			}
			if matchingPath && unsafe.StringData(stored.sum.Path) != unsafe.StringData(stored.snap.Path) {
				t.Fatal("equal summary and snapshot paths did not share owned backing")
			}
			want := int64(512 + len(path))
			for i, s := range borrowed {
				if i != 2 || !matchingPath {
					want += int64(len(s))
				}
			}
			resourceRows(t, c, 1, want)
			oldPath := stored.snap.Path
			resourcePutRow(c, r)
			stored = c.rows[path].Value.(*row)
			for key := range c.rows {
				if unsafe.StringData(key) == unsafe.StringData(oldPath) ||
					unsafe.StringData(key) != unsafe.StringData(stored.snap.Path) {
					t.Fatal("replacement retained an old map key allocation")
				}
			}
			resourceRows(t, c, 1, want)
		})
	}
}

func TestCacheResourceNegativeRowOwnershipAndBudget(t *testing.T) {
	path := resourceBorrow("/invented/empty.jsonl")
	c, _ := testCache(t, &fakeParse{}, nil)
	c.mu.Lock()
	sum, err := c.cacheSummaryLocked(Snapshot{Path: path}, nil, errNoSession)
	c.mu.Unlock()
	if sum != (Summary{}) || !errors.Is(err, errNoSession) {
		t.Fatalf("negative row: %+v %v", sum, err)
	}
	resourceRows(t, c, 1, int64(512+len(path)))
	stored := c.rows[path].Value.(*row)
	for key := range c.rows {
		if unsafe.StringData(key) == unsafe.StringData(path) ||
			unsafe.StringData(key) != unsafe.StringData(stored.snap.Path) {
			t.Fatal("negative row retained borrowed path/key")
		}
	}
	c.cfg.rowBytes = int64(512 + len(path) - 1)
	c.mu.Lock()
	_, err = c.cacheSummaryLocked(Snapshot{Path: path, Size: 1}, nil, errNoSession)
	c.mu.Unlock()
	if !errors.Is(err, errNoSession) {
		t.Fatal(err)
	}
	resourceRows(t, c, 0, 0)
}

func TestCacheResourceOversizedRowReturnsFullLabelWithoutCopy(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.rowBytes = 1024 })
	if _, err := c.Summary(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Rows != 1 {
		t.Fatal("initial small row was not admitted")
	}
	label := resourceBorrow(strings.Repeat("全", 1024))
	c.parse = func(path string) (*Session, error) {
		return &Session{Path: path, ID: "invented", Title: label}, nil
	}
	appendFile(t, path, "grown")
	sum, err := c.Summary(context.Background(), path)
	if err != nil || sum.Label != label || sum.Title != label {
		t.Fatalf("oversized row lost complete labels: label bytes %d, err %v", len(sum.Label), err)
	}
	resourceRows(t, c, 0, 0)
	oversized := &row{snap: Snapshot{Path: path}, sum: sum}
	if allocations := testing.AllocsPerRun(10, func() { resourcePutRow(c, oversized) }); allocations != 0 {
		t.Fatalf("oversized row copied before rejection: %g allocations", allocations)
	}
}

func TestCacheResourceRowFaultLeavesCoherentBytes(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	resourcePutRow(c, &row{snap: Snapshot{Path: "/a"}})
	resourcePutRow(c, &row{snap: Snapshot{Path: "/b"}})
	c.cfg.rowCount = -1
	c.mu.Lock()
	_, err := c.cacheSummaryLocked(Snapshot{Path: "/c"}, &Session{Path: "/c"}, nil)
	c.mu.Unlock()
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("row fault was not sanitized: %v", err)
	}
	resourceRows(t, c, 0, 0)
	resourceQuarantine(t, c, 0)
}

func resourceQuarantine(t *testing.T, c *Cache, count int) {
	t.Helper()
	if st := c.Stats(); st.Quarantined != count {
		t.Fatalf("quarantine stats = %d, want %d", st.Quarantined, count)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.quarantine) != count || len(c.quarantineNodes) != count || c.quarantineLRU.Len() != count {
		t.Fatal("quarantine indexes differ")
	}
	for el := c.quarantineLRU.Front(); el != nil; el = el.Next() {
		snap := el.Value.(Snapshot)
		if c.quarantineNodes[snap.Path] != el || c.quarantine[snap.Path] != snap {
			t.Fatal("quarantine node not indexed by original snapshot")
		}
	}
}

func resourceCall(c *Cache, path, api string) error {
	if api == "summary" {
		_, err := c.Summary(context.Background(), path)
		return err
	}
	_, _, err := c.Session(context.Background(), path)
	return err
}

func TestCacheResourceQuarantineTouchEvictionAndRetry(t *testing.T) {
	for _, api := range []string{"summary", "session"} {
		t.Run(api, func(t *testing.T) {
			f := &fakeParse{}
			c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.quarantineCount = 2 })
			dir := t.TempDir()
			a := writeFile(t, filepath.Join(dir, "a.jsonl"), "invented-a")
			b := writeFile(t, filepath.Join(dir, "b.jsonl"), "invented-b")
			d := writeFile(t, filepath.Join(dir, "d.jsonl"), "invented-d")
			for _, path := range []string{a, b, d} {
				f.panics.Store(path, true)
			}
			for _, path := range []string{a, b} {
				if err := resourceCall(c, path, api); !errors.Is(err, ErrUnreadable) {
					t.Fatal(err)
				}
			}
			resourceQuarantine(t, c, 2)
			f.panics.Delete(a)
			if err := resourceCall(c, a, api); !errors.Is(err, ErrUnreadable) || f.n.Load() != 2 {
				t.Fatalf("unchanged failure retried before eviction: %v, parses %d", err, f.n.Load())
			}
			if err := resourceCall(c, d, api); !errors.Is(err, ErrUnreadable) {
				t.Fatal(err)
			}
			resourceQuarantine(t, c, 2)
			if _, ok := c.quarantine[b]; ok || c.quarantine[a].Path != a {
				t.Fatal("exact lookup did not touch the quarantine LRU")
			}
			f.panics.Delete(b)
			if err := resourceCall(c, b, api); err != nil || f.n.Load() != 4 {
				t.Fatalf("evicted unchanged input did not retry: %v, parses %d", err, f.n.Load())
			}
		})
	}
}

func TestCacheResourceQuarantineChangedSnapshotBothAPIs(t *testing.T) {
	for _, api := range []string{"summary", "session"} {
		t.Run(api, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			f := &fakeParse{}
			f.panics.Store(path, true)
			c, _ := testCache(t, f, nil)
			if err := resourceCall(c, path, api); !errors.Is(err, ErrUnreadable) {
				t.Fatal(err)
			}
			resourceQuarantine(t, c, 1)
			f.panics.Delete(path)
			appendFile(t, path, "changed")
			if err := resourceCall(c, path, api); err != nil {
				t.Fatal(err)
			}
			resourceQuarantine(t, c, 0)
			if f.n.Load() != 2 {
				t.Fatal("changed snapshot was not reparsed")
			}
		})
	}
}

func TestCacheResourceQuarantineOwnsKeysAndBoundsChurn(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.quarantineCount = 3 })
	for i := 0; i < 40; i++ {
		path := resourceBorrow(fmt.Sprintf("/invented/%d.jsonl", i))
		snap := Snapshot{Path: path, Dev: 1, Ino: uint64(i), Size: int64(i), Mtime: int64(i)}
		c.quarantined(snap)
		for _, change := range []bool{false, true} {
			if change {
				snap.Size++
			}
			c.quarantined(snap) // duplicate insertion touches; replacement owns new keys
			want := min(i+1, 3)
			resourceQuarantine(t, c, want)
			stored := c.quarantine[path]
			if stored != snap || unsafe.StringData(stored.Path) == unsafe.StringData(path) {
				t.Fatal("failure marker lost original attributes or retained borrowed path")
			}
			for key := range c.quarantine {
				if unsafe.StringData(key) != unsafe.StringData(c.quarantine[key].Path) {
					t.Fatal("failure map retained a different key backing allocation")
				}
			}
			for key, node := range c.quarantineNodes {
				if unsafe.StringData(key) != unsafe.StringData(node.Value.(Snapshot).Path) {
					t.Fatal("failure node index retained a different key backing allocation")
				}
			}
		}
	}
}

func TestCacheResourceQuarantineDisabledAndOrdinaryErrors(t *testing.T) {
	for _, api := range []string{"summary", "session"} {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/panic=%v", api, panics), func(t *testing.T) {
				path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
				c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) {
					if panics {
						cfg.quarantineCount = 0
					}
				})
				calls := 0
				ordinary := errors.New("invented read error")
				c.parse = func(string) (*Session, error) {
					calls++
					if panics {
						panic("invented parser fault")
					}
					return nil, ordinary
				}
				for i := 0; i < 2; i++ {
					err := resourceCall(c, path, api)
					if panics && !errors.Is(err, ErrUnreadable) || !panics && !errors.Is(err, ordinary) {
						t.Fatal(err)
					}
					resourceQuarantine(t, c, 0)
				}
				if calls != 2 {
					t.Fatalf("retryable outcome parsed %d times", calls)
				}
			})
		}
	}
}

func TestCacheResourceFullAdmissionInvalidatesEqualSnapshotViews(t *testing.T) {
	c, _ := testCache(t, &fakeParse{}, nil)
	snap := Snapshot{Path: "/invented/fixture.jsonl"}
	s := &Session{Path: snap.Path}
	k := ViewKey{Snap: snap}
	build := func() (*View, error) { return &View{ETag: "invented-etag", Bytes: 10, Settled: true}, nil }
	for i := 0; i < 2; i++ {
		if _, err := c.View(k, build); err != nil || !c.NotModified(k, "", "invented-etag") {
			t.Fatalf("could not seed view: %v", err)
		}
		// First parsed admission after standalone, then equal-snapshot replacement.
		c.mu.Lock()
		c.admitLocked(snap, s)
		c.mu.Unlock()
		if c.NotModified(k, "", "invented-etag") {
			t.Fatal("new full parse incarnation retained the old view")
		}
	}
	if _, err := c.View(k, build); err != nil {
		t.Fatal(err)
	}
	resourcePutRow(c, &row{snap: snap, sum: Summary{Path: snap.Path, Label: "invented"}})
	c.cfg.rowBytes = 1
	resourcePutRow(c, &row{snap: snap})
	resourceRows(t, c, 0, 0)
	if !c.NotModified(k, "", "invented-etag") || c.Stats().Parsed != 1 {
		t.Fatal("pure row replacement/eviction invalidated a full parse or its view")
	}
}

func TestCacheResourceHeldParseOverwriteAndExpiryDropViews(t *testing.T) {
	c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.parseBytes = 1 })
	admit := func(path string) Snapshot {
		snap := Snapshot{Path: path}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.admitLocked(snap, &Session{Path: path})
		return snap
	}
	admit("/invented/resident.jsonl")
	a := admit("/invented/held-a.jsonl")
	k := ViewKey{Snap: a}
	build := func() (*View, error) { return &View{ETag: "held-etag", Bytes: 10, Settled: true}, nil }
	if _, err := c.View(k, build); err != nil {
		t.Fatal(err)
	}
	b := admit("/invented/held-b.jsonl")
	if c.NotModified(k, "", "held-etag") || c.held == nil || c.held.snap != b {
		t.Fatal("held overwrite retained the old view or lost the new parse")
	}
	k.Snap = b
	if _, err := c.View(k, build); err != nil {
		t.Fatal(err)
	}
	clk.Add(c.cfg.floor + time.Second)
	c.mu.Lock()
	h := c.heldLocked()
	c.mu.Unlock()
	if h != nil || c.NotModified(k, "", "held-etag") {
		t.Fatal("held expiry retained the old parse or view")
	}
}

func TestCacheResourceAdmissionClockFaultPreservesCurrentViews(t *testing.T) {
	c, clk := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.parseBytes = 1 })
	snap := Snapshot{Path: "/invented/resident.jsonl"}
	c.mu.Lock()
	c.admitLocked(snap, &Session{Path: snap.Path})
	c.admitLocked(Snapshot{Path: "/invented/held.jsonl"}, &Session{})
	c.mu.Unlock()
	k := ViewKey{Snap: snap}
	if _, err := c.View(k, func() (*View, error) {
		return &View{ETag: "current-etag", Bytes: 10, Settled: true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	reads := 0
	c.now = func() time.Time {
		reads++
		if reads == 2 {
			panic("invented second clock fault")
		}
		return clk.Now()
	}
	panicked := false
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		defer func() { panicked = recover() != nil }()
		c.admitLocked(snap, &Session{Path: snap.Path})
	}()
	c.now = clk.Now
	if !panicked || !c.NotModified(k, "", "current-etag") {
		t.Fatal("pre-mutation timing fault erased an existing view")
	}
}
