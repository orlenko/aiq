package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func TestNavDetailOversizedHoldExpiry(t *testing.T) {
	f := detailTestNew(t)
	var projections atomic.Int32
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		projections.Add(1)
		w, err := projectNavConversation(s, o)
		if err != nil {
			return nil, err
		}
		lines := w.Messages[0].Groups[0].Lines
		spare := make([]navConversationLine, len(lines), 400000)
		copy(spare, lines)
		w.Messages[0].Groups[0].Lines = spare
		return w, nil
	}
	h := f.handler(t)
	first := detailTestServe(h, detailTestRequest())
	detailTestOK(t, first)
	tag := first.Header().Get("ETag")
	check := func(w *detailTestWriter) {
		t.Helper()
		if w.status != http.StatusNotModified || w.writes != 0 || w.Header().Get("ETag") != tag {
			t.Fatal("oversized conditional response changed or wrote a body")
		}
	}
	check(detailTestServe(h, detailTestConditional(tag)))
	if projections.Load() != 1 || f.cache.Stats().OversizedViews != 1 {
		t.Fatal("oversized view did not reuse its holding slot")
	}
	// Exercise the real public Cache's two-second view hold, with no clock
	// injection or stat-only shortcut in this incremental handler.
	time.Sleep(2100 * time.Millisecond)
	check(detailTestServe(h, detailTestConditional(tag)))
	if projections.Load() != 2 || f.cache.Stats().OversizedViews != 2 || f.cache.Stats().Parses != 1 {
		t.Fatal("expired held view failed bounded rebuild from retained parse")
	}
}

func TestNavDetailThreeSessionPressureRecovery(t *testing.T) {
	cache := transcript.NewCache()
	type reader struct {
		fixture *detailTestFixture
		handler *navDetailHandler
		path    string
		tag     string
		failed  *atomic.Bool
	}
	readers := make([]reader, 3)
	for i := range readers {
		f := detailTestNew(t)
		id := fmt.Sprintf("00000001-0000-4000-8000-%012d", i+1)
		f.path = filepath.Join(t.TempDir(), id+".jsonl")
		data := bytes.ReplaceAll(detailTestData(fmt.Sprintf("session-%d", i), f.at), []byte(detailTestID), []byte(id))
		if err := os.WriteFile(f.path, data, 0600); err != nil {
			t.Fatal(err)
		}
		f.cache, f.deps.cache = cache, cache
		failed := new(atomic.Bool)
		f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
			if failed.Load() {
				return navDetailFacts{}, fmt.Errorf("fixture facts unavailable")
			}
			return navDetailFacts{leaseKnown: true}, nil
		}
		path := "/api/nav/sessions/claude/" + id
		h := f.handler(t)
		w := detailTestServe(h, httptest.NewRequest(http.MethodGet, path, nil))
		detailTestOK(t, w)
		readers[i] = reader{f, h, path, w.Header().Get("ETag"), failed}
	}
	for round := 0; round < 3; round++ {
		for i := len(readers) - 1; i >= 0; i-- {
			r := &readers[i]
			request := httptest.NewRequest(http.MethodGet, r.path, nil)
			request.Header.Set("If-None-Match", r.tag)
			w := detailTestServe(r.handler, request)
			if w.status != http.StatusNotModified || w.writes != 0 || w.Header().Get("ETag") != r.tag {
				t.Fatal("three-session polling mixed cached bodies or validators")
			}
		}
	}
	// An ordinary failure in one follower must neither poison its retained
	// renderer shape nor prevent the other two sessions from being read.
	r := &readers[1]
	r.failed.Store(true)
	request := httptest.NewRequest(http.MethodGet, r.path, nil)
	request.Header.Set("If-None-Match", "*")
	detailTestError(t, detailTestServe(r.handler, request), 500, "internal_error")
	for _, i := range []int{0, 2} {
		detailTestOK(t, detailTestServe(readers[i].handler, httptest.NewRequest(http.MethodGet, readers[i].path, nil)))
	}
	r.failed.Store(false)
	w := detailTestServe(r.handler, request)
	if w.status != http.StatusNotModified || w.writes != 0 {
		t.Fatal("ordinary failure prevented quiet recovery to the retained view")
	}
	if stats := cache.Stats(); stats.Parses != 3 || stats.ViewBuilds != 3 {
		t.Fatal("ordinary three-session polls repeated parse or view work", stats)
	}
	readers[0].fixture.pressure(t) // Retire all three with invented tiny files.
	for _, r := range readers {
		w := detailTestServe(r.handler, httptest.NewRequest(http.MethodGet, r.path, nil))
		detailTestOK(t, w)
		if w.Header().Get("ETag") != r.tag {
			t.Fatal("quiet rerequest after parse eviction changed content identity")
		}
	}
	if stats := cache.Stats(); stats.Parses != 39 || stats.ViewBuilds != 6 || stats.FailedViews != 0 {
		t.Fatal("three-session retirement/recovery did not complete once per session", stats)
	}
}
