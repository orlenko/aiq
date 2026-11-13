package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type navAuthHTTPCopyKey struct{}

func TestNavAuthHTTPParserOwnerThroughCopies(t *testing.T) {
	// This complete fixture uses only the PRE-correction API on both runs.
	for _, action := range []string{"issue", "exchange"} {
		for _, copyKind := range []string{"WithContext", "Clone"} {
			for _, trailer := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/trailer=%t", action, copyKind, trailer), func(t *testing.T) {
					f := newNavAuthHTTPFixture(t)
					body := `{"action":"issue"}`
					if action == "exchange" {
						body = `{"action":"exchange","code":"` + navAuthHTTPCode(t, f) + `"}`
					}
					_, proof := f.request(t, "POST", "/api/nav/login", body, true)
					ending := "\r\n"
					if trailer {
						ending = "X-Invented-Trailer: value\r\n\r\n"
					}
					wire := fmt.Sprintf("POST /api/nav/login HTTP/1.1\r\nHost: 127.0.0.1:54321\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nX-Aiq-Proof: %s\r\n\r\n%x\r\n%s\r\n0\r\n%s", proof.String(), len(body), body, ending)
					original, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
					if err != nil {
						t.Fatal(err)
					}
					defer original.Body.Close()
					if original.Trailer != nil {
						t.Fatal("fixture did not establish initially nil original trailers")
					}
					issue, exchange, sign := navAuthHTTPCountActions(f)
					// Deliberately use a PLAIN gate; a fixture ingress would mask the old bug.
					gate := &navGate{verifier: f.a, login: http.HandlerFunc(f.a.Login)}
					h := navRecover(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						ctx := context.WithValue(r.Context(), navAuthHTTPCopyKey{}, "invented")
						if copyKind == "Clone" {
							r = r.Clone(ctx)
						} else {
							r = r.WithContext(ctx)
						}
						gate.ServeHTTP(w, r)
					}))
					w := &navAuthHTTPWriter{header: make(http.Header)}
					h.ServeHTTP(w, original)
					if trailer {
						if len(original.Trailer) != 1 {
							t.Error("original parser did not publish EOF trailer")
						}
						if w.status != 400 || string(w.body) != `{"error":"invalid_request"}` || *issue != 0 || *exchange != 0 || *sign != 0 {
							t.Errorf("copied request hid parser trailers: status%d actions%d/%d sign%d", w.status, *issue, *exchange, *sign)
						}
						fresh, _ := f.request(t, "POST", "/api/nav/login", body, false)
						fresh.Header.Set("X-Aiq-Proof", proof.String())
						positive := httptest.NewRecorder()
						gate.ServeHTTP(positive, fresh)
						if positive.Code != 200 {
							t.Error("copied-parser refusal consumed proof/code")
						}
					} else {
						if w.status != 200 {
							t.Error("owned trailer-free chunking refused")
						}
						if action == "issue" && (*issue != 1 || *exchange != 0 || *sign != 1) || action == "exchange" && (*issue != 0 || *exchange != 1 || *sign != 0) {
							t.Error("positive action counts changed")
						}
					}
					if w.headers != 1 || w.statuses != 1 || w.writes != 1 {
						t.Error("copied request attempted multiple output sequences")
					}
				})
			}
		}
	}
}
