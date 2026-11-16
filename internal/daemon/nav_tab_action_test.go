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

func TestNavTabHTTPActionDomainBodyAndOrigin(t *testing.T) {
	for _, mode := range []string{"success", "read-proof", "body", "wrong-query", "other-post", "cross-site", "foreign-origin", "duplicate", "conditional"} {
		t.Run(mode, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			credential := tabFixtureCredential(t, f)
			target := "/api/nav/asks/action?action=dismiss&id=1&revision=" + strings.Repeat("a", 64)
			r, _ := f.request(t, "POST", target, "", false)
			r.Body = http.NoBody
			raw, _ := navauth.Decode32(credential)
			key := sha256.Sum256(raw[:])
			p := navauth.ActionTabProof{Proof: navauth.TabProof{ID: navauth.TabID(key), Nonce: [32]byte{8}}}
			var err error
			p.Proof.MAC, err = navauth.TabActionRequestMAC(key, f.binding, "POST", target, p)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", p.String())
			calls := 0
			f.gate.api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				WriteNavigatorBody(w, r, w.Header(), 200, []byte(`{"invented":true}`))
			})
			switch mode {
			case "read-proof":
				r.Header.Set("Authorization", tabTestHeader(t, f.binding, "GET", target, credential))
			case "body":
				r.ContentLength = 1
				r.Body = io.NopCloser(strings.NewReader("x"))
			case "wrong-query":
				r.URL.RawQuery = strings.Replace(r.URL.RawQuery, "dismiss", "restore", 1)
				r.RequestURI = r.URL.RequestURI()
			case "other-post":
				r.URL.Path = "/api/nav/sessions"
				r.RequestURI = r.URL.RequestURI()
			case "cross-site":
				r.Header.Set("Sec-Fetch-Site", "cross-site")
			case "foreign-origin":
				r.Header.Set("Origin", "https://example.invalid")
			case "duplicate":
				r.Header.Add("Authorization", p.String())
			case "conditional":
				r.Header.Set("If-None-Match", `"invented"`)
			}
			out := httptest.NewRecorder()
			r.RemoteAddr = "127.0.0.1:32001"
			r.Host = "127.0.0.1:54321"
			localOnly(f.gate).ServeHTTP(out, r)
			if mode == "success" {
				if out.Code != 200 || calls != 1 || out.Header().Get("X-Aiq-Tab-Proof") == "" {
					t.Fatal("action not signed", out.Code, calls)
				}
			} else if calls != 0 || out.Code == 200 {
				t.Fatal("invalid action reached handler", mode, out.Code, calls)
			}
		})
	}
}
