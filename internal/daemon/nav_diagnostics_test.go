package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navroute"
)

func navDiagnosticsOrigin() (out diagnostic.Origin) {
	defer func() {
		if value := recover(); value != nil {
			out = diagnostic.Capture(value)
		}
	}()
	navDiagnosticsLeaf()
	return
}
func navDiagnosticsObserver(t *testing.T, sink func(string, ...any)) *navObserver {
	t.Helper()
	o := newNavObserver(navroute.Catalog(), sink, nil, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if !o.Close(ctx) {
			t.Error("observer did not join")
		}
	})
	return o
}
func navDiagnosticsInvoke(h http.Handler, w http.ResponseWriter) (caught any) {
	defer func() { caught = recover() }()
	r := httptest.NewRequest("GET", "/api/nav/sessions?private=invented", nil)
	r.Pattern = "spoofed input"
	h.ServeHTTP(w, r)
	return
}

func TestNavDiagnosticsCompletionAndExactAbort(t *testing.T) {
	known, unknown := navDiagnosticsOrigin(), diagnostic.Capture(nil)
	for _, tc := range []struct {
		name              string
		first             diagnostic.Origin
		abort, laterPanic bool
		want              int
	}{
		{"healthy", diagnostic.Origin{}, false, false, 0},
		{"pure abort", diagnostic.Origin{}, true, false, 0},
		{"known normal", known, false, false, 1},
		{"unknown normal", unknown, false, false, 1},
		{"known abort", known, true, false, 1},
		{"unknown abort", unknown, true, false, 1},
		{"first known before later panic", known, false, true, 1},
		{"first unknown before later panic", unknown, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			o := navDiagnosticsObserver(t, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
			mux := navroute.NewMux()
			if err := mux.Handle(navroute.Sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				diagnostic.Note(r.Context(), tc.first)
				r.Pattern = "untrusted post-dispatch pattern"
				if tc.abort {
					panic(http.ErrAbortHandler)
				}
				if tc.laterPanic {
					panic("invented later private value")
				}
				w.WriteHeader(409)
			})); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			caught := navDiagnosticsInvoke(navRecover(o, mux), w)
			if tc.abort && caught != http.ErrAbortHandler || !tc.abort && caught != nil {
				t.Fatalf("wrong abort: %T", caught)
			}
			if len(lines) != tc.want {
				t.Fatalf("reports=%d want%d", len(lines), tc.want)
			}
			if tc.want != 0 {
				location := "unknown"
				if tc.first == known {
					location = "internal/daemon/nav_diagnostics_leaf_test.go:3"
				}
				if lines[0] != `navigator: panic report on "GET /api/nav/sessions" at `+location {
					t.Fatalf("wrong first/key: %q", lines)
				}
			}
			if !tc.abort && (tc.laterPanic && w.Code != 500 || !tc.laterPanic && w.Code != 409) {
				t.Fatalf("response=%d", w.Code)
			}
		})
	}
}

func TestNavDiagnosticsLimitedAbortCountsOnlyPriorFault(t *testing.T) {
	var lines []string
	o := navDiagnosticsObserver(t, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	a := navDiagnosticsOrigin()
	h := navRecover(o, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		diagnostic.Note(r.Context(), a)
		panic(http.ErrAbortHandler)
	}))
	for range 2 {
		if got := navDiagnosticsInvoke(h, httptest.NewRecorder()); got != http.ErrAbortHandler {
			t.Fatal("abort changed")
		}
	}
	pure := navRecover(o, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	if got := navDiagnosticsInvoke(pure, httptest.NewRecorder()); got != http.ErrAbortHandler {
		t.Fatal("pure abort changed")
	}
	o.mu.Lock()
	pending, normal := o.pending, o.normalCount
	o.mu.Unlock()
	if len(lines) != 1 || pending != 1 || normal != 1 {
		t.Fatalf("reports=%d pending=%d normal=%d", len(lines), pending, normal)
	}
}

type navDiagnosticsWriter struct {
	*httptest.ResponseRecorder
	headerFault, statusFault bool
	headers, statuses        int
}

