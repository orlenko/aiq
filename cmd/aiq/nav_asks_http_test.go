package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

func asksHTTPFixture(t *testing.T, s *state.AsksSnapshot) (*navAsksHandler, *int, *int, *[]string) {
	t.Helper()
	clocks, reads := 0, 0
	paths := []string{}
	h, e := newNavAsksHandler(navAsksHTTPDeps{now: func() time.Time { clocks++; return responseOptions().now },
		read: func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
			reads++
			return s, nil
		},
		resolve: func(_ context.Context, p string) (navAsksAlias, error) {
			paths = append(paths, p)
			return navAsksAlias{}, nil
		}},
		navAsksHTTPLimits{response: responseOptions(), reader: state.AsksSnapshotLimits{Rows: 100, TextBytes: 1 << 20}, requests: 1})
	if e != nil {
		t.Fatal(e)
	}
	return h, &clocks, &reads, &paths
}
func asksHTTPGet(target string) *http.Request {
	return httptest.NewRequest(http.MethodGet, target, nil)
}
func asksHTTPBody(t *testing.T, h *navAsksHandler, target string) (int, []byte) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, asksHTTPGet(target))
	return w.Code, w.Body.Bytes()
}

func TestNavAsksHTTPDecodeAndEarlyRefusal(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   int
	}{
		{"/api/nav/asks", 200}, {"/api/nav/asks?dir=%2Fproject%2F", 200}, {"/api/nav/asks?%64ir=%2Fa+b", 200},
		{"/other", 404}, {"/api/nav/asks/", 400}, {"/api//nav/asks", 400}, {"/api/nav/./asks", 400},
		{"/api/nav/asks?", 400}, {"/api/nav/asks?&", 400}, {"/api/nav/asks?dir=", 400}, {"/api/nav/asks?dir=relative", 400},
		{"/api/nav/asks?dir=%2Fa&", 400}, {"/api/nav/asks?dir=%2Fa&&x=y", 400}, {"/api/nav/asks?x=1", 400},
		{"/api/nav/asks?dir=%2Fa&%64ir=%2Fb", 400}, {"/api/nav/asks?dir=%00", 400}, {"/api/nav/asks?dir=%2F%ff", 400}, {"/api/nav/asks?dir=%2Fa;b", 400},
	} {
		t.Run(tc.target, func(t *testing.T) {
			s := responseSnapshot()
			h, c, r, _ := asksHTTPFixture(t, &s)
			status, b := asksHTTPBody(t, h, tc.target)
			if status != tc.want {
				t.Fatalf("status%d want%d body%s", status, tc.want, b)
			}
			if tc.want != 200 && (*c != 0 || *r != 0) {
				t.Fatal("refusal called dependencies")
			}
		})
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.URL.RawPath = "/api/nav/asks" }, func(r *http.Request) { r.URL.Fragment = "x" }, func(r *http.Request) { r.URL.RawFragment = "x" },
		func(r *http.Request) { r.URL.Opaque = "x" }, func(r *http.Request) { r.URL.Scheme = "http" }, func(r *http.Request) { r.URL.Host = "example.invalid" }, func(r *http.Request) { r.URL.User = url.User("invented") },
		func(r *http.Request) { r.URL.RawQuery = "dir=%"; r.RequestURI = "/api/nav/asks?dir=%" }, func(r *http.Request) { r.RequestURI = "/api/nav/asks?dir=%2Fother" },
		func(r *http.Request) { r.ContentLength = 1 }, func(r *http.Request) { r.TransferEncoding = []string{"chunked"} },
	} {
		s := responseSnapshot()
		h, c, r, _ := asksHTTPFixture(t, &s)
		req := asksHTTPGet("/api/nav/asks")
		mutate(req)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 || *c != 0 || *r != 0 {
			t.Fatal("invalid representation reached read", w.Code)
		}
	}
	for _, n := range []int{4096, 4097} {
		s := responseSnapshot()
		h, _, _, _ := asksHTTPFixture(t, &s)
		dir := "/" + strings.Repeat("x", n-1)
		status, b := asksHTTPBody(t, h, "/api/nav/asks?dir="+url.QueryEscape(dir))
		want := 200
		if n == 4097 {
			want = 400
		}
		if status != want {
			t.Fatal("decoded limit", n, status)
		}
		if status == 200 && responseJSON(t, b)["scope"].(map[string]any)["dir"] != dir {
			t.Fatal("scope changed")
		}
	}
	s := responseSnapshot()
	h, c, r, _ := asksHTTPFixture(t, &s)
	req := asksHTTPGet("/api/nav/asks")
	req.RequestURI = ""
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal("direct empty URI")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req = asksHTTPGet("/api/nav/asks").WithContext(ctx)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 || *c != 1 || *r != 1 {
		t.Fatal("entry cancel called dependencies")
	}
	for _, target := range []string{"/api/nav/asks?dir=", "/other"} {
		req = asksHTTPGet(target).WithContext(ctx)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
		want := 400
		if target == "/other" {
			want = 404
		}
		if w.Code != want {
			t.Fatal("decoder lost to cancel", w.Code)
		}
	}
	req = asksHTTPGet("/api/nav/asks?dir=").WithContext(ctx)
	req.Method = http.MethodPost
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal("method/query/cancel precedence")
	}
}

