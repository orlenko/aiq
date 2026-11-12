package transcript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Force a row-publication fault after a successful parse. The invalid limit is
// deliberate fault injection, not an ordinary transcript input or supported config.
func TestSummaryOuterPanicDoesNotQuarantineParser(t *testing.T) {
	for _, cancelWaiter := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled-%v", cancelWaiter), func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			started, release := make(chan string, 1), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			f := &fakeParse{started: started, release: release}
			c, _ := testCache(t, f, func(cfg *cacheConfig) { cfg.width, cfg.rowCount = 1, -1 })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := c.Summary(ctx, path); result <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("parse did not start")
			}
			if cancelWaiter {
				cancel()
			} else {
				finish()
			}
			select {
			case err := <-result:
				if cancelWaiter {
					if !errors.Is(err, ErrBudget) {
						t.Errorf("cancelled waiter: %v", err)
					}
				} else if !errors.Is(err, ErrUnreadable) || strings.Contains(err.Error(), "panic") {
					t.Errorf("outer failure was not sanitized: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiter did not finish")
			}
			finish()
			// Acquiring the only slot synchronizes with the worker's deferred release.
			select {
			case c.sem <- struct{}{}:
			case <-time.After(time.Second):
				t.Fatal("worker retained its slot after outer recovery")
			}
			c.cfg.rowCount = defaultCacheConfig.rowCount
			<-c.sem
			c.mu.Lock()
			_, quarantined := c.quarantine[path]
			c.mu.Unlock()
			if quarantined {
				t.Error("a row-publication failure quarantined a successful parser")
			}
			if _, err := c.Summary(context.Background(), path); err != nil || f.n.Load() != 2 {
				t.Errorf("same snapshot could not retry after outer recovery: %v, parses %d", err, f.n.Load())
			}
		})
	}
}

func TestSummaryCancelAfterAdmission(t *testing.T) {
	for _, kind := range []string{"success", "empty", "error", "panic"} {
		t.Run(kind, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
			c, _ := testCache(t, &fakeParse{}, func(cfg *cacheConfig) { cfg.width = 1 })
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			finish := func() { once.Do(func() { close(release) }) }
			defer finish()
			c.parse = func(string) (*Session, error) {
				close(started)
				<-release
				switch kind {
				case "empty":
					return nil, nil
				case "error":
					return nil, os.ErrPermission
				case "panic":
					panic("invented parser failure")
				}
				return &Session{Provider: "claude", ID: "fixture", Cwd: "/w", Path: path, Turns: []Turn{{Prompt: "invented"}}}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := c.Summary(ctx, path); result <- err }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("parse did not start")
			}
			cancel()
			returned := false
			select {
			case err := <-result:
				returned = true
				if !errors.Is(err, ErrBudget) {
					t.Errorf("cancelled summary: %v", err)
				}
			case <-time.After(200 * time.Millisecond):
				t.Error("summary waited for its admitted parser after cancellation")
			}
			if len(c.sem) != 1 {
				t.Error("slot freed while admitted parser still runs")
			}
			finish()
			if !returned {
				select {
				case <-result:
				case <-time.After(time.Second):
					t.Fatal("summary never returned")
				}
			}
			deadline := time.Now().Add(time.Second)
			for len(c.sem) != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if len(c.sem) != 0 {
				t.Fatal("completed parser retained slot")
			}
			stats := c.Stats()
			if stats.Parsed != 0 {
				t.Errorf("summary populated full parse cache: %+v", stats)
			}
			wantRows := 0
			if kind == "success" || kind == "empty" {
				wantRows = 1
			}
			if stats.Rows != wantRows {
				t.Errorf("rows %d, want %d", stats.Rows, wantRows)
			}
			if kind == "success" {
				if sum, err := c.Summary(context.Background(), path); err != nil || sum.ID != "fixture" || c.Stats().Parses != 1 {
					t.Errorf("completed row not reusable: %+v %v", sum, err)
				}
			}
		})
	}
}

type cancelListSource struct {
	Discovery
	path string
}

func (s cancelListSource) Candidates(context.Context, string) ([]Candidate, error) {
	return []Candidate{{Provider: "claude", Path: s.path}}, nil
}

func TestCanceledLastSummaryMakesListPartial(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "fixture.jsonl"), "invented")
	started, release := make(chan string, 1), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	c, _ := testCache(t, &fakeParse{started: started, release: release}, func(cfg *cacheConfig) { cfg.width = 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan ListResult, 1)
	go func() {
		got, _ := ListWith(ctx, cancelListSource{Discovery: DirectWalk(Roots{}), path: path}, "/w", c.Summary)
		result <- got
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("last parse did not start")
	}
	cancel()
	select {
	case got := <-result:
		if !got.Partial || got.Of != 1 || got.Scanned != 0 {
			t.Errorf("cancelled last candidate: %+v", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("list waited for last admitted parser after cancellation")
		finish()
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("list never returned")
		}
	}
	finish()
}
