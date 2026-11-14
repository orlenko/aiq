package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/transcript"
)

type navAuthHTTPFixture struct {
	a       *navAuthAdapter
	key     [32]byte
	binding navauth.Binding
	nonce   byte
	gate    *navAuthHTTPFixtureGate
}

type navAuthHTTPFixtureGate struct{ *navGate }

func (g *navAuthHTTPFixtureGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	input, creator := ownNavInput(r)
	if creator {
		defer input.close()
	}
	g.navGate.ServeHTTP(w, r)
}

func newNavAuthHTTPFixture(t *testing.T) *navAuthHTTPFixture {
	t.Helper()
	f := &navAuthHTTPFixture{key: [32]byte{11, 23, 37}, binding: navauth.Binding{Port: 54321, Build: "invented-build"}}
	var err error
	f.a, err = newNavAuthAdapter(f.key, f.binding, navAuthInfo{Root: "/invented/project", DataDir: "/invented/data", Hostname: "invented-host"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.a.Close)
	f.gate = &navAuthHTTPFixtureGate{&navGate{verifier: f.a, hello: http.HandlerFunc(f.a.Hello), login: http.HandlerFunc(f.a.Login), revoke: http.HandlerFunc(f.a.Revoke), info: http.HandlerFunc(f.a.Info), api: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), pages: http.NotFoundHandler()}}
	return f
}

func (f *navAuthHTTPFixture) request(t *testing.T, method, path, body string, proof bool) (*http.Request, navauth.Proof) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "GET" && body == "" {
		r.Body = http.NoBody
	}
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	var p navauth.Proof
	if proof {
		var err error
		p.Timestamp, err = f.a.state.Now()
		if err != nil {
			t.Fatal(err)
		}
		f.nonce++
		p.Nonce[0] = f.nonce
		p.MAC, err = navauth.RequestMAC(f.key, f.binding, method, path, p.Timestamp, p.Nonce, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("X-Aiq-Proof", p.String())
	}
	return r, p
}

func (f *navAuthHTTPFixture) verify(t *testing.T, w *httptest.ResponseRecorder, p navauth.Proof, status int, body string) {
	t.Helper()
	if w.Code != status || body != "" && w.Body.String() != body {
		t.Fatalf("response status/body: %d %q", w.Code, w.Body.String())
	}
	got, err := navauth.Decode32(w.Header().Get("X-Aiq-Response-Proof"))
	want, wantErr := navauth.ResponseMAC(f.key, p, w.Code, w.Body.Bytes())
	if err != nil || wantErr != nil || !navauth.EqualMAC(got, want) {
		t.Error("response does not authenticate its exact status/body")
	}
	if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) || w.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing exact length or no-store")
	}
}

