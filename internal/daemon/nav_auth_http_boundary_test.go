package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/navauth"
)

func navAuthHTTPCode(t *testing.T, f *navAuthHTTPFixture) string {
	t.Helper()
	r, p := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, "")
	var reply struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || len(reply.Code) != 43 {
		t.Fatal("code fixture failed")
	}
	return reply.Code
}

func navAuthHTTPCountActions(f *navAuthHTTPFixture) (issue, exchange, sign *int) {
	i, e, s := 0, 0, 0
	originalIssue, originalExchange, originalSign := f.a.issue, f.a.exchange, f.a.sign
	f.a.issue = func(v *navauth.Facts) (string, error) { i++; return originalIssue(v) }
	f.a.exchange = func(code string) (string, error) { e++; return originalExchange(code) }
	f.a.sign = func(v *navauth.Facts, status int, body []byte) ([32]byte, error) {
		s++
		return originalSign(v, status, body)
	}
	return &i, &e, &s
}

func TestNavAuthHTTPRefusesLateTrailers(t *testing.T) {
	// Use the actual chunk parser. The same fixture compiles on the old adapter.
	for _, action := range []string{"issue", "exchange"} {
		t.Run(action, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			body := `{"action":"issue"}`
			if action == "exchange" {
				body = `{"action":"exchange","code":"` + navAuthHTTPCode(t, f) + `"}`
			}
			_, proof := f.request(t, "POST", "/api/nav/login", body, true)
			wire := fmt.Sprintf("POST /api/nav/login HTTP/1.1\r\nHost: 127.0.0.1:54321\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nX-Aiq-Proof: %s\r\n\r\n%x\r\n%s\r\n0\r\nX-Invented-Trailer: value\r\n\r\n", proof.String(), len(body), body)
			r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			if len(r.Trailer) != 0 || r.ContentLength != -1 {
				t.Fatal("parser did not establish initially undeclared trailers")
			}
			i, e, s := navAuthHTTPCountActions(f)
			w := &navAuthHTTPWriter{header: make(http.Header)}
			f.gate.ServeHTTP(w, r)
			if len(r.Trailer) != 1 {
				t.Error("fixture did not reach EOF trailer transition")
			}
			if w.status != 400 || string(w.body) != `{"error":"invalid_request"}` || w.headers != 1 || w.statuses != 1 || w.writes != 1 || *i != 0 || *e != 0 || *s != 0 {
				t.Errorf("late trailer admitted or acted: status%d actions%d/%d signs%d output%d/%d/%d", w.status, *i, *e, *s, w.headers, w.statuses, w.writes)
			}
			// A fresh trailer-free request with the SAME proof/code must succeed.
			clean, _ := f.request(t, "POST", "/api/nav/login", body, false)
			clean.Header.Set("X-Aiq-Proof", proof.String())
			positive := httptest.NewRecorder()
			f.gate.ServeHTTP(positive, clean)
			if positive.Code != 200 {
				t.Error("refusal consumed nonce or exchange code")
			}
		})
	}
	for _, key := range []string{"Trailer", "trailer"} {
		f := newNavAuthHTTPFixture(t)
		r, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
		r.Header[key] = []string{""}
		w := httptest.NewRecorder()
		f.gate.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Error("empty trailer header declaration accepted")
		}
	}
	// Real chunked/no-trailer control rules out simply refusing all chunking.
	f := newNavAuthHTTPFixture(t)
	body := `{"action":"issue"}`
	_, p := f.request(t, "POST", "/api/nav/login", body, true)
	wire := fmt.Sprintf("POST /api/nav/login HTTP/1.1\r\nHost: 127.0.0.1:54321\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nX-Aiq-Proof: %s\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", p.String(), len(body), body)
	r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, "")
}

func TestNavAuthHTTPLoginInvocationReentry(t *testing.T) {
	for _, action := range []string{"issue", "exchange"} {
		for _, mode := range []string{"nested", "repeat", "cross adapter", "proof then nested", "nested then proof", "generic busy"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				f := newNavAuthHTTPFixture(t)
				other := newNavAuthHTTPFixture(t)
				body := `{"action":"issue"}`
				if action == "exchange" {
					body = `{"action":"exchange","code":"` + navAuthHTTPCode(t, f) + `"}`
				}
				r, p := f.request(t, "POST", "/api/nav/login", body, true)
				i, e, s := navAuthHTTPCountActions(f)
				oi, oe, os := navAuthHTTPCountActions(other)
				w := &navAuthHTTPWriter{header: make(http.Header)}
				nestedHeaders, nestedStatuses, nestedWrites := 0, 0, 0
				reader := &navAuthHTTPReader{body: strings.NewReader(body)}
				reader.before = func() {
					if mode == "proof then nested" {
						if f.a.Proof(r) {
							t.Error("busy proof admitted")
						}
					}
					if mode == "generic busy" {
						if _, ok := f.a.ownedBody(r); ok {
							t.Error("busy snapshot admitted")
						}
						return
					}
					nested := f.gate
					if mode == "cross adapter" {
						nested = other.gate
					}
					nested.ServeHTTP(w, r)
					if mode == "repeat" {
						nested.ServeHTTP(w, r)
					}
					nestedHeaders, nestedStatuses, nestedWrites = w.headers, w.statuses, w.writes
					if mode == "nested then proof" {
						if f.a.Proof(r) {
							t.Error("busy proof admitted")
						}
					}
				}
				r.Body = reader
				f.gate.ServeHTTP(w, r)
				if nestedHeaders != 0 || nestedStatuses != 0 || nestedWrites != 0 {
					t.Error("nested invocation attempted output")
				}
				if w.status != 400 || string(w.body) != `{"error":"invalid_request"}` || w.headers != 1 || w.statuses != 1 || w.writes != 1 {
					t.Errorf("not exactly one original refusal: status%d output%d/%d/%d", w.status, w.headers, w.statuses, w.writes)
				}
				if *i != 0 || *e != 0 || *s != 0 || *oi != 0 || *oe != 0 || *os != 0 {
					t.Error("interrupted invocation performed privileged work")
				}
				reads := reader.reads
				if f.a.Proof(r) || reader.reads != reads {
					t.Error("interrupted snapshot admitted or reread")
				}
				clean, _ := f.request(t, "POST", "/api/nav/login", body, false)
				clean.Header.Set("X-Aiq-Proof", p.String())
				positive := httptest.NewRecorder()
				f.gate.ServeHTTP(positive, clean)
				if positive.Code != 200 {
					t.Error("interrupted invocation consumed nonce/code")
				}
			})
		}
	}
}
