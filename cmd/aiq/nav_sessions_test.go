package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/orlenko/aiq/internal/transcript"
)

const navTestID = "12345678-1234-4234-8234-123456789abc"

type navTestSource struct {
	transcript.Discovery // Unused methods must never be called by this route.
	candidates           func(context.Context, string) ([]transcript.Candidate, error)
	names                func(context.Context) map[string]string
}

func (s navTestSource) Candidates(ctx context.Context, dir string) ([]transcript.Candidate, error) {
	return s.candidates(ctx, dir)
}
func (s navTestSource) CodexNames(ctx context.Context) map[string]string {
	if s.names != nil {
		return s.names(ctx)
	}
	return nil
}

func navTestHandler(t *testing.T, rows []transcript.Summary, change func(*navSessionsDeps, *navSessionsLimits)) *navSessionsHandler {
	t.Helper()
	cands := make([]transcript.Candidate, len(rows))
	byPath := make(map[string]transcript.Summary, len(rows))
	for i, row := range rows {
		path := fmt.Sprintf("fixture-%d", i)
		cands[i] = transcript.Candidate{Provider: row.Provider, Path: path}
		byPath[path] = row
	}
	deps := navSessionsDeps{
		source:  navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) { return cands, nil }},
		summary: func(_ context.Context, path string) (transcript.Summary, error) { return byPath[path], nil },
	}
	limits := navSessionsLimits{rowBytes: 4 << 20, bodyBytes: 4 << 20, builders: 1}
	if change != nil {
		change(&deps, &limits)
	}
	h, err := newNavSessionsHandler(deps, limits)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type navTestWriter struct {
	header         http.Header
	status, writes int
	body           []byte
	onWrite        func()
	writeErr       error
	writePanic     bool
}

func (w *navTestWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *navTestWriter) WriteHeader(status int) {
	if w.status != 0 {
		panic("second status")
	}
	w.status = status
}
func (w *navTestWriter) Write(body []byte) (int, error) {
	w.writes++
	if w.onWrite != nil {
		w.onWrite()
	}
	if w.writePanic {
		panic("invented writer fault")
	}
	w.body = append(w.body, body...)
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(body), nil
}
func navTestRequest(dir string) *http.Request {
	return httptest.NewRequest("GET", "/api/nav/sessions?dir="+url.QueryEscape(dir), nil)
}
func navTestServe(h http.Handler, r *http.Request) *navTestWriter {
	w := &navTestWriter{}
	h.ServeHTTP(w, r)
	return w
}
func navTestResponse(t *testing.T, w *navTestWriter, status int, body string) {
	t.Helper()
	if w.status != status || w.writes != 1 || string(w.body) != body {
		t.Fatalf("response %d writes=%d %q; want %d %q", w.status, w.writes, w.body, status, body)
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing response headers")
	}
}
func navTestDecode(t *testing.T, w *navTestWriter) map[string]any {
	t.Helper()
	if w.status != 200 || w.writes != 1 {
		t.Fatalf("response %d writes=%d %s", w.status, w.writes, w.body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func navTestRows(t *testing.T, out map[string]any) []any {
	t.Helper()
	rows, ok := out["sessions"].([]any)
	if !ok {
		t.Fatal("sessions is not an array")
	}
	return rows
}

type navTestUnreadBody struct{}

func (navTestUnreadBody) Read([]byte) (int, error) { panic("body must not be read") }
func (navTestUnreadBody) Close() error             { return nil }

func TestNavSessionsValidationBeforeIO(t *testing.T) {
	var calls atomic.Int32
	h := navTestHandler(t, nil, func(d *navSessionsDeps, _ *navSessionsLimits) {
		d.source = navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) { calls.Add(1); return nil, nil }}
	})
	for _, tc := range []struct{ name, query string }{
		{"missing", ""}, {"empty", "dir="}, {"relative", "dir=relative"}, {"all", "dir=all"},
		{"duplicate", "dir=/a&dir=/b"}, {"encoded duplicate", "dir=/a&%64ir=/b"}, {"unknown", "dir=/a&x=1"},
		{"duplicate workers", "dir=/a&workers=0&workers=1"}, {"empty workers", "dir=/a&workers="}, {"workers long", "dir=/a&workers=01"}, {"workers invalid", "dir=/a&workers=2"},
		{"semicolon", "dir=/a;workers=1"}, {"bad escape", "dir=%GG"}, {"short escape", "dir=%"},
		{"invalid UTF8", "dir=/%ff"}, {"nul", "dir=/a%00b"}, {"directory over", "dir=/" + strings.Repeat("a", 4096)},
		{"raw over", strings.Repeat("x", 12353)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := navTestRequest("/unused")
			r.URL.RawQuery = tc.query
			r.Body = navTestUnreadBody{}
			navTestResponse(t, navTestServe(h, r), 400, `{"error":"invalid_request"}`)
		})
	}
	for _, modify := range []func(*http.Request){
		func(r *http.Request) { r.URL.RawPath = "/api/nav/sessions" },
		func(r *http.Request) { r.URL.Path = "/api/nav/%73essions" },
		func(r *http.Request) { r.RequestURI = "/api/nav/%73essions?dir=/a" },
		func(r *http.Request) { r.URL.Fragment = "fragment" },
		func(r *http.Request) { r.URL.RawQuery = ""; r.URL.ForceQuery = true },
	} {
		r := navTestRequest("/a")
		modify(r)
		navTestResponse(t, navTestServe(h, r), 400, `{"error":"invalid_request"}`)
	}
	navTestResponse(t, navTestServe(h, httptest.NewRequest("GET", "/api/nav/other", nil)), 404, `{"error":"not_found"}`)
	r := navTestRequest("/a")
	r.Method = "POST"
	w := navTestServe(h, r)
	navTestResponse(t, w, 405, `{"error":"method_not_allowed"}`)
	if w.Header().Get("Allow") != "GET" {
		t.Fatal("missing Allow")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached discovery")
	}
	for _, dir := range []string{"/" + strings.Repeat("a", 4095), "/fixture/ space\t\x1b日本語/../kept ", "/" + strings.Repeat("日", 1365)} {
		r := navTestRequest(dir)
		r.Body = navTestUnreadBody{}
		out := navTestDecode(t, navTestServe(h, r))
		if out["dir"] != filepath.Clean(dir) {
			t.Fatal("directory identity sanitized")
		}
	}
	// Every directory byte and every key/value is percent-encoded, still valid.
	r = navTestRequest("/unused")
	r.URL.RawQuery = "%64%69%72=%2f" + strings.Repeat("%61", 4095) + "&%77%6f%72%6b%65%72%73=%31"
	navTestDecode(t, navTestServe(h, r))
}