func TestNavAsksHTTPSharedAliasCreditsAndOwnership(t *testing.T) {
	for _, limit := range []uint64{43, 42} {
		s := responseRows()
		for i := range s.Items {
			s.Items[i].Workspace = "/b"
		}
		h, _, _, paths := asksHTTPFixture(t, &s)
		h.limits.response.inputBytes = limit
		renders := 0
		h.d.render = func(s string, w int, m bool) ([]webMarkdownLine, bool) {
			renders++
			return webMarkdownLinesChecked(s, w, m)
		}
		h.d.resolve = func(_ context.Context, p string) (navAsksAlias, error) {
			*paths = append(*paths, p)
			return navAsksAlias{path: "/z", resolved: true}, nil
		}
		status, b := asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
		if !reflect.DeepEqual(*paths, []string{"/a", "/b"}) {
			t.Fatal("distinct path calls", *paths)
		}
		// Raw: mode9+dir2+day10+two(workspace2+state4+quote1)=35.
		// Distinct memo facts: (/a2+/z2)+(/b2+/z2)=8, exact total43.
		if limit == 43 {
			if status != 200 || len(responseItems(responseJSON(t, b), 2)) != 2 {
				t.Fatal("exact combined43", status)
			}
		} else if status != 503 || string(b) != `{"error":"asks_too_large"}` || renders != 0 {
			t.Fatal("one-short combined42 rendered/reset", status, renders)
		}
	}
	s := responseRows()
	s.Items = s.Items[:1]
	s.Items[0].Workspace = "/b"
	s.Items[0].Refs = []string{"ref"}
	s.ReportPresent = true
	s.Status.NotWatched = []string{"cold"}
	h, _, _, paths := asksHTTPFixture(t, &s)
	h.d.resolve = func(_ context.Context, p string) (navAsksAlias, error) {
		*paths = append(*paths, p)
		s.Items[0].Workspace = "/wrong"
		s.Items[0].FirstQuote = "changed"
		s.Items[0].Refs[0] = "changed"
		s.Status.NotWatched[0] = "changed"
		return navAsksAlias{"/z", true}, nil
	}
	status, b := asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 200 {
		t.Fatal(status)
	}
	v := responseJSON(t, b)
	item := responseItems(v, 2)[0].(map[string]any)
	if item["workspace"] != "/b" || item["first"].(map[string]any)["copy"] != "> z\n\n" || item["refs"].([]any)[0] != "ref" || v["report"].(map[string]any)["not_watched"].([]any)[0] != "cold" {
		t.Fatal("resolver changed captured graph")
	}
	for _, ws := range []string{"", "/a/sub", "/a", "/"} {
		s = responseRows()
		s.Items[0].Workspace = ws
		s.Items[1].Workspace = ws
		h, _, _, paths = asksHTTPFixture(t, &s)
		status, _ = asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
		if status != 200 || len(*paths) != 0 {
			t.Fatal("lexical called resolver", ws)
		}
	}
	s = responseRows()
	for i := range s.Items {
		s.Items[i].Workspace = "/b"
	}
	h, _, _, paths = asksHTTPFixture(t, &s)
	status, b = asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 200 || len(responseItems(responseJSON(t, b), 2)) != 0 || len(*paths) != 2 {
		t.Fatal("unresolved paths falsely matched")
	}
	h.d.resolve = func(context.Context, string) (navAsksAlias, error) { return navAsksAlias{"/same", true}, nil }
	status, b = asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 200 || len(responseItems(responseJSON(t, b), 2)) != 2 {
		t.Fatal("alias memo survived request")
	}
}

