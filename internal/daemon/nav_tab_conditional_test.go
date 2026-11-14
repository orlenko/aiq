package daemon

import (
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/navauth"
)

func conditionalTabHeader(t *testing.T, b navauth.Binding, target, condition, token string) navauth.ConditionalTabProof {
	t.Helper()
	raw, err := navauth.Decode32(token)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256(raw[:])
	p := navauth.ConditionalTabProof{Proof: navauth.TabProof{ID: navauth.TabID(key), Nonce: [32]byte{53}}}
	p.Proof.MAC, err = navauth.TabConditionalRequestMAC(key, b, "GET", target, condition, p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestNavConditionalTabHTTPHeaderBindingAndRetirement(t *testing.T) {
	for _, mode := range []string{"full", "not-modified", "request-error", "changed-condition", "duplicate", "alias", "empty-values", "v1-mac", "retired", "bad-etag", "etag-alias", "bad-mime", "bodyful304"} {
		t.Run(mode, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			token := tabFixtureCredential(t, f)
			target := "/api/nav/sessions?dir=%2Finvented"
			etag := "\"" + strings.Repeat("a", 64) + "\""
			status := 200
			body := []byte(`{"invented":true}`)
			condition := etag
			calls := 0
			if mode == "not-modified" || mode == "bodyful304" {
				status = 304
				if mode == "not-modified" {
					body = nil
				}
			}
			if mode == "request-error" {
				status = 503
				etag = ""
			}
			f.gate.api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				h := w.Header()
				h.Set("Content-Type", "application/json")
				if etag != "" {
					h.Set("ETag", etag)
				}
				switch mode {
				case "retired":
					f.a.Close()
				case "bad-etag":
					h.Set("ETag", "invented invalid")
				case "etag-alias":
					h["etag"] = []string{etag}
				case "bad-mime":
					h.Set("Content-Type", "text/plain")
				}
				WriteNavigatorBody(w, r, h, status, body)
			})
			r, _ := f.request(t, "GET", target, "", false)
			p := conditionalTabHeader(t, f.binding, target, condition, token)
			r.Header.Set("Authorization", p.String())
			r.Header.Set("If-None-Match", condition)
			switch mode {
			case "changed-condition":
				r.Header.Set("If-None-Match", condition+" ")
			case "duplicate":
				r.Header.Add("If-None-Match", condition)
			case "alias":
				r.Header["if-none-match"] = []string{condition}
			case "empty-values":
				r.Header["If-None-Match"] = nil
			case "v1-mac":
				r.Header.Set("Authorization", "AiqTab2 "+strings.TrimPrefix(tabTestHeader(t, f.binding, "GET", target, token), "AiqTab "))
			}
			w := httptest.NewRecorder()
			f.gate.ServeHTTP(w, r)
			valid := mode == "full" || mode == "not-modified" || mode == "request-error"
			if !valid {
				if w.Code != 401 || w.Body.Len() != 0 {
					t.Fatalf("refusal disclosed body: %d", w.Code)
				}
				if mode == "changed-condition" || mode == "duplicate" || mode == "alias" || mode == "empty-values" || mode == "v1-mac" {
					if calls != 0 {
						t.Fatal("invalid proof reached handler")
					}
				}
				return
			}
			if w.Code != status || calls != 1 {
				t.Fatal("valid conditional request refused")
			}
			raw, _ := navauth.Decode32(token)
			key := sha256.Sum256(raw[:])
			want, err := navauth.TabConditionalResponseMAC(key, f.binding, p, status, etag, w.Body.Bytes())
			got, e := navauth.Decode32(w.Header().Get("X-Aiq-Tab-Proof"))
			if err != nil || e != nil || !navauth.EqualMAC(want, got) {
				t.Fatal("conditional exact response proof mismatch")
			}
			if status == 304 && (w.Header().Get("Content-Type") != "" || w.Body.Len() != 0) {
				t.Fatal("304 wire metadata preparation")
			}
		})
	}
}

func TestNavConditionalTabRealHTTP304HasNoMIMEOrBody(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	token := tabFixtureCredential(t, f)
	tag := "\"" + strings.Repeat("b", 64) + "\""
	target := "/api/nav/sessions?dir=%2Finvented"
	f.gate.api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("ETag", tag)
		WriteNavigatorBody(w, r, h, 304, nil)
	})
	server := httptest.NewServer(f.gate)
	defer server.Close()
	p := conditionalTabHeader(t, f.binding, target, tag, token)
	r, err := http.NewRequest("GET", server.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", p.String())
	r.Header.Set("If-None-Match", tag)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 304 || response.Header.Get("Content-Type") != "" || len(body) != 0 || response.Header.Get("ETag") != tag {
		t.Fatalf("native304 contract: status%d mime%q body%d", response.StatusCode, response.Header.Get("Content-Type"), len(body))
	}
	raw, _ := navauth.Decode32(token)
	key := sha256.Sum256(raw[:])
	want, e := navauth.TabConditionalResponseMAC(key, f.binding, p, 304, tag, body)
	got, e2 := navauth.Decode32(response.Header.Get("X-Aiq-Tab-Proof"))
	if e != nil || e2 != nil || !navauth.EqualMAC(want, got) {
		t.Fatal("native response proof mismatch")
	}
}
