//go:build unix

package main

// The navigator's library-level benchmark and soak: the caches, the
// discovery index, ListWith and the terminal printer as the view builder,
// driven in process, with no HTTP. Both are off unless asked for:
//
//	AIQ_TEST_NAV_FIGURES=fixture   figures on generated transcripts
//	AIQ_TEST_NAV_FIGURES=real      figures on a real workload, read only:
//	    AIQ_TEST_NAV_HOME=<home whose .claude, .codex and .gemini to read>
//	    AIQ_TEST_NAV_DATA=<directory holding a copy of state.db, made with
//	        sqlite3 state.db ".backup <dir>/state.db" or VACUUM INTO, never a
//	        plain file copy of a WAL database; opened read only>
//	AIQ_TEST_NAV_SOAK=10m          the soak, on generated transcripts
//	AIQ_TEST_NAV_BIG_MB=40         the size of the largest generated transcript
//
// Run with -v -timeout above the soak's length, for example
//
//	AIQ_TEST_NAV_SOAK=10m go test -v -timeout 20m -run TestNavSoak ./cmd/aiq
//
// Figures, never transcript contents, go in the pull request.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// navView builds a full-window view the way the web view will, with
// today's printer standing in for 2b's producers: the window's text and
// the printer's message list, hashed for the ETag.
func navView(s *transcript.Session, last int, markup bool) *transcript.View {
	from := convoStart(s.Turns, last)
	p := newConvoPrinter(s.Provider, true, false)
	p.width, p.markup, p.track = 100, markup, true
	p.skip(s.Turns[:from])
	body := p.emit(s)
	n := int64(len(body))
	for _, c := range p.cuts {
		n += int64(64 + len(c.key) + len(c.kind) + len(c.text))
	}
	sum := sha256.Sum256([]byte(body))
	return &transcript.View{Value: body, ETag: hex.EncodeToString(sum[:16]), Bytes: n, Settled: transcript.Settled(s, p.pending)}
}

// navFixture writes an invented workload under root: Claude sessions in a
// few project directories, one of them bigMB large, Codex rollouts (most of
// them only a head, in other directories), and Antigravity conversations.
// It returns the roots and the directory most sessions ran in.
func navFixture(t testing.TB, root string, bigMB int) (transcript.Roots, string) {
	t.Helper()
	work := filepath.Join(root, "work", "app")
	dirs := []string{work, filepath.Join(root, "work", "lib"), filepath.Join(root, "work", "docs")}
	for _, d := range dirs {
		os.MkdirAll(d, 0o700)
	}
	claudeHome, codexHome := filepath.Join(root, "claude"), filepath.Join(root, "codex")
	appData := agy.AppDataDir(filepath.Join(root, "gemini"))
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 240; i++ {
		dir := dirs[i%len(dirs)]
		turns := 2 + rng.Intn(30)
		if i == 0 {
			turns = 0 // sized below
		}
		id := fixtureID(1, i)
		path := filepath.Join(claudeHome, "projects", nonAlnumPath(dir), id+".jsonl")
		writeClaudeFixture(t, path, dir, id, turns, bigMB<<20, rng)
	}
	var history []string
	for i := 0; i < 5000; i++ {
		id := fixtureID(2, i)
		day := filepath.Join(codexHome, "sessions", "2026", "09", fmt.Sprintf("%02d", 1+i%28))
		path := filepath.Join(day, "rollout-2026-09-01T10-00-00-"+id+".jsonl")
		if i%30 == 0 {
			writeCodexFixture(t, path, dirs[i%len(dirs)], id, 2+rng.Intn(12), rng)
		} else {
			writeLinesTB(t, path, codexHead(filepath.Join(root, "elsewhere", strconv.Itoa(i%400)), id))
		}
	}
	for i := 0; i < 20; i++ {
		id := fixtureID(3, i)
		writeLinesTB(t, transcript.AgyTranscript(appData, id),
			`{"type":"USER_INPUT","content":"<USER_REQUEST>`+fixtureText(rng, 20)+`</USER_REQUEST>"}`,
			`{"type":"PLANNER_RESPONSE","content":"`+fixtureText(rng, 60)+`"}`)
		line, _ := json.Marshal(map[string]any{"display": "x", "timestamp": 1, "workspace": dirs[i%len(dirs)], "conversationId": id})
		history = append(history, string(line))
	}
	writeLinesTB(t, filepath.Join(appData, "history.jsonl"), history...)
	return transcript.DefaultRoots(claudeHome, codexHome, appData, ""), canonicalPath(work)
}