func TestNavSessionsLiteralDTOAndOwnership(t *testing.T) {
	label := "<script>\"&\n\x1b日本語\u2028" + strings.Repeat("長", 200)
	row := transcript.Summary{Provider: "claude", ID: navTestID, Path: "NEVER-EXPORT-TRANSCRIPT", Cwd: "/fixture", Label: label, Title: "title", Model: "model", Branch: "branch", Started: time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("fixture", 3600)), Turns: 2, Tools: 3, Bypass: true}
	roots := transcript.Roots{ClaudeProjects: []string{"/configured/claude"}, CodexSessions: []string{"/configured/codex"}, AgyAppData: []string{"/configured/agy"}, CodexIndex: []string{"/not-coverage/index"}, CopilotAppData: []string{"/unsupported/copilot"}, Labels: map[string]string{"/configured/claude": "NOT-AN-ACCOUNT-FACT"}}
	h := navTestHandler(t, []transcript.Summary{row}, func(d *navSessionsDeps, _ *navSessionsLimits) { d.roots = roots })
	roots.ClaudeProjects[0] = "/mutated"
	roots.CodexSessions[0] = "/mutated"
	roots.AgyAppData[0] = "/mutated"
	roots.Labels["/configured/claude"] = "changed"
	w := navTestServe(h, navTestRequest("/fixture"))
	out := navTestDecode(t, w)
	want := map[string]any{"dir": "/fixture", "roots_searched": []any{"/configured/claude", "/configured/codex", "/configured/agy"}, "partial": false, "scanned": float64(1), "of": float64(1), "unreadable_count": float64(0), "sessions": []any{map[string]any{"provider": "claude", "id": navTestID, "cwd": "/fixture", "label": label, "title": "title", "model": "model", "branch": "branch", "started": "2026-01-02T02:04:05.000000006Z", "updated": "0001-01-01T00:00:00Z", "turns": float64(2), "tools": float64(3), "worker": false, "bypass": true, "commands": []any{map[string]any{"verb": "convo", "fresh_only": false}, map[string]any{"verb": "convo_follow", "fresh_only": false}}}}}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("literal DTO mismatch\ngot %#v\nwant %#v", out, want)
	}
	for _, forbidden := range []string{"NEVER-EXPORT-TRANSCRIPT", "NOT-AN-ACCOUNT-FACT", "/mutated", "/not-coverage", "/unsupported", "<script>"} {
		if strings.Contains(string(w.body), forbidden) {
			t.Fatal("private/non-JSON data exported")
		}
	}
	empty := navTestDecode(t, navTestServe(navTestHandler(t, nil, nil), navTestRequest("/fixture")))
	if !reflect.DeepEqual(empty["roots_searched"], []any{}) || len(navTestRows(t, empty)) != 0 {
		t.Fatal("nil arrays exported")
	}
	for _, tc := range []struct {
		provider, id string
		verbs        []string
	}{{"codex", navTestID, []string{"convo", "convo_follow"}}, {"agy", navTestID, []string{"convo"}}, {"claude", "short", nil}, {"unknown", navTestID, nil}} {
		out := navTestDecode(t, navTestServe(navTestHandler(t, []transcript.Summary{{Provider: tc.provider, ID: tc.id}}, nil), navTestRequest("/fixture")))
		commands := navTestRows(t, out)[0].(map[string]any)["commands"].([]any)
		if len(commands) != len(tc.verbs) {
			t.Fatal("command eligibility")
		}
		for i, verb := range tc.verbs {
			if !reflect.DeepEqual(commands[i], map[string]any{"verb": verb, "fresh_only": false}) {
				t.Fatal("command shape")
			}
		}
	}
}

