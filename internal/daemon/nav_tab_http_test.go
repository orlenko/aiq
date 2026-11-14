package daemon

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/navauth"
)

func tabTestHeader(t *testing.T, b navauth.Binding, method, target, credential string) string {
	t.Helper()
	raw, err := navauth.Decode32(credential)
	if err != nil {
		t.Fatal("invented credential")
	}
	key := sha256.Sum256(raw[:])
	p := navauth.TabProof{ID: navauth.TabID(key), Nonce: [32]byte{51}}
	p.MAC, err = navauth.TabRequestMAC(key, b, method, target, p)
	if err != nil {
		t.Fatal("invented read proof")
	}
	return p.String()
}
func setLifeTab(t *testing.T, r *http.Request, g *navGeneration, credential string) {
	t.Helper()
	r.Header.Set("Authorization", tabTestHeader(t, navauth.Binding{Port: g.port, Build: g.auth.build}, r.Method, r.URL.RequestURI(), credential))
}
func tabFixtureCredential(t *testing.T, f *navAuthHTTPFixture) string {
	t.Helper()
	const body = `{"action":"issue"}`
	_, p := f.request(t, "POST", "/api/nav/login", body, true)
	facts, e := f.a.state.Admit("POST", "/api/nav/login", p, []byte(body))
	if e != nil {
		t.Fatal("invented issuer")
	}
	code, e := f.a.state.Issue(facts)
	if e != nil {
		t.Fatal("invented issue")
	}
	token, e := f.a.state.Exchange(code)
	if e != nil {
		t.Fatal("invented exchange")
	}
	return token
}
func TestNavTabHTTPClosedAuthorizationAndSignedBodies(t *testing.T) {
	for _, mode := range []string{"success", "request-error", "bearer", "scheme", "duplicate", "alias", "bad-mac", "wrong-port", "wrong-build", "body", "conditional", "conditional-alias", "proof-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			credential := tabFixtureCredential(t, f)
			calls := 0
			status := 200
			f.gate.api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				WriteNavigatorBody(w, r, w.Header(), status, []byte(`{"invented":true}`))
			})
			r, _ := f.request(t, "GET", "/api/nav/sessions?dir=%2Finvented&workers=0", "", false)
			b := f.binding
			if mode == "wrong-port" {
				b.Port++
			}
			if mode == "wrong-build" {
				b.Build = "other"
			}
			r.Header.Set("Authorization", tabTestHeader(t, b, "GET", r.URL.RequestURI(), credential))
			switch mode {
			case "request-error":
				status = 503
			case "bearer":
				r.Header.Set("Authorization", "Bearer "+credential)
			case "scheme":
				r.Header.Set("Authorization", strings.ToLower(r.Header.Get("Authorization")))
			case "duplicate":
				r.Header.Add("Authorization", r.Header.Get("Authorization"))
			case "alias":
				r.Header["authorization"] = []string{r.Header.Get("Authorization")}
			case "bad-mac":
				r.Header.Set("Authorization", r.Header.Get("Authorization")[:100]+strings.Repeat("A", 38))
			case "body":
				r.ContentLength = 1
				r.Body = http.NoBody
			case "conditional":
				r.Header.Set("If-None-Match", `"invented"`)
			case "conditional-alias":
				r.Header["if-none-match"] = []string{`"invented"`}
			case "proof-only":
				r.URL.Path = "/api/nav/info"
				r.URL.RawQuery = ""
				r.RequestURI = r.URL.Path
				r.Header.Set("Authorization", tabTestHeader(t, b, "GET", r.RequestURI, credential))
			}
			out := httptest.NewRecorder()
			f.gate.ServeHTTP(out, r)
			if mode != "success" && mode != "request-error" {
				if out.Code != 401 || calls != 0 {
					t.Fatal("invalid read reached data handler", mode, out.Code, calls)
				}
				return
			}
			if out.Code != status || calls != 1 {
				t.Fatal("genuine read refused")
			}
			p, e := navauth.ParseTabProof(r.Header.Get("Authorization"))
			raw, _ := navauth.Decode32(credential)
			key := sha256.Sum256(raw[:])
			want, e2 := navauth.TabResponseMAC(key, f.binding, p, status, out.Body.Bytes())
			got, e3 := navauth.Decode32(out.Header().Get("X-Aiq-Tab-Proof"))
			if e != nil || e2 != nil || e3 != nil || !navauth.EqualMAC(got, want) {
				t.Fatal("response status/body proof incorrect")
			}
		})
	}
}
func TestNavTabRetirementDiscardsAlreadyPreparedPrivateBody(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	token := tabFixtureCredential(t, f)
	f.gate.api = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.a.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"invented"`)
		WriteNavigatorBody(w, r, w.Header(), 200, []byte(`{"invented":"not sent"}`))
	})
	r, _ := f.request(t, "GET", "/api/nav/sessions", "", false)
	r.Header.Set("Authorization", tabTestHeader(t, f.binding, "GET", r.URL.RequestURI(), token))
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 || w.Body.Len() != 0 || w.Header().Get("X-Aiq-Tab-Proof") != "" || w.Header().Get("ETag") != "" {
		t.Fatal("retired prepared response committed data")
	}
}
