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

// Invented session ids.
const (
	idA = "0f0e0d0c-0b0a-4908-8706-050403020100"
	idB = "1a2b3c4d-5e6f-4a0b-9c1d-2e3f40516273"
)

func testIndex(r Roots) (*Index, *clock) {
	x := NewIndex(r)
	clk := newClock()
	x.now = clk.Now
	return x, clk
}

// waitWalk waits until the index has run n Codex walks and the last has
// completed.
func waitWalk(t *testing.T, x *Index, n int64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		x.codex.mu.Lock()
		done := x.codex.running == nil && x.codex.gen >= uint64(n)
		x.codex.mu.Unlock()
		if done && x.Walks() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("walk %d did not complete (%d walks)", n, x.Walks())
}

// Through the index and the cache, ListWith gives what the direct walk
// gives, and a second list of an unchanged directory parses nothing and
// walks no rollout within the TTL.
func TestIndexSecondListNoParseNoWalk(t *testing.T) {
	r, work := listFixture(t)
	x, _ := testIndex(r)
	c := NewCache()
	want, err := ListWith(context.Background(), DirectWalk(r), work, ParseSummary)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err := ListWith(context.Background(), x, work, c.Summary)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got.Rows) != fmt.Sprint(want.Rows) || got.Of != want.Of {
			t.Fatalf("list %d through the index:\n%v\nwant\n%v", i, got.Rows, want.Rows)
		}
	}
	if st := c.Stats(); st.Parses != int64(want.Of) {
		t.Errorf("%d parses for two lists of %d transcripts", st.Parses, want.Of)
	}
	if x.Walks() != 1 {
		t.Errorf("%d rollout walks within the TTL", x.Walks())
	}
}

// The lookups behind /state for a bare Codex lease read the index: no walk
// within the TTL, and the same answer as the direct walk.
func TestIndexWrittenWalksOncePerTTL(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	os.MkdirAll(work, 0o700)
	work = canonical(work)
	codexHome := filepath.Join(root, "codex")
	day := filepath.Join(codexHome, "sessions", "2026", "10", "21")
	writeLines(t, filepath.Join(day, "rollout-a.jsonl"), codexFixture(work, idA)...)
	writeLines(t, filepath.Join(day, "rollout-x.jsonl"), strings.ReplaceAll(codexFixture(work, idB)[0], `"source":"cli"`, `"source":"exec"`))
	r := DefaultRoots(filepath.Join(root, "claude"), codexHome, "", "")
	x, _ := testIndex(r)
	since := time.Now().Add(-time.Hour)
	want := Written(r, "codex", work, since)
	for i := 0; i < 5; i++ {
		if got := x.Written(context.Background(), "codex", work, since); fmt.Sprint(got) != fmt.Sprint(want) || len(got) != 1 {
			t.Fatalf("poll %d: %v, want %v", i, got, want)
		}
	}
	if x.Walks() != 1 {
		t.Errorf("%d walks for five polls within the TTL", x.Walks())
	}
}