func TestNavAsksHTTPStructuralAdmissionAndSingleUse(t *testing.T) {
	s := responseRows()
	for i := range s.Items {
		s.Items[i].Workspace = "/b"
	}
	s.Items[0].Refs = make([]string, 3)
	s.Items[1].Refs = make([]string, 2)
	s.ReportPresent = true
	s.Status.NotWatched = make([]string, 2)
	h, _, _, paths := asksHTTPFixture(t, &s)
	h.limits.response.arrayEntries = 7
	status, _ := asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 200 {
		t.Fatal("exact structural7", status)
	}
	s.Items[1].Refs = make([]string, 3)
	*paths = nil
	status, b := asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 503 || string(b) != `{"error":"asks_too_large"}` || len(*paths) != 0 {
		t.Fatal("excluded empty refs escaped preclone gate")
	}
	s.Items[1].Refs = make([]string, 2)
	s.ReportPresent = false
	s.Status.NotWatched = make([]string, 10000)
	status, _ = asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 200 {
		t.Fatal("absent report lists consumed", status)
	}
	s.Items[1].Chain = "\xff"
	*paths = nil
	status, _ = asksHTTPBody(t, h, "/api/nav/asks?dir=%2Fa")
	if status != 500 || len(*paths) != 0 {
		t.Fatal("invalid excluded Chain resolved")
	}
	o := responseOptions()
	f := &navAsksFaultState{latch: &diagnostic.Latch{}}
	run, e := newNavAsksRun(context.Background(), responseSnapshot(), o, f)
	if e != nil {
		t.Fatal(e)
	}
	if b, e := run.finish(nil, json.Marshal); b != nil || e != errNavAsksResponseInvalid {
		t.Fatal("nil renderer accepted")
	}
	if b, e := run.finish(webMarkdownLinesChecked, json.Marshal); b != nil || e != errNavAsksResponseInvalid {
		t.Fatal("failed continuation reused")
	}
	if f.current {
		t.Fatal("normal defer marked panic")
	}
	methods := 0
	s = responseSnapshot()
	h, _, _, _ = asksHTTPFixture(t, &s)
	h.d.read = func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
		return nil, asksWordHostile{&methods}
	}
	status, _ = asksHTTPBody(t, h, "/api/nav/asks")
	if status != 500 || methods != 0 {
		t.Fatal("hostile reader error inspected")
	}
	h.d.read = func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
		return nil, nil
	}
	status, _ = asksHTTPBody(t, h, "/api/nav/asks")
	if status != 500 {
		t.Fatal("nil successful read accepted")
	}
}

