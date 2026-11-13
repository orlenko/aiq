package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func (f *detailTestFixture) evictAndReplace(t *testing.T) (*transcript.Session, transcript.Snapshot) {
	t.Helper()
	old, snap, err := f.cache.Session(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.pressure(t)
	f.write(t, "question-new")
	current, currentSnap, err := f.cache.Session(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	if old == current || snap != currentSnap {
		t.Fatal("fixture did not replace parse under equal value Snapshot")
	}
	return current, currentSnap
}

func (f *detailTestFixture) pressure(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < 33; i++ {
		path := filepath.Join(root, fmt.Sprintf("pressure-%d.jsonl", i))
		if err := os.WriteFile(path, detailTestData("pressure", f.at), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.cache.Session(context.Background(), path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNavDetailParseAuthorityBeforeAdmission(t *testing.T) {
	f := detailTestNew(t)
	var projections atomic.Int32
	replace := true
	f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
		if replace {
			replace = false
			f.evictAndReplace(t)
		}
		return navDetailFacts{leaseKnown: true}, nil
	}
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		projections.Add(1)
		return projectNavConversation(s, o)
	}
	h := f.handler(t)
	detailTestError(t, detailTestServe(h, detailTestRequest()), 409, "conversation_changed")
	if projections.Load() != 0 || f.cache.Stats().FailedViews != 0 {
		t.Fatal("retired initial Session projected or quarantined")
	}
	w := detailTestServe(h, detailTestRequest())
	detailTestOK(t, w)
	if !bytes.Contains(w.body, []byte("question-new")) || bytes.Contains(w.body, []byte("question-old")) || projections.Load() != 1 {
		t.Fatal("quiet retry did not use current Session")
	}
}

func TestNavDetailRetirementAfterWitness(t *testing.T) {
	f := detailTestNew(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var requests sync.WaitGroup
	parent, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); unblock(); requests.Wait() }()
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return projectNavConversation(s, o)
	}
	h := f.handler(t)
	oldResult := make(chan *detailTestWriter, 1)
	requests.Add(1)
	go func() {
		defer requests.Done()
		oldResult <- detailTestServe(h, detailTestRequest().WithContext(parent))
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("old projection did not start")
	}
	f.evictAndReplace(t)
	current := detailTestServe(h, detailTestRequest())
	detailTestOK(t, current)
	if !bytes.Contains(current.body, []byte("question-new")) {
		t.Fatal("new owner not published")
	}
	unblock()
	select {
	case old := <-oldResult:
		detailTestOK(t, old)
		if !bytes.Contains(old.body, []byte("question-old")) {
			t.Fatal("old waiter fixture lost captured snapshot")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old waiter did not finish")
	}
	again := detailTestServe(h, detailTestRequest())
	detailTestOK(t, again)
	if !bytes.Equal(again.body, current.body) {
		t.Fatal("late retired builder replaced current cached bytes")
	}
	conditional := detailTestServe(h, detailTestConditional(current.Header().Get("ETag")))
	if conditional.status != 304 || conditional.writes != 0 {
		t.Fatal("current validator replaced by retired build")
	}
}

type detailTestDeadline struct {
	context.Context
	expired *atomic.Bool
}

func (c detailTestDeadline) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}
func detailTestChild(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	expired := &atomic.Bool{}
	return detailTestDeadline{ctx, expired}, func() { expired.Store(true); cancel() }
}

func TestNavDetailDeadlineBoundaries(t *testing.T) {
	for _, phase := range []string{"admission", "resolve", "load", "facts", "header", "header-conditional", "parent-header"} {
		t.Run(phase, func(t *testing.T) {
			f := detailTestNew(t)
			if phase == "load" {
				if _, _, err := f.cache.Session(context.Background(), f.path); err != nil {
					t.Fatal(err)
				}
			}
			parent, cancelParent := context.WithCancel(context.Background())
			defer cancelParent()
			var expire context.CancelFunc
			var resolveCalls atomic.Int32
			f.deps.requestContext = func(p context.Context) (context.Context, context.CancelFunc) {
				ctx, cancel := detailTestChild(p)
				expire = cancel
				if phase == "admission" {
					cancel()
				}
				return ctx, cancel
			}
			originalResolve := f.deps.resolve
			f.deps.resolve = func(ctx context.Context, p, id string) (transcript.Hit, error) {
				resolveCalls.Add(1)
				hit, err := originalResolve(ctx, p, id)
				if phase == "resolve" {
					expire()
				}
				return hit, err
			}
			f.deps.load = func(ctx context.Context, path string) (*transcript.Session, transcript.Snapshot, error) {
				s, snap, err := f.cache.Session(ctx, path)
				if phase == "load" {
					expire()
				}
				return s, snap, err
			}
			f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
				if phase == "facts" {
					expire()
				}
				return navDetailFacts{leaseKnown: true}, nil
			}
			w := &detailTestWriter{onHeader: func() {
				if phase == "header" || phase == "header-conditional" {
					expire()
				}
				if phase == "parent-header" {
					expire()
					cancelParent()
				}
			}}
			h := f.handler(t)
			request := detailTestRequest()
			if phase == "header-conditional" {
				initial := detailTestServe(h, detailTestRequest())
				detailTestOK(t, initial)
				request = detailTestConditional(initial.Header().Get("ETag"))
			}
			h.ServeHTTP(w, request.WithContext(parent))
			code := "preparing"
			if phase == "parent-header" {
				code = "request_cancelled"
			}
			detailTestError(t, w, 503, code)
			if phase == "admission" && resolveCalls.Load() != 0 {
				t.Fatal("already expired request performed lookup")
			}
		})
	}
}

func TestNavDetailRequestSlotAndWriterLifetime(t *testing.T) {
	for _, outcome := range []string{"success", "error", "short", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			f := detailTestNew(t)
			f.limits.requests = 1
			var lookups atomic.Int32
			resolve := f.deps.resolve
			f.deps.resolve = func(c context.Context, p, id string) (transcript.Hit, error) {
				lookups.Add(1)
				return resolve(c, p, id)
			}
			contexts := make(chan struct{}, 4)
			f.deps.requestContext = func(p context.Context) (context.Context, context.CancelFunc) {
				contexts <- struct{}{}
				return context.WithCancel(p)
			}
			h := f.handler(t)
			writing, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var wg sync.WaitGroup
			parent, cancel := context.WithCancel(context.Background())
			defer func() { cancel(); unblock(); wg.Wait() }()
			w := &detailTestWriter{onWrite: func() { close(writing); <-release }, writePanic: outcome == "panic", short: outcome == "short"}
			if outcome == "error" {
				w.writeErr = io.ErrClosedPipe
			}
			done := make(chan any, 1)
			wg.Add(1)
			go func() { defer wg.Done(); defer func() { done <- recover() }(); h.ServeHTTP(w, detailTestRequest()) }()
			<-contexts
			select {
			case <-writing:
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not start")
			}
			if len(h.slots) != 1 {
				t.Fatal("request admission released before Write returned")
			}
			waiter := make(chan *detailTestWriter, 1)
			wg.Add(1)
			go func() { defer wg.Done(); waiter <- detailTestServe(h, detailTestRequest().WithContext(parent)) }()
			<-contexts
			cancel()
			select {
			case got := <-waiter:
				detailTestError(t, got, 503, "request_cancelled")
			case <-time.After(2 * time.Second):
				t.Fatal("waiting request ignored cancellation")
			}
			if lookups.Load() != 1 {
				t.Fatal("slot waiter performed lookup")
			}
			unblock()
			select {
			case recovered := <-done:
				if (recovered != nil) != (outcome == "panic") {
					t.Fatal("writer panic classification")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not unwind")
			}
			if w.writes != 1 {
				t.Fatal("transport failure attempted second body")
			}
			detailTestOK(t, detailTestServe(h, detailTestRequest()))
		})
	}
}

