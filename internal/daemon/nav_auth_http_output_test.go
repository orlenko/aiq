package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navauth"
)

// Header fails only on its FIRST invocation: a fallback would visibly succeed.
type navAuthHTTPWriter struct {
	header                    http.Header
	mode                      string
	value                     any
	headers, statuses, writes int
	status                    int
	body                      []byte
}

func (w *navAuthHTTPWriter) Header() http.Header {
	w.headers++
	if w.mode == "header" && w.headers == 1 {
		panic(w.value)
	}
	return w.header
}
func (w *navAuthHTTPWriter) WriteHeader(status int) {
	w.statuses++
	w.status = status
	if w.mode == "status" {
		panic(w.value)
	}
}
func (w *navAuthHTTPWriter) Write(body []byte) (int, error) {
	w.writes++
	if w.mode == "write" {
		panic(w.value)
	}
	if w.mode == "partial panic" {
		w.body = append(w.body, body[:1]...)
		panic(w.value)
	}
	if w.mode == "error" {
		return 0, errors.New("invented transport failure")
	}
	if w.mode == "short" {
		w.body = append(w.body, body[:1]...)
		return 1, nil
	}
	w.body = append(w.body, body...)
	return len(body), nil
}

func TestNavAuthHTTPOutputIsTerminal(t *testing.T) {
	known, unknown := navAuthHTTPOrigin(), diagnostic.Capture(nil)
	for _, mode := range []string{"header", "status", "write", "partial panic", "error", "short"} {
		for _, seed := range []diagnostic.Origin{{}, known, unknown} {
			for _, abort := range []bool{false, true} {
				t.Run(mode+"/seed-"+strconv.FormatBool(seed.Present())+"/abort-"+strconv.FormatBool(abort), func(t *testing.T) {
					f := newNavAuthHTTPFixture(t)
					r, _ := f.request(t, "GET", "/api/nav/info", "", true)
					ctx, holder := diagnostic.WithHolder(r.Context())
					*r = *r.WithContext(ctx)
					holder.Note(seed)
					var value any = diagnostic.Failure(diagnostic.Unreadable, known)
					if abort {
						value = http.ErrAbortHandler
					}
					w := &navAuthHTTPWriter{header: make(http.Header), mode: mode, value: value}
					signs := 0
					originalSign := f.a.sign
					f.a.sign = func(facts *navauth.Facts, status int, body []byte) ([32]byte, error) {
						signs++
						return originalSign(facts, status, body)
					}
					caught, panicked := navAuthHTTPCatch(func() { f.gate.ServeHTTP(w, r) })
					if !panicked || caught != http.ErrAbortHandler {
						t.Fatal("output failure did not preserve exact abort")
					}
					wantOrigin := seed
					if !seed.Present() && !abort && mode != "error" && mode != "short" {
						wantOrigin = known
					}
					if holder.Origin() != wantOrigin {
						t.Error("output changed first origin or fabricated transport fault")
					}
					wantStatus, wantWrites := 1, 1
					if mode == "header" {
						wantStatus, wantWrites = 0, 0
					} else if mode == "status" {
						wantWrites = 0
					}
					if w.headers != 1 || w.statuses != wantStatus || w.writes != wantWrites || signs != 1 {
						t.Errorf("output/preparation retried: %d/%d/%d signs %d", w.headers, w.statuses, w.writes, signs)
					}
				})
			}
		}
	}
}

type navAuthHTTPBadContext struct{ context.Context }

func (navAuthHTTPBadContext) Value(any) any { panic("invented context failure") }

func TestNavAuthHTTPSecondaryObservationFailure(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/nav/info", nil)
	r = r.WithContext(navAuthHTTPBadContext{r.Context()})
	w := &navAuthHTTPWriter{header: make(http.Header), mode: "header", value: "invented output panic"}
	caught, panicked := navAuthHTTPCatch(func() { navAuthWrite(w, r, navAuthReply{status: 200, body: []byte(`{}`)}) })
	if !panicked || caught != http.ErrAbortHandler || w.headers != 1 || w.statuses != 0 || w.writes != 0 {
		t.Error("secondary reducer failure escaped or retried output")
	}
}

func TestNavAuthHTTPLostExchangeResponseConsumesLink(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	r, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	// Read the issued code using the closed response's single field.
	code := strings.TrimSuffix(strings.TrimPrefix(w.Body.String(), `{"code":"`), `"}`)
	body := `{"action":"exchange","code":"` + code + `"}`
	r, _ = f.request(t, "POST", "/api/nav/login", body, false)
	failed := &navAuthHTTPWriter{header: make(http.Header), mode: "error"}
	caught, panicked := navAuthHTTPCatch(func() { f.gate.ServeHTTP(failed, r) })
	if !panicked || caught != http.ErrAbortHandler || failed.writes != 1 {
		t.Fatal("lost exchange did not abort")
	}
	r, _ = f.request(t, "POST", "/api/nav/login", body, false)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 || w.Body.String() != `{"error":"invalid_link"}` {
		t.Error("lost response resurrected one-use link")
	}
	// Recovery is a fresh issue operation, not automatic replay or rollback.
	r, _ = f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Error("fresh issue unavailable after lost exchange")
	}
}

func TestNavAuthHTTPRevocationClearsPendingLinks(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	r, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	code := strings.TrimSuffix(strings.TrimPrefix(w.Body.String(), `{"code":"`), `"}`)
	r, p := f.request(t, "POST", "/api/nav/revoke", `{}`, true)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, `{"ok":true}`)
	r, _ = f.request(t, "POST", "/api/nav/login", `{"action":"exchange","code":"`+code+`"}`, false)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Error("revoke retained pending code")
	}
}

func TestNavAuthHTTPBodyReadFailureAndPrivateMetadata(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	r, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	r.Body = io.NopCloser(navAuthHTTPErrorReader{})
	if f.a.Proof(r) {
		t.Error("partial body plus error admitted")
	}
	for _, info := range []navAuthInfo{{Root: strings.Repeat("x", navauth.MaxResponse+1)}, {Root: string([]byte{255})}, {Root: strings.Repeat("\x00", navauth.MaxResponse/2)}} {
		a, err := newNavAuthAdapter(f.key, f.binding, info)
		if err == nil || a != nil {
			if a != nil {
				a.Close()
			}
			t.Error("oversized/invalid metadata admitted")
		}
	}
	var absent *navAuthAdapter
	if absent.Credential("invented") || absent.Proof(r) {
		t.Error("nil adapter admitted authentication")
	}
	absent.Close()
	f.a.Close()
	r, _ = f.request(t, "GET", "/api/nav/hello", "", false)
	r.Header.Set("X-Aiq-Challenge", navauth.Encode32([32]byte{2}))
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 503 || w.Header().Get("X-Aiq-Hello-Proof") != "" {
		t.Error("retired authority signed hello")
	}
}

type navAuthHTTPErrorReader struct{}

func (navAuthHTTPErrorReader) Read(p []byte) (int, error) {
	copy(p, `{`)
	return 1, errors.New("invented read error")
}
