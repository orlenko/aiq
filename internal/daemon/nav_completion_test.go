package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
)

func TestNavCompletionOwnership(t *testing.T) {
	var c navCompletion
	first, second := 0, 0
	if !c.adopt(func() {
		// Callback runs after terminal transition and outside the holder lock.
		c.mu.Lock()
		closed, cleared := c.closed, c.leave == nil
		c.mu.Unlock()
		if !closed || !cleared {
			t.Error("callback retained or terminal transition late")
		}
		if c.adopt(func() { second++ }) {
			t.Error("reentrant late adopt accepted")
		}
		c.close()
		first++
	}) {
		t.Fatal("initial adoption refused")
	}
	if c.adopt(func() { second++ }) {
		t.Error("duplicate adoption accepted")
	}
	c.close()
	c.close()
	if first != 1 || second != 0 {
		t.Error("callback ownership duplicated")
	}
	var empty navCompletion
	empty.close()
	if empty.adopt(func() { t.Error("late callback invoked") }) {
		t.Error("closed owner adopted")
	}
	var absent *navCompletion
	if absent.adopt(func() {}) || c.adopt(nil) {
		t.Error("invalid adoption accepted")
	}
}

func TestNavCompletionConcurrentCloseAndAdoption(t *testing.T) {
	for range 100 {
		var c navCompletion
		var called atomic.Int32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if !c.adopt(func() { called.Add(1) }) {
				called.Add(1)
			}
		}()
		go func() { defer wg.Done(); c.close() }()
		wg.Wait()
		c.close()
		if called.Load() != 1 {
			t.Fatal("callback lost or duplicated across handoff/close")
		}
	}
}

func navCompletionSlot(t *testing.T) (*connSlots, *slotConn, *http.Request) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	s := newConnSlots(1, func(string, ...any) {})
	c := &slotConn{Conn: a, slots: s, mark: 1}
	s.used = 1
	s.seq = 1
	s.conns[c] = struct{}{}
	r := httptest.NewRequest("GET", "/api/nav/sessions", nil)
	r = r.WithContext(context.WithValue(r.Context(), slotKey{}, c))
	return s, c, r
}

func TestNavCompletionSlotFallbackAndPhysicalClose(t *testing.T) {
	for _, mode := range []string{"absent", "closed", "duplicate", "transferred", "physical close", "chosen", "no slot"} {
		t.Run(mode, func(t *testing.T) {
			s, c, r := navCompletionSlot(t)
			completion := &navCompletion{}
			if mode != "absent" {
				r = r.WithContext(context.WithValue(r.Context(), navCompletionKey{}, completion))
			}
			other := 0
			if mode == "closed" {
				completion.close()
			}
			if mode == "duplicate" {
				completion.adopt(func() { other++ })
			}
			if mode == "chosen" {
				c.chosen = true
				s.retiring = 1
			}
			if mode == "no slot" {
				r = r.WithContext(context.Background())
			}
			called := 0
			h := s.handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				called++
				if mode == "physical close" {
					c.Close()
				}
			}), nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			wantActive := mode == "transferred" || mode == "physical close"
			if c.inHandler != wantActive {
				t.Error("wrong inner lifetime/fallback")
			}
			if mode == "chosen" && (called != 0 || w.Code != 503) || mode != "chosen" && called != 1 {
				t.Error("selection refusal/no-slot behavior changed")
			}
			completion.close()
			completion.close()
			if c.inHandler {
				t.Error("outer completion retained busy flag")
			}
			if mode == "duplicate" && other != 1 {
				t.Error("first completion callback overwritten")
			}
			if mode == "physical close" && (s.used != 0 || len(s.conns) != 0) {
				t.Error("completion resurrected physical connection")
			}
			if mode != "chosen" && mode != "no slot" && s.seq != 2 {
				t.Error("leave did not mark exactly once")
			}
		})
	}
}

type navCompletionWriter struct {
	*httptest.ResponseRecorder
	slot   *slotConn
	fail   string
	checks int
}

func (w *navCompletionWriter) check() {
	w.checks++
	if !w.slot.inHandler {
		panic("slot released before terminal writer")
	}
}
func (w *navCompletionWriter) Header() http.Header {
	w.check()
	if w.fail == "header" {
		panic("invented Header failure")
	}
	return w.ResponseRecorder.Header()
}
func (w *navCompletionWriter) WriteHeader(code int) {
	w.check()
	if w.fail == "status" {
		panic("invented status failure")
	}
	w.ResponseRecorder.WriteHeader(code)
}
func (w *navCompletionWriter) Write(body []byte) (int, error) {
	w.check()
	if w.fail == "write" {
		panic("invented Write failure")
	}
	return w.ResponseRecorder.Write(body)
}
func (w *navCompletionWriter) Flush() {
	w.check()
	if w.fail == "flush" {
		panic("invented Flush failure")
	}
	w.ResponseRecorder.Flush()
}

func TestNavCompletionEnclosesObservationAndTransport(t *testing.T) {
	for _, mode := range []string{"success", "ordinary origin", "panic", "pure abort", "seeded abort", "header", "status", "write", "partial", "flush"} {
		t.Run(mode, func(t *testing.T) {
			s, c, r := navCompletionSlot(t)
			reports := 0
			o := navDiagnosticsObserver(t, func(string, ...any) {
				reports++
				if !c.inHandler {
					t.Error("slot released before observer")
				}
			})
			w := &navCompletionWriter{ResponseRecorder: httptest.NewRecorder(), slot: c, fail: mode}
			app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "ordinary origin" || mode == "seeded abort" {
					diagnostic.Note(r.Context(), navDiagnosticsOrigin())
				}
				switch mode {
				case "pure abort", "seeded abort":
					panic(http.ErrAbortHandler)
				case "panic", "header", "status":
					navDiagnosticsLeaf()
				case "write":
					w.Write([]byte("invented"))
				case "partial":
					w.Write([]byte("partial"))
					navDiagnosticsLeaf()
				case "flush":
					w.(http.Flusher).Flush()
				default:
					w.WriteHeader(204)
				}
			})
			var caught any
			func() { defer func() { caught = recover() }(); navRecover(o, s.handler(app, nil)).ServeHTTP(w, r) }()
			wantAbort := mode == "pure abort" || mode == "seeded abort" || mode == "header" || mode == "status" || mode == "write" || mode == "partial" || mode == "flush"
			if wantAbort && caught != http.ErrAbortHandler || !wantAbort && caught != nil {
				t.Error("wrong outer abort/recovery")
			}
			wantReports := 1
			if mode == "success" || mode == "pure abort" {
				wantReports = 0
			}
			if reports != wantReports || c.inHandler || s.seq != 2 {
				t.Errorf("completion/report ownership: reports%d busy%t seq%d", reports, c.inHandler, s.seq)
			}
		})
	}
}