func TestNavAsksHTTPAliasRefusalsAndCancellationStages(t *testing.T) {
	for _, kind := range []string{"empty resolved", "nonempty unresolved", "invalid UTF8", "fatal", "cancel", "cancel sentinel", "key quota", "result quota"} {
		t.Run(kind, func(t *testing.T) {
			s := responseRows()
			for i := range s.Items {
				s.Items[i].Workspace = "/b"
			}
			h, _, _, paths := asksHTTPFixture(t, &s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			methods, renders, marshals := 0, 0, 0
			h.d.render = func(string, int, bool) ([]webMarkdownLine, bool) { renders++; return nil, true }
			h.d.marshal = func(v any) ([]byte, error) { marshals++; return json.Marshal(v) }
			h.d.resolve = func(got context.Context, p string) (navAsksAlias, error) {
				if got != ctx {
					t.Fatal("resolver context changed")
				}
				*paths = append(*paths, p)
				switch kind {
				case "empty resolved":
					return navAsksAlias{"", true}, nil
				case "nonempty unresolved":
					return navAsksAlias{"wrong", false}, nil
				case "invalid UTF8":
					return navAsksAlias{"\xff", true}, nil
				case "fatal":
					return navAsksAlias{}, asksWordHostile{&methods}
				case "cancel":
					cancel()
					return navAsksAlias{"/z", true}, nil
				case "cancel sentinel":
					return navAsksAlias{}, context.Canceled
				}
				return navAsksAlias{"/z", true}, nil
			}
			if kind == "key quota" {
				h.limits.response.inputBytes = 36
			}
			if kind == "result quota" {
				h.limits.response.inputBytes = 37
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, asksHTTPGet("/api/nav/asks?dir=%2Fa").WithContext(ctx))
			want, code := 500, "unshowable"
			switch kind {
			case "fatal":
				code = "internal_error"
			case "cancel", "cancel sentinel":
				want, code = 503, "request_cancelled"
			case "key quota", "result quota":
				want, code = 503, "asks_too_large"
			}
			calls := 1
			if kind == "key quota" {
				calls = 0
			}
			if w.Code != want || w.Body.String() != `{"error":"`+code+`"}` || len(*paths) != calls || methods != 0 || renders != 0 || marshals != 0 {
				t.Fatal("alias refusal escaped", kind, w.Code, len(*paths), methods, renders, marshals)
			}
		})
	}
	for _, stage := range []string{"clock", "reader", "renderer", "marshal"} {
		t.Run(stage, func(t *testing.T) {
			s := responseRows()
			h, clocks, reads, _ := asksHTTPFixture(t, &s)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			renders, marshals := 0, 0
			h.d.render = func(s string, w int, m bool) ([]webMarkdownLine, bool) {
				renders++
				if stage == "renderer" {
					cancel()
				}
				return webMarkdownLinesChecked(s, w, m)
			}
			h.d.marshal = func(v any) ([]byte, error) {
				marshals++
				b, e := json.Marshal(v)
				if stage == "marshal" {
					cancel()
				}
				return b, e
			}
			if stage == "clock" {
				h.d.now = func() time.Time { *clocks++; cancel(); return responseOptions().now }
			}
			if stage == "reader" {
				h.d.read = func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
					*reads++
					cancel()
					return &s, nil
				}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, asksHTTPGet("/api/nav/asks").WithContext(ctx))
			if w.Code != 503 || w.Body.String() != `{"error":"request_cancelled"}` || *clocks != 1 || stage == "clock" && *reads != 0 || stage != "clock" && *reads != 1 || stage != "marshal" && marshals != 0 {
				t.Fatal("interrupted stage continued", stage, w.Code, *clocks, *reads, renders, marshals)
			}
		})
	}
	s := responseSnapshot()
	h, _, _, _ := asksHTTPFixture(t, &s)
	for _, mutate := range []func(*navAsksHTTPDeps, *navAsksHTTPLimits){func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { d.read = nil }, func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { d.resolve = nil }, func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { d.now = nil }, func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { l.requests = 0 }, func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { l.reader.Rows = 0 }, func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { l.response.rows = ^uint64(0) }, func(d *navAsksHTTPDeps, l *navAsksHTTPLimits) { l.response.bodyBytes = 1 }} {
		d, l := h.d, h.limits
		mutate(&d, &l)
		if got, e := newNavAsksHandler(d, l); e == nil || got != nil {
			t.Fatal("invalid constructor accepted")
		}
	}
}

type asksHTTPWriter struct {
	header                  http.Header
	headerHook, writeHook   func()
	headers, status, writes int
	statusWrites            int
	body                    []byte
	writeErr                error
}