func TestNavSessionsSelectionBeforeCap(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []transcript.Summary{{Provider: "claude", ID: "duplicate", Label: "old human", Updated: at}, {Provider: "claude", ID: "duplicate", Label: "new worker", Worker: true, Updated: at.Add(time.Hour)}, {Provider: "codex", ID: "tie", Label: "codex", Updated: at}, {Provider: "claude", ID: "tie", Label: "first", Updated: at}, {Provider: "claude", ID: "tie", Label: "second", Updated: at}, {Provider: "agy", ID: "tie", Updated: at}, {Provider: "claude", ID: "wrongcwd", Cwd: "/other"}}
	for i := 0; i < 501; i++ {
		rows = append(rows, transcript.Summary{Provider: "claude", ID: fmt.Sprintf("excluded-%03d", i), Empty: true, Updated: at.Add(2 * time.Hour)})
	}
	h := navTestHandler(t, rows, nil)
	out := navTestDecode(t, navTestServe(h, navTestRequest("/fixture")))
	got := navTestRows(t, out)
	if len(got) != 3 || out["partial"] != false || out["scanned"] != float64(508) || out["of"] != float64(508) {
		t.Fatal("selection/counts", out)
	}
	for i, provider := range []string{"agy", "claude", "codex"} {
		if got[i].(map[string]any)["provider"] != provider {
			t.Fatal("cross-provider tie order")
		}
	}
	if got[1].(map[string]any)["label"] != "first" {
		t.Fatal("equal-updated copy did not retain first source")
	}
	r := navTestRequest("/fixture")
	r.URL.RawQuery += "&workers=1"
	got = navTestRows(t, navTestDecode(t, navTestServe(h, r)))
	if len(got) != 4 || got[0].(map[string]any)["label"] != "new worker" {
		t.Fatal("worker filter resurrected older copy")
	}
	for _, count := range []int{500, 501} {
		rows = make([]transcript.Summary, count)
		for i := range rows {
			rows[i] = transcript.Summary{Provider: "claude", ID: fmt.Sprintf("%04d", count-i), Updated: at}
		}
		out = navTestDecode(t, navTestServe(navTestHandler(t, rows, nil), navTestRequest("/fixture")))
		got = navTestRows(t, out)
		if len(got) != 500 || out["partial"] != (count == 501) {
			t.Fatal("eligible cap")
		}
		if got[0].(map[string]any)["id"] != "0001" || got[499].(map[string]any)["id"] != "0500" {
			t.Fatal("cap before deterministic sort")
		}
	}
}