func fixtureID(kind, i int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012x", kind, i) }

func nonAlnumPath(dir string) string {
	var b strings.Builder
	for _, r := range canonicalPath(dir) {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

var fixtureWords = strings.Fields("the cache keeps a parse of each file and the view built from it so a poll that finds nothing new " +
	"answers from a stat while a list reads rows from the summaries and the index walks rollouts once per window")

func fixtureText(rng *rand.Rand, words int) string {
	w := make([]string, words)
	for i := range w {
		w[i] = fixtureWords[rng.Intn(len(fixtureWords))]
	}
	return strings.Join(w, " ")
}

// fixtureReply is an answer with the Markdown the printer renders.
func fixtureReply(rng *rand.Rand, words int) string {
	return "## Summary\n\n" + fixtureText(rng, words) + "\n\n- " + fixtureText(rng, 12) + "\n- " + fixtureText(rng, 9) +
		"\n\n| step | result |\n| --- | --- |\n| build | ok |\n| test | ok |\n\n```go\nfunc f() error { return nil }\n```\n\n> " + fixtureText(rng, 10)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// claudeTurn is one turn's records: a typed prompt, a said line with a
// tool call and its result, and a final answer.
func claudeTurn(dir, id string, at time.Time, n int, rng *rand.Rand, tool string) []string {
	c := `"cwd":` + jsonString(dir) + `,"sessionId":"` + id + `"`
	ts := func(s int) string { return at.Add(time.Duration(s) * time.Second).UTC().Format(time.RFC3339) }
	msg := fmt.Sprintf("m%d", n)
	return []string{
		`{"type":"user",` + c + `,"timestamp":"` + ts(0) + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":` + jsonString(fixtureText(rng, 25)) + `}}`,
		`{"type":"assistant",` + c + `,"timestamp":"` + ts(2) + `","message":{"id":"` + msg + `a","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":` + jsonString(fixtureText(rng, 15)) + `}]}}`,
		`{"type":"assistant",` + c + `,"timestamp":"` + ts(3) + `","message":{"id":"` + msg + `a","model":"claude-opus-5","role":"assistant","content":[{"type":"tool_use","id":"t` + msg + `","name":"Bash","input":{}}]}}`,
		`{"type":"user",` + c + `,"timestamp":"` + ts(4) + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t` + msg + `","content":` + jsonString(tool) + `}]}}`,
		`{"type":"assistant",` + c + `,"timestamp":"` + ts(30) + `","message":{"id":"` + msg + `b","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":` + jsonString(fixtureReply(rng, 80+rng.Intn(200))) + `}]}}`,
	}
}

// writeClaudeFixture writes a session of turns turns, or with turns 0 one
// of about size bytes.
func writeClaudeFixture(t testing.TB, path, dir, id string, turns, size int, rng *rand.Rand) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(rng.Intn(1000)) * time.Hour)
	written := 0
	for n := 0; turns == 0 && written < size || n < turns; n++ {
		tool := fixtureText(rng, 40+rng.Intn(400))
		for _, l := range claudeTurn(dir, id, at.Add(time.Duration(n)*time.Minute), n, rng, tool) {
			k, _ := f.WriteString(l + "\n")
			written += k
		}
	}
}

func codexHead(dir, id string) string {
	return `{"timestamp":"2026-09-01T10:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":` + jsonString(dir) + `,"originator":"codex-tui","source":"cli"}}`
}

func writeCodexFixture(t testing.TB, path, dir, id string, turns int, rng *rand.Rand) {
	lines := []string{codexHead(dir, id),
		`{"timestamp":"2026-09-01T10:00:01Z","type":"turn_context","payload":{"cwd":` + jsonString(dir) + `,"model":"gpt-6"}}`}
	for n := 0; n < turns; n++ {
		ts := time.Date(2026, 9, 1, 10, n, 0, 0, time.UTC).Format(time.RFC3339)
		turn := fmt.Sprint("t", n)
		lines = append(lines,
			`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"task_started","turn_id":"`+turn+`"}}`,
			`{"timestamp":"`+ts+`","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":`+jsonString(fixtureText(rng, 20))+`}]}}`,
			`{"timestamp":"`+ts+`","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":`+jsonString(fixtureReply(rng, 120))+`}]}}`,
			`{"timestamp":"`+ts+`","type":"event_msg","payload":{"type":"task_complete","turn_id":"`+turn+`"}}`)
	}
	writeLinesTB(t, path, lines...)
}

func writeLinesTB(t testing.TB, path string, lines ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func bigMB() int {
	if n, err := strconv.Atoi(os.Getenv("AIQ_TEST_NAV_BIG_MB")); err == nil && n > 0 {
		return n
	}
	return 40
}

// procUsage is the process's resident memory (Linux: now; elsewhere: its peak)
// and CPU time so far.
func procUsage() (rssMB float64, cpu time.Duration) {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	if data, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
				kb, _ := strconv.ParseFloat(f[1], 64)
				return kb / 1024, cpu
			}
		}
	}
	return float64(ru.Maxrss) / (1 << 20), cpu
}