func (w *navDiagnosticsWriter) Header() http.Header {
	w.headers++
	if w.headerFault {
		panic("invented private Header fault")
	}
	return w.ResponseRecorder.Header()
}
func (w *navDiagnosticsWriter) WriteHeader(code int) {
	w.statuses++
	if w.statusFault {
		panic("invented private status fault")
	}
	w.ResponseRecorder.WriteHeader(code)
}

func TestNavDiagnosticsSecondaryTransportAndSink(t *testing.T) {
	for _, header := range []bool{false, true} {
		t.Run(fmt.Sprint(header), func(t *testing.T) {
			var lines []string
			o := navDiagnosticsObserver(t, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)); panic("invented sink fault") })
			a := navDiagnosticsOrigin()
			h := navRecover(o, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				diagnostic.Note(r.Context(), a)
				panic("invented later fault")
			}))
			w := &navDiagnosticsWriter{ResponseRecorder: httptest.NewRecorder(), headerFault: header, statusFault: !header}
			if got := navDiagnosticsInvoke(h, w); got != http.ErrAbortHandler {
				t.Fatalf("secondary escape: %T", got)
			}
			if len(lines) != 1 || lines[0] != `navigator: panic report on "unregistered" at internal/daemon/nav_diagnostics_leaf_test.go:3` {
				t.Fatalf("origin/report: %q", lines)
			}
			if w.headers != 1 || w.statuses != map[bool]int{true: 0, false: 1}[header] {
				t.Fatalf("recovery repeated: Header%d status%d", w.headers, w.statuses)
			}
			if got := navDiagnosticsInvoke(navRecover(o, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })), httptest.NewRecorder()); got != nil {
				t.Fatal("healthy request failed")
			}
		})
	}
}

func TestNavDiagnosticsRetiredPreCoreFaultRealSocket(t *testing.T) {
	logged, serverLog := &lockedBuffer{}, &lockedBuffer{}
	s := New(config.Default(), nil, log.New(logged, "", 0))
	s.slots = newConnSlots(2, func(string, ...any) {})
	s.navObservations = navDiagnosticsObserver(t, s.log.Printf)
	s.navVerifier = testVerifier{}
	s.MountNavigator(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, marker) }))
	server := s.navigatorServer()
	baseContext := server.ConnContext
	var connections, requests atomic.Int32
	server.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		ctx = baseContext(ctx, c)
		sc, _ := ctx.Value(slotKey{}).(*slotConn)
		if sc == nil {
			panic("missing real slot")
		}
		if n := connections.Add(1); n == 1 || n == 3 {
			s.slots.mu.Lock()
			sc.chosen = true
			s.slots.retiring++
			s.slots.mu.Unlock()
		}
		return ctx
	}
	outer := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			outer.ServeHTTP(&navDiagnosticsWriter{ResponseRecorder: httptest.NewRecorder(), headerFault: true}, r)
			return
		}
		outer.ServeHTTP(w, r)
	})
	server.ErrorLog = log.New(serverLog, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := s.slots.listener(ln)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); <-done })
	if _, _, err := rawResponse(t, ln.Addr().String(), "/api/nav/hello", ""); err == nil {
		t.Fatal("persistent Header fault returned complete response")
	}
	if !strings.HasPrefix(logged.String(), `navigator: panic report on "unregistered" at internal/daemon/nav_diagnostics_test.go:`) || strings.Count(logged.String(), "\n") != 1 {
		t.Fatalf("fallback observation: %q", logged.String())
	}
	if serverLog.String() != "" {
		t.Fatalf("raw server log: %q", serverLog.String())
	}
	if resp, _, err := rawResponse(t, ln.Addr().String(), "/api/nav/hello", ""); err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthy next request: %v %v", err, resp)
	}
	if resp, body, err := rawResponse(t, ln.Addr().String(), "/api/nav/hello", ""); err != nil || resp.StatusCode != 503 || !resp.Close || body != "" {
		t.Fatalf("normal retired refusal: %v %v %q", err, resp, body)
	}
	server.Close()
	if st := s.slots.stats(); st.used != 0 {
		t.Fatalf("retained admission: %+v", st)
	}
}