func TestNavAuthHTTPGateLifecycle(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	r, p := f.request(t, "GET", "/api/nav/info", "", true)
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, `{"root":"/invented/project","data_dir":"/invented/data","hostname":"invented-host"}`)
	// Replaying an admitted proof cannot reach the protected handler.
	r, _ = f.request(t, "GET", "/api/nav/info", "", false)
	r.Header.Set("X-Aiq-Proof", p.String())
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 || w.Body.Len() != 0 {
		t.Fatal("replayed proof accepted")
	}
	r, p = f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, "")
	var issued struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(w.Body.Bytes(), &issued) != nil || len(issued.Code) != 43 {
		t.Fatal("missing code")
	}
	exchange := `{"action":"exchange","code":"` + issued.Code + `"}`
	r, _ = f.request(t, "POST", "/api/nav/login", exchange, false)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	var credential struct {
		Credential string `json:"credential"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &credential) != nil || !f.a.Credential(credential.Credential) || w.Header().Get("X-Aiq-Response-Proof") != "" {
		t.Fatal("exchange did not return a live unsigned credential")
	}
	r, _ = f.request(t, "POST", "/api/nav/login", exchange, false)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 || w.Body.String() != `{"error":"invalid_link"}` {
		t.Fatal("one-use link replay accepted")
	}
	r, _ = f.request(t, "GET", "/api/nav/synthetic", "", false)
	r.Header.Set("Authorization", tabTestHeader(t, f.binding, r.Method, r.URL.RequestURI(), credential.Credential))
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal("live credential did not cross gate")
	}
	r, p = f.request(t, "POST", "/api/nav/revoke", `{}`, true)
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, `{"ok":true}`)
	if f.a.Credential(credential.Credential) {
		t.Error("revoked credential remains live")
	}
}

func TestNavAuthHTTPHello(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	for _, challenged := range []bool{false, true} {
		r, _ := f.request(t, "GET", "/api/nav/hello", "", false)
		challenge := [32]byte{41}
		if challenged {
			r.Header.Set("X-Aiq-Challenge", navauth.Encode32(challenge))
		}
		w := httptest.NewRecorder()
		f.gate.ServeHTTP(w, r)
		var body map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal("hello failed")
		}
		wantFields := 2
		if challenged {
			wantFields = 3
		}
		if len(body) != wantFields || string(body["build"]) != `"invented-build"` || string(body["nav_version"]) != "1" {
			t.Error("hello schema or privacy changed")
		}
		if challenged {
			got, e := navauth.Decode32(w.Header().Get("X-Aiq-Hello-Proof"))
			want, we := navauth.HelloMAC(f.key, f.binding, challenge, w.Code, w.Body.Bytes())
			if e != nil || we != nil || !navauth.EqualMAC(got, want) || len(body["server_time_ms"]) == 0 {
				t.Error("hello challenge not bound to exact response")
			}
		} else if w.Header().Get("X-Aiq-Hello-Proof") != "" {
			t.Error("unsolicited proof")
		}
	}
	for _, header := range [][]string{{"bad"}, {navauth.Encode32([32]byte{1}), navauth.Encode32([32]byte{2})}} {
		r, _ := f.request(t, "GET", "/api/nav/hello", "", false)
		r.Header["X-Aiq-Challenge"] = header
		w := httptest.NewRecorder()
		f.gate.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Error("invalid challenge accepted")
		}
	}
}

type navAuthHTTPReader struct {
	body   *strings.Reader
	reads  int
	before func()
}

func (b *navAuthHTTPReader) Read(p []byte) (int, error) {
	b.reads++
	if b.before != nil {
		fn := b.before
		b.before = nil
		fn()
	}
	if len(p) > 2 {
		p = p[:2]
	}
	return b.body.Read(p)
}
func (*navAuthHTTPReader) Close() error { return nil }

func TestNavAuthHTTPOwnedBodyAndFacts(t *testing.T) {
	f := newNavAuthHTTPFixture(t)
	r, p := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	b := &navAuthHTTPReader{body: strings.NewReader(`{"action":"issue"}`)}
	r.Body = b
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	// Login parses first, then Proof must consume that SAME owned snapshot.
	b.before = func() {
		if f.a.Proof(r) {
			t.Error("reentrant proof accepted incomplete body")
		}
	}
	// A reentrant attempt consumes the request's one proof attempt; it must fail closed.
	w := httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("reentrant request did not fail closed")
	}
	reads := b.reads
	if f.a.Proof(r) || b.reads != reads {
		t.Error("second attempt traversed body")
	}
	r, p = f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
	b = &navAuthHTTPReader{body: strings.NewReader(`{"action":"issue"}`)}
	r.Body = b
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	f.verify(t, w, p, 200, "")
	if b.reads < 2 {
		t.Error("fixture did not exercise multiple reads")
	}
	reads = b.reads
	other := newNavAuthHTTPFixture(t)
	if f.a.Proof(r) || other.a.Proof(r) || b.reads != reads {
		t.Error("cached request readmitted")
	}
	// Gate admission owns route facts; later mutable Request fields cannot repurpose them.
	r, p = f.request(t, "GET", "/api/nav/info", "", true)
	if !f.a.Proof(r) {
		t.Fatal("proof failed")
	}
	r.Method = "POST"
	r.URL.Path = "/api/nav/revoke"
	r.Body = io.NopCloser(strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	f.a.Revoke(w, r)
	if w.Code != 401 {
		t.Fatal("info facts repurposed for revoke")
	}
	w = httptest.NewRecorder()
	f.a.Info(w, r)
	f.verify(t, w, p, 200, "")
	// A verifier's boolean alone does not manufacture adapter-private facts.
	r, _ = f.request(t, "GET", "/api/nav/info", "", false)
	r.Header.Set("X-Test-Proof", "yes")
	f.gate.verifier = testVerifier{}
	w = httptest.NewRecorder()
	f.gate.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Error("fake gate acceptance manufactured facts")
	}
}

func TestNavAuthHTTPRejectsFraming(t *testing.T) {
	mutations := map[string]func(*http.Request){
		"query":                 func(r *http.Request) { r.URL.RawQuery = "ignored=yes" },
		"force query":           func(r *http.Request) { r.URL.ForceQuery = true },
		"raw path":              func(r *http.Request) { r.URL.RawPath = r.URL.Path },
		"absolute":              func(r *http.Request) { r.URL.Scheme = "http"; r.URL.Host = "127.0.0.1" },
		"inconsistent URI":      func(r *http.Request) { r.RequestURI += "?" },
		"empty URI":             func(r *http.Request) { r.RequestURI = "" },
		"encoding":              func(r *http.Request) { r.Header["Content-Encoding"] = []string{""} },
		"duplicate type alias":  func(r *http.Request) { r.Header["content-type"] = []string{"application/json"} },
		"duplicate proof alias": func(r *http.Request) { r.Header["x-aiq-proof"] = []string{r.Header.Get("X-Aiq-Proof")} },
		"extra type parameter":  func(r *http.Request) { r.Header.Set("Content-Type", "application/json; foo=bar") },
		"wrong charset":         func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=ascii") },
		"bad length":            func(r *http.Request) { r.ContentLength++ },
		"negative length":       func(r *http.Request) { r.ContentLength = -2 },
		"transfer ambiguity":    func(r *http.Request) { r.TransferEncoding = []string{"chunked"} },
		"trailers":              func(r *http.Request) { r.Trailer = http.Header{"X-Test": []string{"x"}} },
		"canceled": func(r *http.Request) {
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			*r = *r.WithContext(ctx)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			r, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
			mutate(r)
			if f.a.Proof(r) {
				t.Error("invalid framing admitted")
			}
		})
	}
	for _, raw := range []string{`{"Action":"issue"}`, `{"action":"issue","action":"issue"}`, `{"action":"issue","\u0061ction":"issue"}`, `{"action":"issue","unknown":"x"}`, `{"action":"issue"} {}`, `{"action":true}`, `[]`, `null`, `{"action":"issue","code":"x"}`, string([]byte{'{', '"', 255, '"', ':', '"', 'x', '"', '}'}), strings.Repeat(" ", navauth.MaxRequest+1)} {
		f := newNavAuthHTTPFixture(t)
		r, _ := f.request(t, "POST", "/api/nav/login", raw, false)
		w := httptest.NewRecorder()
		f.gate.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("invalid object accepted, length %d", len(raw))
		}
	}
	f := newNavAuthHTTPFixture(t)
	r, _ := f.request(t, "GET", "/api/nav/info", "", true)
	r.Body = io.NopCloser(strings.NewReader("hidden"))
	r.ContentLength = -1
	if f.a.Proof(r) {
		t.Error("GET hidden body admitted")
	}
}

func navAuthHTTPOrigin() (origin diagnostic.Origin) {
	defer func() { origin = diagnostic.Capture(recover()) }()
	navAuthHTTPLeaf()
	return
}

type navAuthHTTPHostile struct{ calls *int }

func (e navAuthHTTPHostile) Error() string { *e.calls++; panic("hostile Error invoked") }
func (e navAuthHTTPHostile) Unwrap() error { *e.calls++; panic("hostile Unwrap invoked") }
func (e navAuthHTTPHostile) Is(error) bool { *e.calls++; panic("hostile Is invoked") }

func navAuthHTTPCatch(fn func()) (value any, panicked bool) {
	panicked = true
	defer func() { value = recover() }()
	fn()
	panicked = false
	return
}

func TestNavAuthHTTPSignedRecovery(t *testing.T) {
	known, unknown := navAuthHTTPOrigin(), diagnostic.Capture(nil)
	if file, line, ok := known.Location(); !ok || file != "internal/daemon/nav_auth_http_leaf_test.go" || line != 3 {
		t.Fatal("leaf origin fixture invalid")
	}
	core := diagnostic.Failure(diagnostic.Unreadable, known)
	typedNil := reflect.Zero(reflect.TypeOf(core)).Interface().(error)
	// Produce the genuine legacy carrier through its exported parser seam, with no IO.
	restore := transcript.SetParserForTest(func(string) (*transcript.Session, error) { navAuthHTTPLeaf(); return nil, nil })
	_, legacy := transcript.ParseFile("/invented/no-read.jsonl")
	restore()
	if transcript.PanicOrigin(legacy) != known {
		t.Fatal("legacy carrier fixture invalid")
	}
	calls := 0
	cases := []struct {
		name   string
		value  any
		origin diagnostic.Origin
	}{
		{"core known", core, known}, {"core unknown", diagnostic.Failure(diagnostic.Unreadable, unknown), unknown},
		{"legacy known", legacy, known}, {"typed nil", typedNil, diagnostic.Origin{}},
		{"hostile", navAuthHTTPHostile{&calls}, diagnostic.Origin{}}, {"map", map[string]string{"invented": "private"}, diagnostic.Origin{}}, {"nil", nil, diagnostic.Origin{}},
	}
	for _, c := range cases {
		for _, seed := range []diagnostic.Origin{{}, known, unknown} {
			t.Run(c.name+"/seed-"+strconv.FormatBool(seed.Present()), func(t *testing.T) {
				f := newNavAuthHTTPFixture(t)
				f.a.issue = func(*navauth.Facts) (string, error) { panic(c.value) }
				r, p := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
				ctx, holder := diagnostic.WithHolder(r.Context())
				*r = *r.WithContext(ctx)
				holder.Note(seed)
				w := httptest.NewRecorder()
				f.gate.ServeHTTP(w, r)
				f.verify(t, w, p, 500, `{"error":"internal_error"}`)
				want := c.origin
				if seed.Present() {
					want = seed
				}
				if want.Present() && holder.Origin() != want || !holder.Origin().Present() {
					t.Error("first origin lost or replaced")
				}
			})
		}
	}
	if calls != 0 {
		t.Error("recovery invoked hostile methods")
	}
	for _, seed := range []diagnostic.Origin{{}, known, unknown} {
		f := newNavAuthHTTPFixture(t)
		f.a.issue = func(*navauth.Facts) (string, error) { panic(http.ErrAbortHandler) }
		r, _ := f.request(t, "POST", "/api/nav/login", `{"action":"issue"}`, true)
		ctx, holder := diagnostic.WithHolder(r.Context())
		*r = *r.WithContext(ctx)
		holder.Note(seed)
		w := httptest.NewRecorder()
		value, panicked := navAuthHTTPCatch(func() { f.gate.ServeHTTP(w, r) })
		if !panicked || value != http.ErrAbortHandler || holder.Origin() != seed || w.Body.Len() != 0 || len(w.Header()) != 0 {
			t.Error("abort captured, replaced or answered")
		}
	}
}

func TestNavAuthHTTPFinitePreparation(t *testing.T) {
	for _, mode := range []string{"encode error", "encode panic", "encode oversize", "sign error", "sign panic", "secondary sign panic"} {
		t.Run(mode, func(t *testing.T) {
			f := newNavAuthHTTPFixture(t)
			encodes, signs := 0, 0
			originalEncode, originalSign := f.a.encode, f.a.sign
			f.a.encode = func(v navAuthResult) ([]byte, error) {
				encodes++
				switch mode {
				case "encode error":
					return nil, errors.New("invented encoder error")
				case "encode panic", "secondary sign panic":
					navAuthHTTPLeaf()
				case "encode oversize":
					return bytes.Repeat([]byte{'x'}, navauth.MaxResponse+1), nil
				}
				return originalEncode(v)
			}
			f.a.sign = func(v *navauth.Facts, status int, body []byte) ([32]byte, error) {
				signs++
				if mode == "sign error" {
					return [32]byte{}, errors.New("invented sign error")
				}
				if mode == "sign panic" || mode == "secondary sign panic" {
					navAuthHTTPLeaf()
				}
				return originalSign(v, status, body)
			}
			r, p := f.request(t, "GET", "/api/nav/info", "", true)
			w := httptest.NewRecorder()
			value, panicked := navAuthHTTPCatch(func() { f.gate.ServeHTTP(w, r) })
			if strings.Contains(mode, "sign") {
				if !panicked || value != http.ErrAbortHandler || len(w.Header()) != 0 || w.Body.Len() != 0 {
					t.Error("sign failure not terminal")
				}
			} else {
				if panicked {
					t.Fatal("encoder failure escaped")
				}
				f.verify(t, w, p, 500, `{"error":"internal_error"}`)
			}
			if encodes != 1 || signs != 1 {
				t.Errorf("unbounded/retried preparation: encode %d sign %d", encodes, signs)
			}
		})
	}
}