// A read past the TTL starts one scan, shared by every reader, and each is
// answered from the last completed scan without waiting for it.
func TestIndexPastTTLAnsweredFromLastScan(t *testing.T) {
	x, clk := testIndex(Roots{})
	var runs atomic.Int64
	release := make(chan struct{})
	run := func(prev any) any {
		n := runs.Add(1)
		if n > 1 {
			<-release
		}
		return n
	}
	var s scan
	if d, _ := x.read(context.Background(), &s, run); d != int64(1) {
		t.Fatalf("first read: %v", d)
	}
	clk.Add(DiscoveryTTL)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if d, err := x.read(ctx, &s, run); d != int64(1) || err != nil {
				t.Errorf("reader past the TTL: %v %v", d, err)
			}
		}()
	}
	wg.Wait()
	if runs.Load() != 2 {
		t.Errorf("%d scans for five readers past the TTL", runs.Load())
	}
	close(release)
	for i := 0; ; i++ {
		if d, _ := x.read(context.Background(), &s, run); d == int64(2) {
			break
		}
		if i > 200 {
			t.Fatal("the new scan never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.gen != 2 {
		t.Errorf("generation %d after two scans", s.gen)
	}
}

// A new file appears within the TTL (Claude at once, Codex after the next
// scan), an append updates counts, and a deleted file disappears: the
// scan drops what it did not see. A rollout already seen at its inode is
// not opened again.
func TestIndexFreshness(t *testing.T) {
	r, work := listFixture(t)
	x, clk := testIndex(r)
	c := NewCache()
	list := func() map[string]Summary {
		t.Helper()
		res, err := ListWith(context.Background(), x, work, c.Summary)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]Summary{}
		for _, s := range res.Rows {
			out[s.Provider+"/"+s.ID] = s
		}
		return out
	}
	list()
	enc := nonAlnum.ReplaceAllString(work, "-")
	claudeNew := filepath.Join(r.ClaudeProjects[0], enc, idA+".jsonl")
	writeLines(t, claudeNew, strings.ReplaceAll(strings.Join(claudeFixture(work), "\n"), `"s1"`, `"`+idA+`"`))
	day := filepath.Join(r.CodexSessions[0], "2026", "09", "16")
	codexNew := filepath.Join(day, "rollout-new.jsonl")
	writeLines(t, codexNew, codexFixture(work, idB)...)
	if got := list(); got["claude/"+idA].ID == "" || got["codex/"+idB].ID != "" {
		t.Errorf("within the TTL: claude new %v, codex new %v", got["claude/"+idA].ID, got["codex/"+idB].ID)
	}
	clk.Add(DiscoveryTTL)
	list()
	waitWalk(t, x, 2)
	got := list()
	if got["codex/"+idB].ID == "" {
		t.Error("a new rollout did not appear after the next scan")
	}
	turns := got["codex/"+idB].Turns

	appendLines(t, codexNew, `{"timestamp":"2026-09-16T21:30:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"and again"}]}}`)
	if got := list(); got["codex/"+idB].Turns != turns+1 {
		t.Errorf("after an append: %d turns, want %d", got["codex/"+idB].Turns, turns+1)
	}

	os.Remove(codexNew)
	os.Remove(claudeNew)
	clk.Add(DiscoveryTTL)
	list()
	waitWalk(t, x, 3)
	if got := list(); got["codex/"+idB].ID != "" || got["claude/"+idA].ID != "" {
		t.Error("a deleted transcript is still listed")
	}
	cs, _ := x.codexScan(context.Background())
	if _, ok := cs.seen[codexNew]; ok {
		t.Error("the scan kept a path it did not see")
	}

	// Rewritten in place, a rollout keeps its inode: the scan does not
	// read its head again.
	a := filepath.Join(r.CodexSessions[0], "2026", "09", "15", "rollout-a.jsonl")
	if cs.seen[a].ino == 0 {
		t.Skip("no inodes here")
	}
	f, _ := os.OpenFile(a, os.O_WRONLY|os.O_TRUNC, 0)
	f.WriteString(strings.Join(codexFixture("/elsewhere", "c1"), "\n") + "\n")
	f.Close()
	clk.Add(DiscoveryTTL)
	list()
	waitWalk(t, x, 4)
	cs, _ = x.codexScan(context.Background())
	if cs.seen[a].cwd != work {
		t.Errorf("a rollout seen at its inode was read again: cwd %q", cs.seen[a].cwd)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(strings.Join(lines, "\n") + "\n")
}

// ListDirs stops at its row cap and its time, and says so.
func TestListDirsBudget(t *testing.T) {
	r, work := listFixture(t)
	res, err := ListDirs(context.Background(), DirectWalk(r), []string{work, work}, ParseSummary)
	if err != nil || res.Partial || len(res.Rows) != 12 {
		t.Fatalf("two lists: %d rows, partial %v, %v", len(res.Rows), res.Partial, err)
	}
	for i := 1; i < len(res.Rows); i++ {
		if res.Rows[i].Updated.After(res.Rows[i-1].Updated) {
			t.Fatal("rows not newest first")
		}
	}
	many := make([]string, ListRows/6+2)
	for i := range many {
		many[i] = work
	}
	res, _ = ListDirs(context.Background(), DirectWalk(r), many, ParseSummary)
	if !res.Partial || len(res.Rows) != ListRows {
		t.Errorf("over the row cap: %d rows, partial %v", len(res.Rows), res.Partial)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, _ = ListDirs(ctx, DirectWalk(r), []string{work}, ParseSummary)
	if !res.Partial || len(res.Rows) != 0 {
		t.Errorf("out of time: %d rows, partial %v", len(res.Rows), res.Partial)
	}
}

// With no scan completed, a reader waits for the first only as long as its
// context lets it: a cancelled reader returns at once, and the scan carries
// on and answers the readers after it.
func TestIndexFirstScanUnderTheReadersContext(t *testing.T) {
	x, _ := testIndex(Roots{})
	release := make(chan struct{})
	var runs atomic.Int64
	run := func(any) any { runs.Add(1); <-release; return "first" }
	var s scan
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := x.read(ctx, &s, run); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("cancelled reader: %v after %s", err, time.Since(start))
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := x.read(ctx, &s, run); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("reader past its deadline: %v", err)
	}
	close(release)
	if d, err := x.read(context.Background(), &s, run); d != "first" || err != nil {
		t.Errorf("a later reader: %v %v", d, err)
	}
	if runs.Load() != 1 {
		t.Errorf("%d scans, want the one the cancelled reader started", runs.Load())
	}
}
