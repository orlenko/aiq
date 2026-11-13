package daemon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/transcript"
)

func navAuthHTTPUnknownLeaf() { reflect.Value{}.Interface() }
func navAuthHTTPUnknownOrigin() (origin diagnostic.Origin) {
	defer func() { origin = diagnostic.Capture(recover()) }()
	navAuthHTTPUnknownLeaf()
	return
}
func navAuthHTTPValueOrigin(value any) (origin diagnostic.Origin) {
	defer func() { origin = diagnostic.Capture(recover()) }()
	navAuthHTTPPanic(value)
	return
}
func navAuthHTTPLegacy(t *testing.T, leaf func()) error {
	t.Helper()
	restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) { leaf(); return nil, nil })
	defer restore()
	_, err := transcript.ParseFile("/invented/no-read.jsonl")
	if err == nil || !transcript.PanicOrigin(err).Present() {
		t.Fatal("legacy carrier fixture failed")
	}
	return err
}

func TestNavAuthHTTPExactOriginMatrix(t *testing.T) {
	known, unknown := navAuthHTTPOrigin(), navAuthHTTPUnknownOrigin()
	if !unknown.Present() {
		t.Fatal("unknown leaf absent")
	}
	if _, _, ok := unknown.Location(); ok {
		t.Fatal("reflect leaf unexpectedly allowlisted")
	}
	legacyKnown := navAuthHTTPLegacy(t, navAuthHTTPLeaf)
	legacyUnknown := navAuthHTTPLegacy(t, navAuthHTTPUnknownLeaf)
	if transcript.PanicOrigin(legacyKnown) != known || transcript.PanicOrigin(legacyUnknown) != unknown {
		t.Fatal("legacy origin precondition failed")
	}
	legacyNil := reflect.Zero(reflect.TypeOf(legacyKnown)).Interface().(error)
	coreNil := reflect.Zero(reflect.TypeOf(diagnostic.Failure(diagnostic.Unreadable, known))).Interface().(error)
	raw := navAuthHTTPValueOrigin(legacyNil)
	if file, line, ok := raw.Location(); !ok || file != "internal/daemon/nav_auth_http_origin_leaf_test.go" || line != 3 {
		t.Fatal("raw capture oracle changed")
	}
	cases := []struct {
		name string
		leaf func()
		want diagnostic.Origin
	}{
		{"legacy known", func() { navAuthHTTPPanic(legacyKnown) }, known},
		{"legacy unknown", func() { navAuthHTTPPanic(legacyUnknown) }, unknown},
		{"legacy nil", func() { navAuthHTTPPanic(legacyNil) }, raw},
		{"core nil", func() { navAuthHTTPPanic(coreNil) }, raw},
		{"raw known", navAuthHTTPLeaf, known},
		{"raw unknown", navAuthHTTPUnknownLeaf, unknown},
	}
	for _, c := range cases {
		for index, seed := range []diagnostic.Origin{{}, known, unknown} {
			t.Run(c.name+"/seed-"+strconv.Itoa(index), func(t *testing.T) {
				f := newNavAuthHTTPFixture(t)
				f.a.issue = func(*navauth.Facts) (string, error) { c.leaf(); return "", nil }
				r, p := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
				ctx, holder := diagnostic.WithHolder(r.Context())
				*r = *r.WithContext(ctx)
				holder.Note(seed)
				w := httptest.NewRecorder()
				f.gate.ServeHTTP(w, r)
				f.verify(t, w, p, 500, `{"error":"internal_error"}`)
				want := c.want
				if seed.Present() {
					want = seed
				}
				if holder.Origin() != want {
					t.Error("exact first origin changed")
				}
			})
		}
	}
	// Exercise the same concrete legacy formats at the first output call too.
	for _, c := range []struct {
		name  string
		value error
		want  diagnostic.Origin
	}{{"known", legacyKnown, known}, {"unknown", legacyUnknown, unknown}, {"nil", legacyNil, raw}} {
		for index, seed := range []diagnostic.Origin{{}, known, unknown} {
			t.Run("transport/"+c.name+"/seed-"+strconv.Itoa(index), func(t *testing.T) {
				f := newNavAuthHTTPFixture(t)
				r, _ := f.request(t, "GET", "/api/nav/info", "", true)
				ctx, holder := diagnostic.WithHolder(r.Context())
				*r = *r.WithContext(ctx)
				holder.Note(seed)
				w := &navAuthHTTPOriginWriter{value: c.value}
				value, panicked := navAuthHTTPCatch(func() { f.gate.ServeHTTP(w, r) })
				want := c.want
				if seed.Present() {
					want = seed
				}
				if !panicked || value != http.ErrAbortHandler || holder.Origin() != want || w.headers != 1 || w.statuses != 0 || w.writes != 0 {
					t.Error("legacy output recovery changed origin, sentinel or attempted output")
				}
			})
		}
	}
}

type navAuthHTTPOriginWriter struct {
	value                     error
	headers, statuses, writes int
}

func (w *navAuthHTTPOriginWriter) Header() http.Header {
	w.headers++
	navAuthHTTPPanic(w.value)
	return nil
}
func (w *navAuthHTTPOriginWriter) WriteHeader(int)                { w.statuses++ }
func (w *navAuthHTTPOriginWriter) Write(body []byte) (int, error) { w.writes++; return len(body), nil }

func TestNavAuthHTTPAdmittedFailuresAreSigned(t *testing.T) {
	for _, capacity := range []bool{true, false} {
		t.Run(strconv.FormatBool(capacity), func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			actions, signs := 0, 0
			originalSign := f.a.sign
			f.a.issue = func(*navauth.Facts) (string, error) {
				actions++
				if capacity {
					return "", navauth.ErrCapacity
				}
				return "", errors.New("invented ordinary issue failure")
			}
			f.a.sign = func(v *navauth.Facts, status int, body []byte) ([32]byte, error) {
				signs++
				return originalSign(v, status, body)
			}
			r, p := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
			w := httptest.NewRecorder()
			f.gate.ServeHTTP(w, r)
			status, body := 500, `{"error":"internal_error"}`
			if capacity {
				status, body = 409, `{"error":"login_busy"}`
			}
			f.verify(t, w, p, status, body)
			if actions != 1 || signs != 1 {
				t.Error("admitted failure repeated action/sign")
			}
			r, _ = f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, false)
			w = httptest.NewRecorder()
			f.gate.ServeHTTP(w, r)
			if w.Code != 401 || w.Body.Len() != 0 || w.Header().Get("X-Aiq-Response-Proof") != "" || actions != 1 || signs != 1 {
				t.Error("unadmitted request exposed signed capacity/failure")
			}
		})
	}
}