func TestNavSessionsTextAndBodyBoundaries(t *testing.T) {
	row := transcript.Summary{Provider: "claude", ID: "id", Path: "PRIVATE", Cwd: "/fixture", Label: "label", Title: "title", Model: "model", Branch: "branch"}
	// Independent accounting: 512+8(dir)+16+2(root), then 512 plus
	// 6+2+7+8+5+5+5+6 row bytes. Path is charged despite never being sent.
	const rowFit int64 = 538 + 556
	for _, limit := range []int64{rowFit, rowFit - 1} {
		h := navTestHandler(t, []transcript.Summary{row}, func(d *navSessionsDeps, l *navSessionsLimits) {
			d.roots = transcript.Roots{ClaudeProjects: []string{"/r"}}
			l.rowBytes = limit
		})
		w := navTestServe(h, navTestRequest("/fixture"))
		if limit == rowFit {
			navTestDecode(t, w)
		} else {
			navTestResponse(t, w, 503, `{"error":"list_too_large"}`)
		}
	}
	// A filtered row still consumes its complete charge, and no reservation is
	// refunded for equal identity or a mismatched cwd/provider.
	for _, excluded := range []transcript.Summary{{Provider: "claude", ID: "id", Worker: true}, {Provider: "claude", ID: "id", Empty: true}, {Provider: "claude", ID: "id", Cwd: "/another"}} {
		want := int64(520 + 512 + len(excluded.Provider) + len(excluded.ID) + len(excluded.Cwd))
		h := navTestHandler(t, []transcript.Summary{excluded, excluded}, func(_ *navSessionsDeps, l *navSessionsLimits) { l.rowBytes = want })
		navTestResponse(t, navTestServe(h, navTestRequest("/fixture")), 503, `{"error":"list_too_large"}`)
	}
	// Late names only charge IDs needing a title. The enormous unrelated title
	// is deliberately outside the request's admitted text.
	for _, limit := range []int64{520 + 519 + 267, 520 + 519 + 266} {
		h := navTestHandler(t, []transcript.Summary{{Provider: "codex", ID: "id"}}, func(d *navSessionsDeps, l *navSessionsLimits) {
			l.rowBytes = limit
			source := d.source.(navTestSource)
			source.names = func(context.Context) map[string]string {
				return map[string]string{"id": "new title", "other": strings.Repeat("x", 1<<20)}
			}
			d.source = source
		})
		w := navTestServe(h, navTestRequest("/fixture"))
		if limit == 1306 {
			out := navTestDecode(t, w)
			r := navTestRows(t, out)[0].(map[string]any)
			if r["label"] != "new title" || r["title"] != "new title" {
				t.Fatal("late rename missing")
			}
		} else {
			navTestResponse(t, w, 503, `{"error":"list_too_large"}`)
		}
	}
	const emptyBody = `{"dir":"/fixture","roots_searched":[],"partial":false,"scanned":0,"of":0,"unreadable_count":0,"sessions":[]}`
	for _, size := range []int64{int64(len(emptyBody)), int64(len(emptyBody) - 1)} {
		h := navTestHandler(t, nil, func(_ *navSessionsDeps, l *navSessionsLimits) { l.bodyBytes = size })
		w := navTestServe(h, navTestRequest("/fixture"))
		if size == int64(len(emptyBody)) {
			navTestResponse(t, w, 200, emptyBody)
		} else {
			navTestResponse(t, w, 503, `{"error":"list_too_large"}`)
		}
	}
	// Escaped HTML expansion must count toward B, with no partial response.
	h := navTestHandler(t, []transcript.Summary{{Provider: "claude", ID: "id", Label: strings.Repeat("<", 100)}}, func(_ *navSessionsDeps, l *navSessionsLimits) { l.bodyBytes = 600 })
	navTestResponse(t, navTestServe(h, navTestRequest("/fixture")), 503, `{"error":"list_too_large"}`)
}

