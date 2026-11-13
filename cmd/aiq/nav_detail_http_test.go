package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

type detailTestFixture struct {
	path   string
	cache  *transcript.Cache
	deps   navDetailDeps
	limits navDetailLimits
	at     time.Time
}

func detailTestData(prompt string, at time.Time) []byte {
	return []byte(fmt.Sprintf("{\"type\":\"user\",\"sessionId\":%q,\"cwd\":\"/fixture/work\",\"timestamp\":%q,\"message\":{\"role\":\"user\",\"content\":%q}}\n{\"type\":\"assistant\",\"timestamp\":%q,\"message\":{\"id\":\"fixture-message\",\"model\":\"fixture-model\",\"role\":\"assistant\",\"stop_reason\":\"end_turn\",\"content\":[{\"type\":\"text\",\"text\":\"fixture answer\"}]}}\n{\"type\":\"user\",\"isMeta\":true,\"timestamp\":%q,\"message\":{\"role\":\"user\",\"content\":\"fixture bookkeeping\"}}\n", detailTestID, at.Format(time.RFC3339Nano), prompt, at.Add(time.Second).Format(time.RFC3339Nano), at.Add(2*time.Second).Format(time.RFC3339Nano)))
}
func detailTestNew(t *testing.T) *detailTestFixture {
	t.Helper()
	f := &detailTestFixture{path: filepath.Join(t.TempDir(), detailTestID+".jsonl"), cache: transcript.NewCache(), at: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), limits: navDetailLimits{bodyBytes: 4 << 20, requests: 4}}
	f.write(t, "question-old")
	f.deps = navDetailDeps{cache: f.cache, resolve: func(context.Context, string, string) (transcript.Hit, error) {
		return transcript.Hit{Path: f.path}, nil
	}, facts: func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
		return navDetailFacts{leaseKnown: true}, nil
	}, now: func() time.Time { return time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC) }, location: time.UTC, build: "fixture-build", skippedNote: "explicit fixture skip", liveMark: "!"}
	return f
}
func (f *detailTestFixture) write(t *testing.T, prompt string) {
	t.Helper()
	if err := os.WriteFile(f.path, detailTestData(prompt, f.at), 0600); err != nil {
		t.Fatal(err)
	}
	pinned := time.Unix(1600000000, 0)
	if err := os.Chtimes(f.path, pinned, pinned); err != nil {
		t.Fatal(err)
	}
}
func (f *detailTestFixture) handler(t *testing.T) *navDetailHandler {
	t.Helper()
	h, err := newNavDetailHandler(f.deps, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type detailTestWriter struct {
	header            http.Header
	status, writes    int
	body              []byte
	onHeader, onWrite func()
	headerOnce        sync.Once
	writeErr          error
	writePanic, short bool
}

func (w *detailTestWriter) Header() http.Header {
	w.headerOnce.Do(func() {
		if w.onHeader != nil {
			w.onHeader()
		}
	})
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *detailTestWriter) WriteHeader(n int) {
	if w.status != 0 {
		panic("second status")
	}
	w.status = n
}
func (w *detailTestWriter) Write(b []byte) (int, error) {
	w.writes++
	if w.onWrite != nil {
		w.onWrite()
	}
	if w.writePanic {
		panic("invented writer panic")
	}
	w.body = append(w.body, b...)
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short {
		return len(b) - 1, nil
	}
	return len(b), nil
}
func detailTestServe(h http.Handler, r *http.Request) *detailTestWriter {
	w := &detailTestWriter{}
	h.ServeHTTP(w, r)
	return w
}
func detailTestError(t *testing.T, w *detailTestWriter, status int, code string) {
	t.Helper()
	expected := `{"build":"fixture-build","error":"` + code + `","waited_s":0}`
	if w.status != status || w.writes != 1 || string(w.body) != expected {
		t.Fatalf("error %d/%d %s want %d %s", w.status, w.writes, w.body, status, expected)
	}
	retry := ""
	if code == "preparing" {
		retry = "2"
	}
	if w.Header().Get("Retry-After") != retry {
		t.Fatal("error retry policy")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing response headers")
	}
}
func detailTestOK(t *testing.T, w *detailTestWriter) map[string]any {
	t.Helper()
	if w.status != 200 || w.writes != 1 {
		t.Fatalf("not success %d/%d %s", w.status, w.writes, w.body)
	}
	hash := sha256.Sum256(w.body)
	if w.Header().Get("ETag") != `"`+hex.EncodeToString(hash[:])+`"` {
		t.Fatal("ETag not quoted SHA256 of exact response")
	}
	var value map[string]any
	if err := json.Unmarshal(w.body, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
func detailTestConditional(tag string) *http.Request {
	r := detailTestRequest()
	if tag != "" {
		r.Header.Set("If-None-Match", tag)
	}
	return r
}

func TestNavDetailValidationAndErrorMapping(t *testing.T) {
	f := detailTestNew(t)
	var calls atomic.Int32
	f.deps.resolve = func(context.Context, string, string) (transcript.Hit, error) {
		calls.Add(1)
		return transcript.Hit{}, errors.New("PRIVATE resolver")
	}
	h := f.handler(t)
	for _, query := range []string{"last=0", "markup=2", "at=0", "from_key=x", "last=1&last=2", "last=%GG"} {
		r := detailTestRequest()
		r.URL.RawQuery = query
		r.RequestURI = r.URL.RequestURI()
		r.Body = navTestUnreadBody{}
		detailTestError(t, detailTestServe(h, r), 400, "invalid_request")
	}
	r := detailTestRequest()
	r.Header.Add("If-None-Match", strings.Repeat("x", 8192))
	r.Header.Add("If-None-Match", "")
	detailTestError(t, detailTestServe(h, r), 400, "invalid_request")
	r = detailTestRequest()
	r.Method = "POST"
	w := detailTestServe(h, r)
	detailTestError(t, w, 405, "method_not_allowed")
	if w.Header().Get("Allow") != "GET" {
		t.Fatal("missing Allow")
	}
	r = detailTestRequest()
	r.URL.Path = "/not-found"
	r.RequestURI = r.URL.Path
	detailTestError(t, detailTestServe(h, r), 404, "not_found")
	if calls.Load() != 0 {
		t.Fatal("invalid request performed lookup")
	}
	for _, tc := range []struct {
		stage  string
		err    error
		status int
		code   string
	}{{"resolve", transcript.ErrNoSuchSession, 404, "session_not_found"}, {"resolve", transcript.ErrBadID, 400, "invalid_request"}, {"resolve", errors.New("PRIVATE cause"), 500, "internal_error"}, {"load", os.ErrNotExist, 404, "session_not_found"}, {"load", transcript.ErrTooLarge, 413, "transcript_too_large"}, {"load", transcript.ErrUnreadable, 500, "internal_error"}, {"load", transcript.ErrPreparing, 503, "preparing"}, {"facts", errors.New("PRIVATE cause"), 500, "internal_error"}, {"project", transcript.ErrUnshowable, 500, "unshowable"}, {"project", errNavConversationInvalid, 500, "internal_error"}, {"project", errNavConversationTooLarge, 503, "view_too_large"}} {
		t.Run(tc.stage+tc.code, func(t *testing.T) {
			f := detailTestNew(t)
			switch tc.stage {
			case "resolve":
				f.deps.resolve = func(context.Context, string, string) (transcript.Hit, error) { return transcript.Hit{}, tc.err }
			case "load":
				f.deps.load = func(context.Context, string) (*transcript.Session, transcript.Snapshot, error) {
					return nil, transcript.Snapshot{}, tc.err
				}
			case "facts":
				f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
					return navDetailFacts{}, tc.err
				}
			case "project":
				f.deps.project = func(*transcript.Session, navConversationOptions) (*navConversationWindow, error) { return nil, tc.err }
			}
			detailTestError(t, detailTestServe(f.handler(t), detailTestConditional("*")), tc.status, tc.code)
		})
	}
	f = detailTestNew(t)
	r = detailTestRequest()
	r.URL.RawQuery = "at=1234"
	r.RequestURI = r.URL.RequestURI()
	detailTestError(t, detailTestServe(f.handler(t), r), 404, "anchor_not_found")
	f = detailTestNew(t)
	f.deps.load = func(ctx context.Context, path string) (*transcript.Session, transcript.Snapshot, error) {
		s, snap, err := f.cache.Session(ctx, path)
		owned := *s
		owned.ID = "00000001-0000-4000-8000-000000000002"
		return &owned, snap, err
	}
	detailTestError(t, detailTestServe(f.handler(t), detailTestRequest()), 409, "conversation_changed")
}

func TestNavDetailRealCacheEnvelopeAndConditionals(t *testing.T) {
	f := detailTestNew(t)
	var projections atomic.Int32
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		projections.Add(1)
		return projectNavConversation(s, o)
	}
	h := f.handler(t)
	first := detailTestServe(h, detailTestRequest())
	value := detailTestOK(t, first)
	if value["build"] != "fixture-build" || value["reset"] != true || value["window_start"] != float64(0) || value["earlier_hidden"] != float64(0) || value["later_hidden"] != float64(0) {
		t.Fatal("flat envelope literal fields")
	}
	if _, nested := value["window"]; nested {
		t.Fatal("window re-encoded as nested object")
	}
	if strings.Contains(string(first.body), f.path) {
		t.Fatal("transcript filename leaked")
	}
	if value["session"].(map[string]any)["cwd"] != "/fixture/work" {
		t.Fatal("authorized display Cwd lost")
	}
	for _, header := range []string{first.Header().Get("ETag"), "W/" + first.Header().Get("ETag"), `"other", W/` + first.Header().Get("ETag"), "*"} {
		w := detailTestServe(h, detailTestConditional(header))
		if w.status != 304 || w.writes != 0 || len(w.body) != 0 || w.Header().Get("ETag") != first.Header().Get("ETag") {
			t.Fatal("conditional response body/status", w.status, w.writes)
		}
	}
	for _, header := range []string{`"other"`, first.Header().Get("ETag") + ", malformed"} {
		w := detailTestServe(h, detailTestConditional(header))
		detailTestOK(t, w)
		if !bytes.Equal(w.body, first.body) {
			t.Fatal("unchanged representation bytes changed")
		}
	}
	if projections.Load() != 1 || f.cache.Stats().Parses != 1 || f.cache.Stats().ViewBuilds != 1 {
		t.Fatal("ordinary conditional poll repeated parse/project")
	}
}

func TestNavDetailBodyPolicyAndQuarantinePrecedence(t *testing.T) {
	for _, smallFirst := range []bool{false, true} {
		f := detailTestNew(t)
		large := f.handler(t)
		f.limits.bodyBytes = 256
		small := f.handler(t)
		if smallFirst {
			detailTestError(t, detailTestServe(small, detailTestRequest()), 503, "view_too_large")
		}
		good := detailTestServe(large, detailTestRequest())
		detailTestOK(t, good)
		for _, condition := range []string{"", good.Header().Get("ETag"), "W/" + good.Header().Get("ETag"), "*"} {
			detailTestError(t, detailTestServe(small, detailTestConditional(condition)), 503, "view_too_large")
		}
		if f.cache.Stats().FailedViews != 0 {
			t.Fatal("body policy error poisoned renderer")
		}
		w := detailTestServe(large, detailTestRequest())
		detailTestOK(t, w)
		if !bytes.Equal(w.body, good.body) {
			t.Fatal("small policy damaged larger view")
		}
	}
	f := detailTestNew(t)
	contextB := false
	var projections atomic.Int32
	f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
		label := "A"
		if contextB {
			label = "B"
		}
		return navDetailFacts{leaseKnown: true, accounts: []convoAccountFact{{known: true, label: label}}}, nil
	}
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		projections.Add(1)
		if o.accounts[0].label == "B" {
			return nil, transcript.ErrUnshowable
		}
		return projectNavConversation(s, o)
	}
	h := f.handler(t)
	a := detailTestServe(h, detailTestRequest())
	detailTestOK(t, a)
	contextB = true
	detailTestError(t, detailTestServe(h, detailTestRequest()), 500, "unshowable")
	contextB = false
	for _, condition := range []string{"", a.Header().Get("ETag"), "W/" + a.Header().Get("ETag"), `"other", ` + a.Header().Get("ETag"), "*"} {
		detailTestError(t, detailTestServe(h, detailTestConditional(condition)), 500, "unshowable")
	}
	if projections.Load() != 2 {
		t.Fatal("quarantined shape rendered or bypassed by condition")
	}
	// Retire the old shape without waiting for the normal two-second parse
	// floor: pressure uses only additional invented tiny transcript files.
	f.pressure(t)
	f.write(t, "question-expanded")
	detailTestOK(t, detailTestServe(h, detailTestRequest()))
}