// TestNavFigures records the figures the caches' bounds are to be set from.
func TestNavFigures(t *testing.T) {
	mode := os.Getenv("AIQ_TEST_NAV_FIGURES")
	var roots transcript.Roots
	var dirs []string
	switch mode {
	case "":
		t.Skip("set AIQ_TEST_NAV_FIGURES=fixture, or real with AIQ_TEST_NAV_HOME and AIQ_TEST_NAV_DATA")
	case "fixture":
		var work string
		roots, work = navFixture(t, t.TempDir(), bigMB())
		dirs = []string{work}
		t.Log("figures: library-level, generated fixture data")
	case "real":
		home, data := os.Getenv("AIQ_TEST_NAV_HOME"), os.Getenv("AIQ_TEST_NAV_DATA")
		if home == "" || data == "" {
			t.Fatal("real figures need AIQ_TEST_NAV_HOME and AIQ_TEST_NAV_DATA")
		}
		roots = transcript.DefaultRoots(filepath.Join(home, ".claude"), filepath.Join(home, ".codex"),
			agy.AppDataDir(filepath.Join(home, ".gemini")), "", filepath.Join(data, "claude"), filepath.Join(data, "codex"))
		st, err := state.OpenReadOnly(filepath.Join(data, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ds, err := st.Directories(hostname())
		st.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("directories from state.db: %d in %s", len(ds), time.Since(start).Round(time.Millisecond))
		for _, d := range ds {
			dirs = append(dirs, d.Path)
		}
		t.Log("figures: library-level, the owner's workload")
	default:
		t.Fatalf("AIQ_TEST_NAV_FIGURES=%q: want fixture or real", mode)
	}

	files, total, largest, largestSize := transcriptFiles(roots)
	t.Logf("transcripts: %d files, %.1f MB in all; largest %.1f MB", files, float64(total)/(1<<20), float64(largestSize)/(1<<20))

	x := transcript.NewIndex(roots)
	start := time.Now()
	x.Written(context.Background(), "codex", "/", time.Time{})
	t.Logf("index build (first Codex walk, every head read): %s", time.Since(start).Round(time.Millisecond))
	start = time.Now()
	transcript.NewIndex(roots).Written(context.Background(), "codex", "/", time.Time{})
	t.Logf("Codex walk, warm file cache, new index: %s", time.Since(start).Round(time.Millisecond))

	c := transcript.NewCache()
	if len(dirs) > 0 {
		start = time.Now()
		res, err := transcript.ListDirs(context.Background(), x, dirs[:1], c.Summary)
		t.Logf("first list of the most recent directory: %d rows, %d of %d scanned, partial %v, %s, %v",
			len(res.Rows), res.Scanned, res.Of, res.Partial, time.Since(start).Round(time.Millisecond), err)
		before := c.Stats().Parses
		start = time.Now()
		transcript.ListDirs(context.Background(), x, dirs[:1], c.Summary)
		t.Logf("second list: %d parses, %s", c.Stats().Parses-before, time.Since(start).Round(time.Millisecond))
	}

	if largest == "" {
		return
	}
	var ms0, ms1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms0)
	start = time.Now()
	s, err := transcript.ParseFile(largest)
	took := time.Since(start)
	runtime.ReadMemStats(&ms1)
	if err != nil || s == nil {
		t.Fatalf("largest transcript: %v", err)
	}
	runtime.GC()
	var ms2 runtime.MemStats
	runtime.ReadMemStats(&ms2)
	t.Logf("parse of the largest: %s, %.1f MB allocated, about %.1f MB retained, %d turns",
		took.Round(time.Millisecond), float64(ms1.TotalAlloc-ms0.TotalAlloc)/(1<<20), float64(int64(ms2.HeapAlloc)-int64(ms0.HeapAlloc))/(1<<20), len(s.Turns))
	for _, last := range []int{50, 10000} {
		start = time.Now()
		v := navView(s, last, true)
		t.Logf("view build of the largest, last=%d: %s, %.1f MB estimated", last, time.Since(start).Round(time.Millisecond), float64(v.Bytes)/(1<<20))
	}
	runtime.KeepAlive(s)
}

// transcriptFiles counts the transcripts under roots and finds the largest.
func transcriptFiles(r transcript.Roots) (n int, total int64, largest string, size int64) {
	var all []string
	for _, root := range append(append([]string{}, r.ClaudeProjects...), r.CodexSessions...) {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
				all = append(all, p)
			}
			return nil
		})
	}
	for _, app := range r.AgyAppData {
		m, _ := filepath.Glob(transcript.AgyTranscript(app, "*"))
		all = append(all, m...)
	}
	for _, p := range all {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		n++
		total += fi.Size()
		if fi.Size() > size {
			largest, size = p, fi.Size()
		}
	}
	return n, total, largest, size
}