func TestNavDetailAbortedInitiatorKeepsSharedBuild(t *testing.T) {
	f := detailTestNew(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var projects atomic.Int32
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		if projects.Add(1) == 1 {
			close(entered)
			<-release
		}
		return projectNavConversation(s, o)
	}
	var factsCalls atomic.Int32
	secondFacts := make(chan struct{})
	f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
		if factsCalls.Add(1) == 2 {
			close(secondFacts)
		}
		return navDetailFacts{leaseKnown: true}, nil
	}
	h := f.handler(t)
	parent, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); unblock(); wg.Wait() }()
	first, second := make(chan *detailTestWriter, 1), make(chan *detailTestWriter, 1)
	wg.Add(1)
	go func() { defer wg.Done(); first <- detailTestServe(h, detailTestRequest().WithContext(parent)) }()
	<-entered
	wg.Add(1)
	go func() { defer wg.Done(); second <- detailTestServe(h, detailTestRequest()) }()
	<-secondFacts
	cancel()
	select {
	case got := <-first:
		detailTestError(t, got, 503, "request_cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("initiator did not cancel")
	}
	unblock()
	select {
	case got := <-second:
		detailTestOK(t, got)
	case <-time.After(2 * time.Second):
		t.Fatal("surviving waiter lost detached builder")
	}
	if projects.Load() != 1 {
		t.Fatal("shared build repeated after initiator cancellation")
	}
}