func TestNavSessionsAdmittedStorageAndConcurrentReservation(t *testing.T) {
	borrow := func(s string) string {
		return (strings.Repeat("!", 1<<16) + s + strings.Repeat("!", 1<<16))[1<<16 : 1<<16+len(s)]
	}
	in := transcript.Summary{Provider: borrow("codex"), ID: borrow("id"), Path: borrow("path"), Cwd: borrow("cwd"), Label: borrow("label"), Model: borrow("model"), Branch: borrow("branch"), Turns: 9, Tools: 7, Worker: true, Bypass: true, Empty: true, Started: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	in.Title = borrow("title")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &navListRun{parse: func(context.Context, string) (transcript.Summary, error) { return in, nil }, limit: 1 << 20, cancel: cancel, needed: map[string]struct{}{}}
	out, err := run.summary(ctx, "fixture")
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Fatal("owned values changed", err)
	}
	for i, pair := range [][2]string{{in.Provider, out.Provider}, {in.ID, out.ID}, {in.Path, out.Path}, {in.Cwd, out.Cwd}, {in.Label, out.Label}, {in.Title, out.Title}, {in.Model, out.Model}, {in.Branch, out.Branch}} {
		if unsafe.StringData(pair[0]) == unsafe.StringData(pair[1]) {
			t.Fatalf("field %d retains borrowed storage", i)
		}
	}
	in.Title = ""
	out, err = run.summary(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	for id := range run.needed {
		if id != "id" || unsafe.StringData(id) == unsafe.StringData(in.ID) {
			t.Fatal("needed ID retains raw backing")
		}
	}
	title := borrow("late title")
	run.Discovery = navTestSource{names: func(context.Context) map[string]string { return map[string]string{in.ID: title} }}
	names := run.CodexNames(ctx)
	for id, v := range names {
		if id != "id" || v != "late title" || unsafe.StringData(id) == unsafe.StringData(in.ID) || unsafe.StringData(v) == unsafe.StringData(title) {
			t.Fatal("late name ownership")
		}
	}
	// Both callbacks return successfully from the dependency before they race
	// for enough remaining space for exactly one complete row.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	var workers sync.WaitGroup
	defer func() { unblock(); workers.Wait() }()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	run = &navListRun{limit: 514, cancel: cancel2, needed: map[string]struct{}{}, parse: func(context.Context, string) (transcript.Summary, error) {
		entered <- struct{}{}
		<-release
		return transcript.Summary{ID: "id"}, nil
	}}
	type result struct {
		s   transcript.Summary
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); s, e := run.summary(ctx2, "fixture"); results <- result{s, e} }()
	}
	<-entered
	<-entered
	unblock()
	success := 0
	for i := 0; i < 2; i++ {
		v := <-results
		if v.err == nil {
			success++
		} else if v.s != (transcript.Summary{}) {
			t.Fatal("partially admitted row escaped")
		}
	}
	if success != 1 || run.used != 514 || !run.flags().resource {
		t.Fatal("concurrent quota crossing")
	}
	if s, err := run.summary(context.Background(), "later"); err == nil || s != (transcript.Summary{}) {
		t.Fatal("resource failure was not sticky")
	}
}

// A manually expired child permits deterministic deadline transitions inside
// discovery/parse/names without sleeping or leaving a timeout goroutine.
type navTestDeadline struct {
	context.Context
	expired *atomic.Bool
}

func (c navTestDeadline) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}
func navTestChild(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	expired := &atomic.Bool{}
	return navTestDeadline{ctx, expired}, func() { expired.Store(true); cancel() }
}

func TestNavSessionsFailurePrecedenceAndPartial(t *testing.T) {
	for _, mode := range []string{"discovery", "discovery-deadline", "context-only", "summary-panic", "names-panic", "unreadable", "ordinary", "budget", "names-deadline", "parent"} {
		t.Run(mode, func(t *testing.T) {
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			var expire context.CancelFunc
			w := &navTestWriter{}
			h := navTestHandler(t, []transcript.Summary{{Provider: "claude", ID: "id"}}, func(d *navSessionsDeps, _ *navSessionsLimits) {
				d.listContext = func(p context.Context) (context.Context, context.CancelFunc) {
					ctx, cancel := navTestChild(p)
					expire = cancel
					return ctx, cancel
				}
				src := d.source.(navTestSource)
				original := src.candidates
				src.candidates = func(ctx context.Context, dir string) ([]transcript.Candidate, error) {
					if w.status != 0 || w.writes != 0 || len(w.header) != 0 {
						t.Error("writer mutated before build")
					}
					rows, _ := original(ctx, dir)
					switch mode {
					case "discovery":
						return rows, errors.New("PRIVATE dependency detail")
					case "discovery-deadline":
						expire()
						return rows, errors.New("PRIVATE dependency detail")
					case "context-only":
						expire()
						return rows, context.DeadlineExceeded
					}
					return rows, nil
				}
				src.names = func(context.Context) map[string]string {
					if mode == "names-panic" {
						panic("PRIVATE names")
					}
					if mode == "names-deadline" {
						expire()
					}
					return nil
				}
				d.source = src
				d.summary = func(context.Context, string) (transcript.Summary, error) {
					if w.status != 0 || w.writes != 0 || len(w.header) != 0 {
						t.Error("writer mutated before build")
					}
					switch mode {
					case "summary-panic":
						panic("PRIVATE panic")
					case "unreadable":
						return transcript.Summary{}, transcript.ErrUnreadable
					case "ordinary":
						return transcript.Summary{}, errors.New("PRIVATE parse")
					case "budget":
						expire()
						return transcript.Summary{}, transcript.ErrBudget
					case "parent":
						cancelParent()
					}
					return transcript.Summary{Provider: "claude", ID: "id"}, nil
				}
			})
			h.ServeHTTP(w, navTestRequest("/fixture").WithContext(parent))
			switch mode {
			case "discovery", "discovery-deadline", "summary-panic", "names-panic":
				navTestResponse(t, w, 500, `{"error":"internal_error"}`)
			case "parent":
				navTestResponse(t, w, 503, `{"error":"request_cancelled"}`)
			default:
				out := navTestDecode(t, w)
				partial := mode == "context-only" || mode == "budget" || mode == "names-deadline"
				if out["partial"] != partial || out["of"] != float64(1) {
					t.Fatal("interruption metadata", out)
				}
				scanned := float64(1)
				if mode == "context-only" || mode == "budget" {
					scanned = 0
				}
				if out["scanned"] != scanned {
					t.Fatal("scanned counts completed attempts")
				}
				unread := float64(0)
				if mode == "unreadable" {
					unread = 1
				}
				if out["unreadable_count"] != unread {
					t.Fatal("unreadable count")
				}
			}
			if strings.Contains(string(w.body), "PRIVATE") {
				t.Fatal("dependency detail leaked")
			}
		})
	}
}