func (w *asksHTTPWriter) Header() http.Header {
	w.headers++
	if w.headerHook != nil {
		w.headerHook()
	}
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *asksHTTPWriter) WriteHeader(status int) { w.statusWrites++; w.status = status }
func (w *asksHTTPWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.writeHook != nil {
		w.writeHook()
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	w.body = append(w.body, b...)
	return len(b), nil
}
func asksHTTPSeed(unknown bool) (o diagnostic.Origin) {
	defer func() { o = diagnostic.Capture(recover()) }()
	if unknown {
		regexp.MustCompile("[")
	}
	panic("invented seed")
}

func TestNavAsksHTTPFaultPrecedenceSurvivesHeaderCancellation(t *testing.T) {
	// The public entry also hands the actual inner origin to its optional holder.
	snapshot := responseRows()
	public, _, _, _ := asksHTTPFixture(t, &snapshot)
	methods := 0
	public.d.render = func(string, int, bool) ([]webMarkdownLine, bool) { panic(asksWordHostile{&methods}) }
	ctx, holder := diagnostic.WithHolder(context.Background())
	w := httptest.NewRecorder()
	public.ServeHTTP(w, asksHTTPGet("/api/nav/asks").WithContext(ctx))
	file, _, known := holder.Origin().Location()
	if w.Code != 500 || methods != 0 || !known || file != "cmd/aiq/nav_asks_http_test.go" {
		t.Fatal("public entry lost inner origin or inspected panic")
	}
	for _, unknown := range []bool{false, true} {
		seed := asksHTTPSeed(unknown)
		_, _, located := seed.Location()
		if located == unknown {
			t.Fatal("seed knownness does not match schedule")
		}
		if !seed.Present() {
			t.Fatal("seed absent")
		}
		for _, stage := range []string{"success", "preflight", "coverage", "clock panic", "reader panic", "resolver panic", "pair panic", "marshal panic", "reader carrier", "resolver carrier", "marshal carrier"} {
			t.Run(stage+map[bool]string{false: " known", true: " unknown"}[unknown], func(t *testing.T) {
				s := responseRows()
				h, _, _, _ := asksHTTPFixture(t, &s)
				methods := 0
				req := asksHTTPGet("/api/nav/asks")
				switch stage {
				case "preflight":
					s.Items[0].StateText = "\xff"
				case "coverage":
					h.d.render = func(string, int, bool) ([]webMarkdownLine, bool) { return nil, false }
				case "clock panic":
					h.d.now = func() time.Time { panic(asksWordHostile{&methods}) }
				case "reader panic":
					h.d.read = func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
						panic(asksWordHostile{&methods})
					}
				case "resolver panic", "resolver carrier":
					req = asksHTTPGet("/api/nav/asks?dir=%2Fa")
					s.Items[0].Workspace = "/b"
					h.d.resolve = func(context.Context, string) (navAsksAlias, error) {
						if stage == "resolver panic" {
							panic(asksWordHostile{&methods})
						}
						return navAsksAlias{}, diagnostic.Failure(diagnostic.Unshowable, asksHTTPSeed(unknown))
					}
				case "pair panic":
					h.d.render = func(string, int, bool) ([]webMarkdownLine, bool) { panic(asksWordHostile{&methods}) }
				case "marshal panic":
					h.d.marshal = func(any) ([]byte, error) { panic(asksWordHostile{&methods}) }
				case "reader carrier":
					h.d.read = func(context.Context, time.Time, state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
						return nil, diagnostic.Failure(diagnostic.Unshowable, asksHTTPSeed(unknown))
					}
				case "marshal carrier":
					h.d.marshal = func(any) ([]byte, error) {
						return nil, diagnostic.Failure(diagnostic.Unshowable, asksHTTPSeed(unknown))
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx, holder := diagnostic.WithHolder(ctx)
				req = req.WithContext(ctx)
				f := &navAsksFaultState{latch: &diagnostic.Latch{}}
				f.latch.Note(seed)
				w := &asksHTTPWriter{headerHook: cancel}
				h.serve(w, req, f)
				fault := stage != "success" && stage != "preflight" && stage != "coverage"
				want := 503
				if fault {
					want = 500
				}
				if w.status != want || f.current != fault || f.latch.Origin() != seed || holder.Origin() != seed || methods != 0 || w.headers != 1 || w.statusWrites != 1 || w.writes != 1 || len(h.slots) != 0 {
					t.Fatalf("precedence stage%s status%d fault%v methods%d", stage, w.status, f.current, methods)
				}
				if !fault && string(w.body) != `{"error":"request_cancelled"}` {
					t.Fatal("ordinary failure protected by seed")
				}
			})
		}
	}
	for _, tc := range []struct {
		path, method string
		status       int
	}{{"/api/nav/asks?dir=", "GET", 400}, {"/other", "GET", 404}, {"/api/nav/asks?dir=", "POST", 405}} {
		s := responseSnapshot()
		h, c, r, _ := asksHTTPFixture(t, &s)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := asksHTTPGet(tc.path).WithContext(ctx)
		req.Method = tc.method
		w := &asksHTTPWriter{headerHook: cancel}
		h.ServeHTTP(w, req)
		if w.status != tc.status || *c != 0 || *r != 0 {
			t.Fatal("decoder rebased after Header", tc, w.status)
		}
	}
}

func TestNavAsksHTTPAdmissionThroughTransportAndRelease(t *testing.T) {
	for _, at := range []string{"header", "write"} {
		t.Run(at, func(t *testing.T) {
			s := responseSnapshot()
			h, clocks, reads, _ := asksHTTPFixture(t, &s)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			w := &asksHTTPWriter{}
			hook := func() { close(entered); <-release }
			if at == "header" {
				w.headerHook = hook
			} else {
				w.writeHook = hook
			}
			go func() { defer close(done); h.ServeHTTP(w, asksHTTPGet("/api/nav/asks")) }()
			<-entered
			status, b := asksHTTPBody(t, h, "/api/nav/asks")
			if status != 503 || string(b) != `{"error":"asks_busy"}` || *clocks != 1 || *reads != 1 {
				t.Fatal("transport released admission early")
			}
			close(release)
			<-done
			if len(h.slots) != 0 || w.status != 200 || w.statusWrites != 1 || w.writes != 1 {
				t.Fatal("slot not released")
			}
			status, _ = asksHTTPBody(t, h, "/api/nav/asks")
			if status != 200 || *reads != 2 {
				t.Fatal("next request did not recover")
			}
		})
	}
	for _, at := range []string{"header", "write", "write error"} {
		s := responseSnapshot()
		h, _, _, _ := asksHTTPFixture(t, &s)
		w := &asksHTTPWriter{header: http.Header{"Etag": []string{"stale"}, "Retry-After": []string{"stale"}, "Allow": []string{"stale"}, "Content-Length": []string{"1"}}}
		switch at {
		case "header":
			w.headerHook = func() { panic("invented transport") }
		case "write":
			w.writeHook = func() { panic("invented transport") }
		case "write error":
			w.writeErr = errors.New("invented write error")
		}
		panicked := false
		func() { defer func() { panicked = recover() != nil }(); h.ServeHTTP(w, asksHTTPGet("/api/nav/asks")) }()
		if panicked != (at != "write error") || len(h.slots) != 0 || w.headers != 1 || w.writes > 1 {
			t.Fatal("writer recovered/retried or leaked permit", at)
		}
		expectedHeaders := 1
		if at == "header" {
			expectedHeaders = 0
		}
		if w.statusWrites != expectedHeaders {
			t.Fatal("multiple transport headers", at)
		}
		if at == "write error" && (w.header.Get("ETag") != "" || w.header.Get("Retry-After") != "" || w.header.Get("Allow") != "" || w.header.Get("Content-Length") != strconv.Itoa(len(responseEmptyJSON))) {
			t.Fatal("stale or wrong response headers")
		}
	}
}
