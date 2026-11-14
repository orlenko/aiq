package daemon

import (
	"context"
	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navroute"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type navSocketFaultKey struct{}
type navSocketFaultWriter struct {
	http.ResponseWriter
	kind  string
	armed bool
}

func (w *navSocketFaultWriter) Header() http.Header {
	if w.armed && w.kind == "header" {
		panic("invented secondary Header fault")
	}
	return w.ResponseWriter.Header()
}
func (w *navSocketFaultWriter) WriteHeader(code int) {
	if w.armed && w.kind == "status" {
		panic("invented secondary status fault")
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *navSocketFaultWriter) Flush()                      { w.ResponseWriter.(http.Flusher).Flush() }
func (w *navSocketFaultWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestNavDiagnosticsSocketFaultMatrix(t *testing.T) {
	for _, mode := range []string{"sink", "header", "status", "started", "partial", "flushed"} {
		t.Run(mode, func(t *testing.T) {
			logged, serverLog := &lockedBuffer{}, &lockedBuffer{}
			var attempts, requests atomic.Int32
			observer := navDiagnosticsObserver(t, func(f string, args ...any) {
				attempts.Add(1)
				log.New(logged, "", 0).Printf(f, args...)
				panic("invented sink fault")
			})
			mux := navroute.NewMux()
			if err := mux.Handle(navroute.Sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) > 1 {
					io.WriteString(w, marker)
					return
				}
				// An actual earlier recovering operation contributes A; later transport B
				// and the panicking sink must neither replace A nor produce a second log.
				a := navDiagnosticsOrigin()
				diagnostic.Note(r.Context(), a)
				w.Header().Set("Content-Length", "100")
				proxy := r.Context().Value(navSocketFaultKey{}).(*navSocketFaultWriter)
				proxy.armed = true
				switch mode {
				case "started":
					w.WriteHeader(200)
				case "partial":
					io.WriteString(w, `{"partial":`)
				case "flushed":
					io.WriteString(w, `{"partial":`)
					w.(http.Flusher).Flush()
				}
				panic("invented private later handler fault")
			})); err != nil {
				t.Fatal(err)
			}
			s := New(config.Default(), nil, log.New(io.Discard, "", 0))
			s.slots = newConnSlots(2, func(string, ...any) {})
			s.navVerifier = testVerifier{}
			s.navObservations = observer
			s.MountNavigator(mux)
			server := s.navigatorServer()
			outer := server.Handler
			server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				proxy := &navSocketFaultWriter{ResponseWriter: w, kind: mode}
				r = r.WithContext(context.WithValue(r.Context(), navSocketFaultKey{}, proxy))
				outer.ServeHTTP(proxy, r)
			})
			server.ErrorLog = log.New(serverLog, "", 0)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- server.Serve(s.slots.listener(ln)) }()
			t.Cleanup(func() { server.Close(); <-done })
			resp, body, err := rawResponse(t, ln.Addr().String(), "/api/nav/sessions?private=invented", rightCredential)
			if mode == "sink" {
				if err != nil || resp.StatusCode != 500 || body != "" || resp.ContentLength > 0 {
					t.Fatalf("sink prevented intended500: %v %v %q", err, resp, body)
				}
			} else if err == nil {
				t.Fatalf("fault returned complete response: %v %q", resp, body)
			}
			expected := `navigator: panic report on "GET /api/nav/sessions" at internal/daemon/nav_diagnostics_leaf_test.go:3` + "\n"
			if logged.String() != expected || attempts.Load() != 1 || serverLog.String() != "" {
				t.Fatalf("first-A/count/secondary log: attempts%d diagnostic%q server%q", attempts.Load(), logged.String(), serverLog.String())
			}
			if resp, body, err := rawResponse(t, ln.Addr().String(), "/api/nav/sessions", rightCredential); err != nil || resp.StatusCode != 200 || body != marker {
				t.Fatalf("healthy next request: %v %v %q", err, resp, body)
			}
			if attempts.Load() != 1 || serverLog.String() != "" || strings.Contains(logged.String(), "private") {
				t.Fatal("healthy request or secondary value added a report")
			}
			server.Close()
			if st := s.slots.stats(); st.used != 0 {
				t.Fatalf("retained admission: %+v", st)
			}
		})
	}
}