func TestNavSessionsStickyPanicAndCommitCancellation(t *testing.T) {
	// ListWith caps workers by GOMAXPROCS. This fixture needs three active
	// callbacks even on smaller CI runners; restore the process setting afterward.
	previous := runtime.GOMAXPROCS(3)
	t.Cleanup(func() { runtime.GOMAXPROCS(previous) })
	// All three callbacks are running before the middle one panics. The other
	// two return valid rows only after cancellation acknowledges that fault.
	entered := make(chan struct{}, 3)
	cancelled := make(chan struct{})
	var once sync.Once
	var completed atomic.Int32
	h := navTestHandler(t, []transcript.Summary{{Provider: "claude"}, {Provider: "claude"}, {Provider: "claude"}}, func(d *navSessionsDeps, _ *navSessionsLimits) {
		d.listContext = func(p context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(p)
			return ctx, func() { once.Do(func() { close(cancelled) }); cancel() }
		}
		d.summary = func(_ context.Context, path string) (transcript.Summary, error) {
			entered <- struct{}{}
			if path == "fixture-1" {
				for i := 0; i < 3; i++ {
					<-entered
				}
				panic("PRIVATE middle failure")
			}
			<-cancelled
			completed.Add(1)
			return transcript.Summary{Provider: "claude", ID: path}, nil
		}
	})
	navTestResponse(t, navTestServe(h, navTestRequest("/fixture")), 500, `{"error":"internal_error"}`)
	if completed.Load() != 2 {
		t.Fatal("failure returned before joining later successful callbacks")
	}

	for _, cancelOriginal := range []bool{false, true} {
		parent, cancelParent := context.WithCancel(context.Background())
		w := &navTestWriter{}
		var child context.Context
		h = navTestHandler(t, nil, func(d *navSessionsDeps, _ *navSessionsLimits) {
			d.listContext = func(p context.Context) (context.Context, context.CancelFunc) {
				ctx, cancel := navTestChild(p)
				child = ctx
				return ctx, func() {
					if w.status != 0 || w.writes != 0 || len(w.header) != 0 {
						t.Error("response mutated before complete builder cleanup")
					}
					cancel()
					if cancelOriginal {
						cancelParent()
					}
				}
			}
		})
		h.ServeHTTP(w, navTestRequest("/fixture").WithContext(parent))
		if child.Err() == nil {
			t.Fatal("child did not expire at builder completion")
		}
		if cancelOriginal {
			navTestResponse(t, w, 503, `{"error":"request_cancelled"}`)
		} else if out := navTestDecode(t, w); out["partial"] != false {
			t.Fatal("post-completion child expiry rewrote complete list")
		}
		cancelParent()
	}
}