// TestNavSoak drives the caches for a while as three clients would: one
// lists the directory every 15 s, one follows a session appended every
// second, polling every 2 s with its last ETag, and one opens a session
// every 2 s, every fifth time the largest one shown in full.
func TestNavSoak(t *testing.T) {
	d, err := time.ParseDuration(os.Getenv("AIQ_TEST_NAV_SOAK"))
	if err != nil || d <= 0 {
		t.Skip("set AIQ_TEST_NAV_SOAK to a duration, such as 10m")
	}
	t.Log("soak: library-level, generated fixture data")
	roots, work := navFixture(t, t.TempDir(), bigMB())
	_, _, largest, _ := transcriptFiles(roots)
	followed := filepath.Join(roots.ClaudeProjects[0], nonAlnumPath(work), fixtureID(9, 0)+".jsonl")
	rng := rand.New(rand.NewSource(2))
	writeClaudeFixture(t, followed, work, fixtureID(9, 0), 3, 0, rng)

	x, c := transcript.NewIndex(roots), transcript.NewCache()
	runtime.GC()
	rss0, cpu0 := procUsage()
	t.Logf("start: RSS %.1f MB, CPU %s", rss0, cpu0.Round(time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()

	var (
		mu                  sync.Mutex
		paths               []string
		lists, partials     atomic.Int64
		polls, notModified  atomic.Int64
		opens, preparing    atomic.Int64
		slowest             atomic.Int64 // the longest detail wait, ns
		maxViews, maxParsed atomic.Int64
		fails               []string
	)
	fail := func(format string, args ...any) {
		mu.Lock()
		if len(fails) < 20 {
			fails = append(fails, fmt.Sprintf(format, args...))
		}
		mu.Unlock()
	}
	detail := func(path string, last int, etag, fromKey string) (string, bool) {
		snap, err := transcript.Stat(path)
		if err != nil {
			fail("stat: %v", err)
			return "", false
		}
		k := transcript.ViewKey{Snap: snap, Last: last, Markup: true}
		if etag != "" && c.NotModified(k, fromKey, etag) {
			notModified.Add(1)
			return etag, true
		}
		start := time.Now()
		s, got, err := c.Session(ctx, path)
		if w := int64(time.Since(start)); w > slowest.Load() {
			slowest.Store(w)
		}
		switch {
		case errors.Is(err, transcript.ErrPreparing):
			preparing.Add(1)
			return "", false
		case ctx.Err() != nil:
			return "", false
		case err != nil:
			fail("session %s: %v", filepath.Base(path), err)
			return "", false
		}
		k.Snap = got
		v, err := c.View(k, func() (*transcript.View, error) { return navView(s, last, true), nil })
		if err != nil {
			fail("view: %v", err)
			return "", false
		}
		if fromKey != "" {
			c.NoteDelta(k, fromKey, v.ETag+"/"+fromKey)
			return v.ETag + "/" + fromKey, false
		}
		return v.ETag, false
	}
	watch := func() {
		st := c.Stats()
		if int64(st.Views) > maxViews.Load() {
			maxViews.Store(int64(st.Views))
		}
		if int64(st.Parsed) > maxParsed.Load() {
			maxParsed.Store(int64(st.Parsed))
		}
		if st.Views > 64 || st.ViewBytes > 16<<20 || st.Parsed > 32 || st.Rows > 4096 {
			fail("bounds: %+v", st)
		}
	}
	every := func(wg *sync.WaitGroup, gap time.Duration, f func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ctx.Err() == nil; i++ {
				f(i)
				watch()
				select {
				case <-ctx.Done():
				case <-time.After(gap):
				}
			}
		}()
	}
	var wg sync.WaitGroup
	every(&wg, time.Second, func(i int) {
		at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		f, err := os.OpenFile(followed, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			fail("append: %v", err)
			return
		}
		for _, l := range claudeTurn(work, fixtureID(9, 0), at, 1000+i, rng, fixtureText(rng, 50)) {
			f.WriteString(l + "\n")
		}
		f.Close()
	})
	every(&wg, 15*time.Second, func(int) {
		res, err := transcript.ListDirs(ctx, x, []string{work}, c.Summary)
		if err != nil {
			if ctx.Err() == nil {
				fail("list: %v", err)
			}
			return
		}
		lists.Add(1)
		if res.Partial {
			partials.Add(1)
		}
		mu.Lock()
		paths = paths[:0]
		for _, r := range res.Rows {
			paths = append(paths, r.Path)
		}
		mu.Unlock()
	})
	var etag string
	every(&wg, 2*time.Second, func(i int) {
		polls.Add(1)
		etag, _ = detail(followed, 50, etag, "k")
	})
	pick := rand.New(rand.NewSource(3))
	every(&wg, 2*time.Second, func(i int) {
		opens.Add(1)
		if i%5 == 4 {
			detail(largest, 10000, "", "")
			return
		}
		mu.Lock()
		var p string
		if len(paths) > 0 {
			p = paths[pick.Intn(len(paths))]
		}
		mu.Unlock()
		if p != "" {
			detail(p, 50, "", "")
		}
	})
	wg.Wait()

	runtime.GC()
	rss1, cpu1 := procUsage()
	st := c.Stats()
	t.Logf("end: RSS %.1f MB, CPU %s (%s over the soak)", rss1, cpu1.Round(time.Millisecond), (cpu1 - cpu0).Round(time.Millisecond))
	t.Logf("lists %d (%d partial), follower polls %d (%d answered from a stat), opens %d, preparing %d, slowest detail wait %s",
		lists.Load(), partials.Load(), polls.Load(), notModified.Load(), opens.Load(), preparing.Load(), time.Duration(slowest.Load()).Round(time.Millisecond))
	t.Logf("parses %d, oversized parses held %d, views built %d (%d oversized), mean view build %s, Codex walks %d",
		st.Parses, st.Holds, st.ViewBuilds, st.OversizedViews, meanDuration(st.ViewTime, st.ViewBuilds), x.Walks())
	t.Logf("cache at the end: %d parses (%.1f MB), %d rows, %d views (%.1f MB); most seen: %d parses, %d views",
		st.Parsed, float64(st.ParsedBytes)/(1<<20), st.Rows, st.Views, float64(st.ViewBytes)/(1<<20), maxParsed.Load(), maxViews.Load())
	sort.Strings(fails)
	for _, f := range fails {
		t.Error(f)
	}
	if lists.Load() == 0 || polls.Load() == 0 {
		t.Error("a client never ran")
	}
}

func meanDuration(total time.Duration, n int64) time.Duration {
	if n == 0 {
		return 0
	}
	return (total / time.Duration(n)).Round(time.Microsecond)
}