func TestNavDetailDifferentBudgetFlightsDoNotJoin(t *testing.T) {
	f := detailTestNew(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var wg sync.WaitGroup
	defer func() { unblock(); wg.Wait() }()
	largeB := f.limits.bodyBytes
	var encodes atomic.Int32
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		if o.bodyBytes == largeB {
			close(entered)
			<-release
		}
		return projectNavConversation(s, o)
	}
	f.deps.marshal = func(v any) ([]byte, error) { encodes.Add(1); return json.Marshal(v) }
	large := f.handler(t)
	f.limits.bodyBytes = 256
	small := f.handler(t)
	bigResult, smallResult := make(chan *detailTestWriter, 1), make(chan *detailTestWriter, 1)
	wg.Add(1)
	go func() { defer wg.Done(); bigResult <- detailTestServe(large, detailTestRequest()) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("large builder not admitted")
	}
	wg.Add(1)
	go func() { defer wg.Done(); smallResult <- detailTestServe(small, detailTestConditional("*")) }()
	select {
	case w := <-smallResult:
		detailTestError(t, w, 503, "view_too_large")
	case <-time.After(2 * time.Second):
		t.Fatal("small policy joined blocked large-policy flight")
	}
	if encodes.Load() != 0 || f.cache.Stats().FailedViews != 0 {
		t.Fatal("small overflow encoded or poisoned shape")
	}
	unblock()
	select {
	case w := <-bigResult:
		detailTestOK(t, w)
	case <-time.After(2 * time.Second):
		t.Fatal("large builder did not finish")
	}
	if encodes.Load() != 1 {
		t.Fatal("large success envelope encoding count")
	}
}
