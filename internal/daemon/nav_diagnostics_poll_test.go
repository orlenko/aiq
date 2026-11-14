package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navroute"
	"github.com/orlenko/aiq/internal/transcript"
)

// These in-process requests use the actual production navigatorServer envelope,
// gate, registered mux and Cache.ViewContext. No socket or detail mixed-witness
// claim is made. navObs helpers come from the separately frozen observer tests.
func TestNavDiagnosticsRetainedViewPollObservations(t *testing.T) {
	for _, mode := range []string{"known", "unknown", "bare", "ordinary"} {
		t.Run(mode, func(t *testing.T) {
			driver, sink := navObsNewDriver(), navObsNewSink()
			var owner navObserverOwner // isolated; never reset the production owner
			observer := owner.acquire(navroute.Catalog(), sink.write, driver.now, driver.factory)
			driver.observer.Store(observer)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if !owner.Close(ctx) {
					t.Error("poll observer did not join")
				}
				driver.mu.Lock()
				bad := driver.factories != 1 || driver.dropped != 0 || driver.badDelay
				driver.mu.Unlock()
				if bad {
					t.Error("polling reconstructed timer or violated driver contract")
				}
			})
			driver.settle(t, observer)
			start := driver.now()
			cache := transcript.NewCache()
			key := transcript.ViewKey{Snap: transcript.Snapshot{Path: "invented-poll-view", Size: 1, Mtime: 1}, Last: 50}
			ordinary := errors.New("invented ordinary selection failure")
			var builds, executions, entered atomic.Int64
			mux := navroute.NewMux()
			if err := mux.Handle(navroute.Sessions, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered.Add(1)
				v, err := cache.ViewContext(r.Context(), key, func() (*transcript.View, error) {
					builds.Add(1)
					switch mode {
					case "known":
						executions.Add(1)
						navDiagnosticsLeaf()
					case "unknown":
						executions.Add(1)
						strings.Repeat("invented", -1) // real non-allowlisted leaf
					case "ordinary":
						return nil, ordinary
					}
					return nil, transcript.ErrUnshowable
				})
				if v != nil {
					t.Error("failure produced a View")
				}
				if mode == "ordinary" {
					if err != ordinary {
						t.Error("ordinary error identity changed")
					}
					w.WriteHeader(http.StatusConflict)
				} else {
					if !errors.Is(err, transcript.ErrUnshowable) || err.Error() != "transcript: could not be shown" {
						t.Error("fixed View failure changed")
					}
					origin := transcript.PanicOrigin(err)
					file, line, known := origin.Location()
					if mode == "known" && (!known || file != "internal/daemon/nav_diagnostics_leaf_test.go" || line != 3) || mode == "unknown" && (!origin.Present() || known) || mode == "bare" && (origin.Present() || err != transcript.ErrUnshowable) {
						t.Error("actual retained failure lost literal leaf/unknown/bare identity")
					}
					w.WriteHeader(http.StatusInternalServerError)
				}
				io.WriteString(w, "fixed fixture failure\n")
			})); err != nil {
				t.Fatal(err)
			}
			makeServer := func() (*Server, http.Handler) {
				o := owner.acquire(navroute.Catalog(), sink.write, driver.now, driver.factory)
				if o != observer {
					t.Fatal("server reconstruction replaced observer/credits")
				}
				s := &Server{log: log.New(io.Discard, "", 0), navVerifier: testVerifier{}, navObservations: o, slots: newConnSlots(2, func(string, ...any) {})}
				s.MountNavigator(mux)
				return s, s.navigatorServer().Handler
			}
			s1, current := makeServer()
			// A synthetic selected slot exercises production handler bookkeeping;
			// native admission/retirement is covered by the separate socket tests.
			active := &slotConn{slots: s1.slots}
			request := func(s *Server, h http.Handler, sc *slotConn, target, auth string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:50001"+target, nil)
				r.RequestURI = target
				r.RemoteAddr = "127.0.0.1:50000"
				r.Header.Set("Authorization", auth)
				r.Pattern = "untrusted spoof"
				r = r.WithContext(s.slots.connContext(r.Context(), sc))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				s.slots.mu.Lock()
				busy := sc.inHandler
				s.slots.mu.Unlock()
				if busy || s.slots.stats().used != 0 {
					t.Error("request left handler/admission bookkeeping active")
				}
				return w
			}
			poll := func(s *Server, h http.Handler, sc *slotConn) {
				w := request(s, h, sc, "/api/nav/sessions?private=invented", "Bearer "+rightCredential)
				want := http.StatusInternalServerError
				if mode == "ordinary" {
					want = http.StatusConflict
				}
				if w.Code != want || w.Body.String() != "fixed fixture failure\n" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") != navCSP || w.Header().Get("X-Aiq-Build") != navBuild() {
					t.Fatal("poll observation changed application response/guards")
				}
			}
			poll(s1, current, active) // one actual failing build, first observation
			s2, reconstructed := makeServer()
			active2 := &slotConn{slots: s2.slots}
			for i := 0; i < 4; i++ {
				poll(s2, reconstructed, active2)
			}
			fault := mode == "known" || mode == "unknown"
			if st := navObsRaw(observer); fault && (st.pending != 4 || st.normal != 1 || len(sink.all()) != 1) || !fault && (st.pending != 0 || st.normal != 0 || len(sink.all()) != 0) {
				t.Fatal("reconstructed server reset credits or counted executions instead of polls")
			}
			// Neither the actual refusal branch nor denied/canonical requests can
			// invoke the failure route or manufacture another observation.
			before := entered.Load()
			retired := &slotConn{slots: s1.slots, chosen: true}
			w := request(s1, current, retired, "/api/nav/sessions", "Bearer "+rightCredential)
			if w.Code != 503 || w.Body.Len() != 0 || w.Header().Get("Connection") != "close" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("retired connection response changed")
			}
			if request(s2, reconstructed, active2, "/api/nav/sessions", "").Code != 401 || request(s2, reconstructed, active2, "/api//nav/sessions", "Bearer "+rightCredential).Code != 400 || entered.Load() != before {
				t.Fatal("refusal/gate/canonical request entered application")
			}
			driver.set(start.Add(59999 * time.Millisecond))
			poll(s1, current, active)
			driver.settle(t, observer)
			if fault {
				if navObsRaw(observer).pending != 5 || len(sink.all()) != 1 {
					t.Fatal("per-key credit refreshed before sixty seconds")
				}
				mark := navObsFire(t, driver, start.Add(time.Minute))
				sink.line(t, "navigator: 5 further panic reports suppressed")
				driver.wait(t, mark, "park")
			} else {
				driver.set(start.Add(time.Minute))
			}
			for i := 0; i < 3; i++ {
				poll(s2, reconstructed, active2)
			}
			driver.settle(t, observer)
			if fault {
				if navObsRaw(observer).pending != 2 {
					t.Fatal("next window did not retain two suppressed polls")
				}
				mark := navObsFire(t, driver, start.Add(2*time.Minute))
				sink.line(t, "navigator: 2 further panic reports suppressed")
				driver.wait(t, mark, "park")
				location := "unknown"
				if mode == "known" {
					location = "internal/daemon/nav_diagnostics_leaf_test.go:3"
				}
				wantNormal := `navigator: panic report on "GET /api/nav/sessions" at ` + location
				lines := sink.all()
				if len(lines) != 4 || lines[0] != wantNormal || lines[1] != "navigator: 5 further panic reports suppressed" || lines[2] != wantNormal || lines[3] != "navigator: 2 further panic reports suppressed" || executions.Load() != 1 || builds.Load() != 1 {
					t.Fatal("nine observed polls must mean two normals plus seven suppressed, one execution")
				}
			} else if len(sink.all()) != 0 || navObsRaw(observer).pending != 0 || executions.Load() != 0 {
				t.Fatal("ordinary originless result produced a report or K")
			}
			st := cache.Stats()
			wantBuilds, wantFailures := int64(1), 1
			if mode == "ordinary" {
				wantBuilds, wantFailures = 9, 0 // ordinary errors remain retryable
			}
			if entered.Load() != 9 || builds.Load() != wantBuilds || st.ViewBuilds != wantBuilds || st.FailedViews != wantFailures || st.BuildingViews != 0 || st.Views != 0 || st.Parses != 0 || st.Parsed != 0 || st.Quarantined != 0 || navObsRaw(observer).pending != 0 {
				t.Fatal("polling rebuilt a retained failure, parsed/quarantined, or leaked a flight")
			}
		})
	}
}
