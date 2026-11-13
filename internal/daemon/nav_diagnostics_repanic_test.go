package daemon

import (
	"fmt"
	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navroute"
	"github.com/orlenko/aiq/internal/transcript"
	"io"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

type navRepanicHostile struct{ calls *atomic.Int64 }

func (v navRepanicHostile) Error() string          { v.calls.Add(1); return "invented private error" }
func (v navRepanicHostile) String() string         { v.calls.Add(1); return "invented private string" }
func (v navRepanicHostile) Format(fmt.State, rune) { v.calls.Add(1) }
func (v navRepanicHostile) Unwrap() error          { v.calls.Add(1); return nil }
func (v navRepanicHostile) Is(error) bool          { v.calls.Add(1); return false }

func TestNavDiagnosticsLegacyRepanicOuter(t *testing.T) {
	for _, mode := range []string{"legacy known", "legacy unknown", "core known", "core unknown", "typed nil", "hostile"} {
		t.Run(mode, func(t *testing.T) {
			a := navDiagnosticsOrigin()
			unknown := strings.Contains(mode, "unknown")
			var value any
			var want diagnostic.Origin
			var methods atomic.Int64
			if strings.HasPrefix(mode, "legacy") {
				restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) {
					if unknown {
						strings.Repeat("invented", -1)
					}
					navDiagnosticsLeaf()
					return nil, nil
				})
				_, err := transcript.ParseFile("invented-private-legacy-path.jsonl")
				restore()
				value = err
				want = transcript.PanicOrigin(err)
				if !want.Present() {
					t.Fatal("actual legacy adapter absent")
				}
			} else if mode == "typed nil" {
				restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) { navDiagnosticsLeaf(); return nil, nil })
				_, err := transcript.ParseFile("invented-private-legacy-path.jsonl")
				restore()
				// Fixture-only reflection constructs a typed nil of the ACTUAL opaque
				// adapter type without unsafe access or a shipped testing API.
				value = reflect.Zero(reflect.TypeOf(err)).Interface()
			} else if strings.HasPrefix(mode, "core") {
				want = a
				if unknown {
					want = diagnostic.Capture(nil)
				}
				value = diagnostic.Failure(diagnostic.Unreadable, want)
			} else {
				value = navRepanicHostile{&methods}
			}
			var lines []string
			observer := navDiagnosticsObserver(t, func(f string, args ...any) { lines = append(lines, fmt.Sprintf(f, args...)) })
			mux := navroute.NewMux()
			if err := mux.Handle(navroute.Sessions, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(value) })); err != nil {
				t.Fatal(err)
			}
			s := New(config.Default(), nil, log.New(io.Discard, "", 0))
			s.slots = newConnSlots(2, func(string, ...any) {})
			s.navVerifier = testVerifier{}
			s.navObservations = observer
			s.MountNavigator(mux)
			w := (navRequest{target: "/api/nav/sessions?invented=private", header: bearer(rightCredential)}).serve(s.navigatorServer().Handler)
			if w.Code != 500 || w.Body.Len() != 0 || len(lines) != 1 || methods.Load() != 0 {
				t.Fatal("re-panic changed response/count or queried arbitrary methods")
			}
			location := "unknown"
			if file, line, known := want.Location(); known {
				location = fmt.Sprintf("%s:%d", file, line)
			}
			if mode == "hostile" || mode == "typed nil" {
				if !strings.HasPrefix(lines[0], `navigator: panic report on "GET /api/nav/sessions" at internal/daemon/nav_diagnostics_repanic_test.go:`) {
					t.Fatal("raw hostile origin lost")
				}
			} else if lines[0] != `navigator: panic report on "GET /api/nav/sessions" at `+location {
				t.Fatal("outer re-panic lost original known/unknown legacy source")
			}
			for _, private := range []string{"invented-private-legacy-path", "invented=private", "private error", "navRepanicHostile"} {
				if strings.Contains(lines[0], private) {
					t.Fatal("private payload/path/type entered report")
				}
			}
		})
	}
}