func TestNavSessionsConcurrentRequestIsolation(t *testing.T) {
	names := map[string]string{"/first/human": "title /first/human", "/first/worker": "title /first/worker", "/second/human": "title /second/human", "/second/worker": "title /second/worker"}
	h := navTestHandler(t, nil, func(d *navSessionsDeps, l *navSessionsLimits) {
		l.builders = 2
		// Each request fits its own two rows/names, but not all four names.
		l.rowBytes = 2300
		d.source = navTestSource{candidates: func(_ context.Context, dir string) ([]transcript.Candidate, error) {
			return []transcript.Candidate{{Provider: "codex", Path: dir + "/human"}, {Provider: "codex", Path: dir + "/worker"}}, nil
		}, names: func(context.Context) map[string]string { return names }}
		d.summary = func(_ context.Context, path string) (transcript.Summary, error) {
			return transcript.Summary{Provider: "codex", ID: path, Cwd: filepath.Dir(path), Worker: strings.HasSuffix(path, "worker")}, nil
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := "/first"
			if i%2 != 0 {
				dir = "/second"
			}
			r := navTestRequest(dir)
			workers := i%3 == 0
			if workers {
				r.URL.RawQuery += "&workers=1"
			}
			out := navTestDecode(t, navTestServe(h, r))
			rows := navTestRows(t, out)
			want := 1
			if workers {
				want = 2
			}
			if out["dir"] != dir || len(rows) != want || out["scanned"] != float64(2) {
				t.Error("request scope or filter contaminated")
				return
			}
			for _, v := range rows {
				row := v.(map[string]any)
				if row["cwd"] != dir || !strings.HasPrefix(row["id"].(string), dir+"/") {
					t.Error("foreign request row")
				}
				if row["title"] != "title "+row["id"].(string) || row["label"] != row["title"] {
					t.Error("request-local name enrichment contaminated")
				}
			}
		}(i)
	}
	wg.Wait()
	if len(names) != 4 || names["/first/human"] != "title /first/human" || names["/second/worker"] != "title /second/worker" {
		t.Fatal("shared source names mutated")
	}
}

func TestNavSessionsAdmissionAndTransportLifetime(t *testing.T) {
	for _, finish := range []string{"success", "error", "panic"} {
		t.Run(finish, func(t *testing.T) {
			var calls atomic.Int32
			childStarted := make(chan context.CancelFunc, 4)
			h := navTestHandler(t, nil, func(d *navSessionsDeps, _ *navSessionsLimits) {
				d.source = navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) { calls.Add(1); return nil, nil }}
				d.listContext = func(p context.Context) (context.Context, context.CancelFunc) {
					ctx, cancel := navTestChild(p)
					childStarted <- cancel
					return ctx, cancel
				}
			})
			writing, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			parent, cancelParent := context.WithCancel(context.Background())
			var requests sync.WaitGroup
			defer func() { cancelParent(); unblock(); requests.Wait() }()
			w := &navTestWriter{onWrite: func() { close(writing); <-release }, writePanic: finish == "panic"}
			if finish == "error" {
				w.writeErr = io.ErrClosedPipe
			}
			done := make(chan any, 1)
			requests.Add(1)
			go func() {
				defer requests.Done()
				var v any
				defer func() {
					if p := recover(); p != nil {
						v = p
					}
					done <- v
				}()
				h.ServeHTTP(w, navTestRequest("/fixture"))
			}()
			<-childStarted
			select {
			case <-writing:
			case <-time.After(time.Second):
				t.Fatal("first response did not reach writer")
			}
			// Write is blocked and no waiter exists yet: admission must still
			// be held, independent of when the next goroutine is scheduled.
			if len(h.slots) != 1 {
				t.Fatal("builder admission released before Write returned")
			}
			waited := make(chan *navTestWriter, 1)
			requests.Add(1)
			go func() {
				defer requests.Done()
				waited <- navTestServe(h, navTestRequest("/second").WithContext(parent))
			}()
			expire := <-childStarted
			if calls.Load() != 1 {
				t.Fatal("blocked write allowed another builder")
			}
			if finish == "error" {
				cancelParent()
			} else {
				expire()
			}
			select {
			case got := <-waited:
				code := `{"error":"list_busy"}`
				if finish == "error" {
					code = `{"error":"request_cancelled"}`
				}
				navTestResponse(t, got, 503, code)
			case <-time.After(time.Second):
				t.Fatal("admission waiter did not finish")
			}
			if calls.Load() != 1 {
				t.Fatal("waiting request performed discovery")
			}
			unblock()
			select {
			case v := <-done:
				if (v != nil) != (finish == "panic") {
					t.Fatal("transport panic swallowed or invented")
				}
			case <-time.After(time.Second):
				t.Fatal("writer did not finish")
			}
			if w.writes != 1 {
				t.Fatal("second response after transport failure")
			}
			navTestDecode(t, navTestServe(h, navTestRequest("/retry")))
			if calls.Load() != 2 {
				t.Fatal("admission not released")
			}
		})
	}
	// Both select arms can be ready: expiry must still be checked after an
	// acquired slot and never let a no-work scan pass as complete data.
	var calls atomic.Int32
	h := navTestHandler(t, nil, func(d *navSessionsDeps, _ *navSessionsLimits) {
		d.listContext = func(p context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := navTestChild(p)
			cancel()
			return ctx, cancel
		}
		d.source = navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) { calls.Add(1); return nil, nil }}
	})
	for i := 0; i < 16; i++ {
		navTestResponse(t, navTestServe(h, navTestRequest("/fixture")), 503, `{"error":"list_busy"}`)
	}
	if calls.Load() != 0 {
		t.Fatal("expired child reached discovery")
	}
	p, cancel := context.WithCancel(context.Background())
	cancel()
	navTestResponse(t, navTestServe(h, navTestRequest("/fixture").WithContext(p)), 503, `{"error":"request_cancelled"}`)
}

func TestNavSessionsConstructorAndRealCache(t *testing.T) {
	base := navSessionsDeps{source: navTestSource{candidates: func(context.Context, string) ([]transcript.Candidate, error) {
		t.Error("constructor I/O")
		return nil, nil
	}}, summary: func(context.Context, string) (transcript.Summary, error) {
		t.Error("constructor parse")
		return transcript.Summary{}, nil
	}}
	// Derive the minimum from independent wire literals, not the constructor.
	minBody := 0
	for _, code := range []string{"invalid_request", "not_found", "method_not_allowed", "internal_error", "list_too_large", "request_cancelled", "list_busy"} {
		minBody = max(minBody, len(`{"error":"`+code+`"}`))
	}
	for _, limits := range []navSessionsLimits{{0, 1024, 1}, {1024, 0, 1}, {1024, 1024, 0}, {1024, int64(minBody - 1), 1}} {
		if h, err := newNavSessionsHandler(base, limits); h != nil || err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	if _, err := newNavSessionsHandler(base, navSessionsLimits{1024, int64(minBody), 1}); err != nil {
		t.Fatal("derived body minimum rejected")
	}
	for _, deps := range []navSessionsDeps{{source: base.source}, {summary: base.summary}} {
		if h, err := newNavSessionsHandler(deps, navSessionsLimits{1024, 1024, 1}); h != nil || err == nil {
			t.Fatal("missing dependency accepted")
		}
	}

	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	// Small actual parser fixtures, independent of the injected Summary tests.
	claudeRoot, codexRoot, agyRoot := filepath.Join(root, "claude"), filepath.Join(root, "codex"), filepath.Join(root, "agy")
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(claudeRoot, nonAlnumPath(work), navTestID+".jsonl"), fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q,\"cwd\":%q,\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"fixture question\"}}\n", navTestID, work))
	const codexID = "23456789-1234-4234-8234-123456789abc"
	write(filepath.Join(codexRoot, "rollout-"+codexID+".jsonl"), fmt.Sprintf("{\"type\":\"session_meta\",\"timestamp\":\"2026-01-02T00:00:00Z\",\"payload\":{\"id\":%q,\"cwd\":%q,\"source\":\"cli\"}}\n{\"type\":\"response_item\",\"timestamp\":\"2026-01-02T00:00:00Z\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"fixture codex\"}]}}\n", codexID, work))
	const agyID = "3456789a-1234-4234-8234-123456789abc"
	write(transcript.AgyTranscript(agyRoot, agyID), "{\"type\":\"USER_INPUT\",\"source\":\"USER_EXPLICIT\",\"status\":\"DONE\",\"content\":\"fixture agy\",\"created_at\":\"2026-01-03T00:00:00Z\"}\n")
	write(filepath.Join(agyRoot, "history.jsonl"), fmt.Sprintf("{\"conversationId\":%q,\"workspace\":%q,\"timestamp\":1,\"display\":\"fixture\"}\n", agyID, work))
	roots := transcript.Roots{ClaudeProjects: []string{claudeRoot}, CodexSessions: []string{codexRoot}, AgyAppData: []string{agyRoot}}
	cache := transcript.NewCache()
	index := transcript.NewIndex(roots)
	h, err := newNavSessionsHandler(navSessionsDeps{roots: roots, source: index, summary: cache.Summary}, navSessionsLimits{1 << 20, 1 << 20, 1})
	if err != nil {
		t.Fatal(err)
	}
	var previous []byte
	for i := 0; i < 2; i++ {
		w := navTestServe(h, navTestRequest(work))
		out := navTestDecode(t, w)
		rows := navTestRows(t, out)
		if len(rows) != 3 || out["scanned"] != float64(3) || out["of"] != float64(3) {
			t.Fatal("real fixture membership", out)
		}
		for j, want := range []string{"agy/" + agyID, "codex/" + codexID, "claude/" + navTestID} {
			r := rows[j].(map[string]any)
			if r["provider"].(string)+"/"+r["id"].(string) != want || r["turns"] != float64(1) {
				t.Fatal("real parser row", r)
			}
		}
		if i == 1 && string(previous) != string(w.body) {
			t.Fatal("unchanged cached response changed")
		}
		previous = append([]byte(nil), w.body...)
	}
	if cache.Stats().Parses != 3 || index.Walks() != 1 {
		t.Fatal("second request reparsed or rewalked")
	}
}
