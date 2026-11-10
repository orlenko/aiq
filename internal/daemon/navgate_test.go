package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
)

const (
	rightCredential = "a-test-credential-value-0123456789"
	marker          = "behind-the-gate"
)

// testVerifier accepts one credential, and a proof sent as a test header.
type testVerifier struct{}

func (testVerifier) Credential(bearer string) bool { return bearer == rightCredential }
func (testVerifier) Proof(r *http.Request) bool    { return r.Header.Get("X-Test-Proof") == "yes" }

// markerHandler answers every request with the marker, and counts them.
func markerHandler(n *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Write([]byte(marker))
	})
}

// gateUnderTest is the navigator's handler with markers behind every guarded route.
func gateUnderTest(v NavVerifier) (h http.Handler, api, proven *atomic.Int32) {
	api, proven = &atomic.Int32{}, &atomic.Int32{}
	return newNavHandler(navGate{
		verifier: v,
		hello:    navHello("test-build"),
		login:    navLoginClosed(),
		revoke:   markerHandler(proven),
		info:     markerHandler(proven),
		api:      markerHandler(api),
	}, "test-build", func(string, ...any) {}), api, proven
}

// navRequest is a request as the navigator's port would see it from a loopback peer.
type navRequest struct {
	method, target string
	header         map[string]string
}

func (c navRequest) serve(h http.Handler) *httptest.ResponseRecorder {
	method := c.method
	if method == "" {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:50001"+c.target, nil)
	r.RequestURI = c.target
	r.RemoteAddr = "127.0.0.1:50000"
	for k, v := range c.header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearer(v string) map[string]string { return map[string]string{"Authorization": "Bearer " + v} }

func checkNavHeaders(t *testing.T, name string, h http.Header) {
	t.Helper()
	for k, want := range map[string]string{
		"Cache-Control":                "no-store",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Origin-Agent-Cluster":         "?1",
		"Content-Security-Policy":      "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; require-trusted-types-for 'script'; trusted-types 'none'",
		"X-Frame-Options":              "DENY",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s: %s is %q, want %q", name, k, got, want)
		}
	}
	if h.Get("X-Aiq-Build") == "" {
		t.Errorf("%s: no X-Aiq-Build", name)
	}
}

// checkHello requires hello's answer to hold exactly the protocol and the build: it is
// unauthenticated, so it says nothing about the machine or its data.
func checkHello(t *testing.T, name string, body []byte, build string) {
	t.Helper()
	var hello map[string]any
	if err := json.Unmarshal(body, &hello); err != nil || len(hello) != 2 || hello["nav_version"] != float64(navProtocol) || hello["build"] != build {
		t.Errorf("%s: hello is %q", name, body)
	}
}

// Every guarded route answers 401 with an empty body, whatever is wrong with the
// request, so the answer tells a caller nothing about what is there.
func TestTheGateRefusesWithoutACredential(t *testing.T) {
	for _, v := range []NavVerifier{denyAll{}, testVerifier{}} {
		h, api, _ := gateUnderTest(v)
		for name, c := range map[string]navRequest{
			"no header":         {target: "/api/nav/ping"},
			"wrong":             {target: "/api/nav/ping", header: bearer("wrong")},
			"mutated":           {target: "/api/nav/ping", header: bearer(rightCredential[:10] + "X" + rightCredential[11:])},
			"short":             {target: "/api/nav/ping", header: bearer(rightCredential[:20])},
			"long":              {target: "/api/nav/ping", header: bearer(rightCredential + "0")},
			"empty":             {target: "/api/nav/ping", header: bearer("")},
			"lowercase scheme":  {target: "/api/nav/ping", header: map[string]string{"Authorization": "bearer " + rightCredential}},
			"basic":             {target: "/api/nav/ping", header: map[string]string{"Authorization": "Basic " + rightCredential}},
			"as a cookie":       {target: "/api/nav/ping", header: map[string]string{"Cookie": "Authorization=Bearer%20" + rightCredential}},
			"in the query":      {target: "/api/nav/ping?access_token=" + rightCredential + "&bearer=" + rightCredential},
			"an unknown route":  {target: "/api/nav/sessions/claude/0123"},
			"outside /api/nav/": {target: "/api/state"},
			"a POST":            {method: http.MethodPost, target: "/api/nav/ping"},
			"conditional":       {target: "/api/nav/ping", header: map[string]string{"If-None-Match": `"x"`}},
			"HEAD on hello":     {method: http.MethodHead, target: "/api/nav/hello"},
			"POST on hello":     {method: http.MethodPost, target: "/api/nav/hello"},
			"GET on login":      {target: "/api/nav/login"},
			"GET on revoke":     {target: "/api/nav/revoke"},
			"POST on info":      {method: http.MethodPost, target: "/api/nav/info"},
			"a case variant":    {target: "/api/nav/Hello"},
			"a longer path":     {target: "/api/nav/hellos"},
			"a longer login":    {method: http.MethodPost, target: "/api/nav/logins"},
			"below hello":       {target: "/api/nav/hello/x"},
		} {
			w := c.serve(h)
			if w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
				t.Errorf("%T, %s: %d %q", v, name, w.Code, w.Body)
			}
			checkNavHeaders(t, name, w.Header())
		}
		if api.Load() != 0 {
			t.Errorf("%T: a refused request reached the API", v)
		}
	}
}

func TestTheGateLetsOnlyAnAcceptedCredentialThrough(t *testing.T) {
	right := navRequest{target: "/api/nav/ping", header: bearer(rightCredential)}
	h, api, _ := gateUnderTest(denyAll{})
	if w := right.serve(h); w.Code != http.StatusUnauthorized || api.Load() != 0 {
		t.Errorf("deny-all let it through: %d", w.Code)
	}
	h, api, _ = gateUnderTest(testVerifier{})
	if w := right.serve(h); w.Code != http.StatusOK || w.Body.String() != marker || api.Load() != 1 {
		t.Errorf("the accepted credential got %d %q", w.Code, w.Body)
	}
	// A second Authorization header is not a way to slip one past.
	r := httptest.NewRequest(http.MethodGet, "/api/nav/ping", nil)
	r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:50001"
	r.Header.Add("Authorization", "Bearer "+rightCredential)
	r.Header.Add("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("two credentials got %d", w.Code)
	}
}

// acceptAll is a verifier that accepts anything, for checking the gate's own refusals.
type acceptAll struct{}

func (acceptAll) Credential(string) bool   { return true }
func (acceptAll) Proof(*http.Request) bool { return true }

// The gate refuses a missing, empty, doubled or wrongly named credential itself, whatever
// the verifier would say.
func TestTheGateRefusesMalformedCredentialsItself(t *testing.T) {
	h, api, _ := gateUnderTest(acceptAll{})
	for name, header := range map[string][]string{
		"no header":        nil,
		"an empty bearer":  {"Bearer "},
		"the bare scheme":  {"Bearer"},
		"two headers":      {"Bearer a", "Bearer b"},
		"lowercase scheme": {"bearer " + rightCredential},
		"another scheme":   {"Basic " + rightCredential},
	} {
		r := httptest.NewRequest(http.MethodGet, "/api/nav/ping", nil)
		r.RemoteAddr, r.Host = "127.0.0.1:50000", "127.0.0.1:50001"
		for _, v := range header {
			r.Header.Add("Authorization", v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if api.Load() != 0 {
		t.Error("a malformed credential reached the API")
	}
	if w := (navRequest{target: "/api/nav/ping", header: bearer("anything")}).serve(h); w.Code != http.StatusOK {
		t.Errorf("a well-formed credential the verifier accepts: %d", w.Code)
	}
}

// localOnly and the gate refuse a request from another site even with the credential.
func TestTheGateRefusesOtherSitesWithTheCredential(t *testing.T) {
	h, api, _ := gateUnderTest(testVerifier{})
	for name, c := range map[string]navRequest{
		"a foreign Origin":     {target: "/api/nav/ping", header: map[string]string{"Authorization": "Bearer " + rightCredential, "Origin": "https://example.com"}},
		"a cross-site GET":     {target: "/api/nav/ping", header: map[string]string{"Authorization": "Bearer " + rightCredential, "Sec-Fetch-Site": "cross-site"}},
		"a same-site GET":      {target: "/api/nav/ping", header: map[string]string{"Authorization": "Bearer " + rightCredential, "Sec-Fetch-Site": "same-site"}},
		"a cross-site hello":   {target: "/api/nav/hello", header: map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"a cross-site login":   {method: http.MethodPost, target: "/api/nav/login", header: map[string]string{"Sec-Fetch-Site": "cross-site"}},
		"a cross-site revoke":  {method: http.MethodPost, target: "/api/nav/revoke", header: map[string]string{"Sec-Fetch-Site": "cross-site", "X-Test-Proof": "yes"}},
		"a cross-site info":    {target: "/api/nav/info", header: map[string]string{"Sec-Fetch-Site": "cross-site", "X-Test-Proof": "yes"}},
		"another site's frame": {target: "/api/nav/ping", header: map[string]string{"Authorization": "Bearer " + rightCredential, "Origin": "null"}},
	} {
		if w := c.serve(h); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/api/nav/ping", nil)
	r.RemoteAddr, r.Host = "127.0.0.1:50000", "evil.example:50001"
	r.Header.Set("Authorization", "Bearer "+rightCredential)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("a rebound name: %d", w.Code)
	}
	if api.Load() != 0 {
		t.Error("another site reached the API")
	}
	for _, site := range []string{"same-origin", "none"} {
		c := navRequest{target: "/api/nav/ping", header: map[string]string{"Authorization": "Bearer " + rightCredential, "Sec-Fetch-Site": site}}
		if w := c.serve(h); w.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %s: %d", site, w.Code)
		}
	}
}

// The exempt routes are answered from the gate's own table and never reach the API.
func TestTheExemptRoutesAreServedByTheGate(t *testing.T) {
	for _, v := range []NavVerifier{denyAll{}, testVerifier{}} {
		h, api, proven := gateUnderTest(v)
		for _, target := range []string{"/api/nav/hello", "/api/nav/hello?"} {
			w := navRequest{target: target}.serve(h)
			if w.Code != http.StatusOK {
				t.Errorf("%T: %s: %d %q", v, target, w.Code, w.Body)
			}
			checkHello(t, target, w.Body.Bytes(), "test-build")
			checkNavHeaders(t, target, w.Header())
		}
		if w := (navRequest{method: http.MethodPost, target: "/api/nav/login"}).serve(h); w.Code != http.StatusForbidden || w.Body.Len() != 0 {
			t.Errorf("%T: login: %d %q", v, w.Code, w.Body)
		}
		for _, c := range []navRequest{{method: http.MethodPost, target: "/api/nav/revoke"}, {target: "/api/nav/info"}} {
			if w := c.serve(h); w.Code != http.StatusUnauthorized || w.Body.Len() != 0 {
				t.Errorf("%T: %s without a proof: %d", v, c.target, w.Code)
			}
			// A credential is not a proof.
			c.header = bearer(rightCredential)
			if w := c.serve(h); w.Code != http.StatusUnauthorized {
				t.Errorf("%T: %s with a credential: %d", v, c.target, w.Code)
			}
			c.header = map[string]string{"X-Test-Proof": "yes"}
			w := c.serve(h)
			_, accepts := v.(testVerifier)
			if accepts && w.Body.String() != marker || !accepts && w.Code != http.StatusUnauthorized {
				t.Errorf("%T: %s with a proof: %d %q", v, c.target, w.Code, w.Body)
			}
		}
		if api.Load() != 0 {
			t.Errorf("%T: an exempt route went through the API", v)
		}
		if _, accepts := v.(testVerifier); !accepts && proven.Load() != 0 {
			t.Error("deny-all let a proof through")
		}
	}
}

// A path the mux could read differently from the gate is refused before the gate.
func TestNonCanonicalPathsAreRefused(t *testing.T) {
	h, api, proven := gateUnderTest(testVerifier{})
	for _, target := range []string{
		"/api/nav/%68ello", "/api/nav/hello%2F", "/api/nav/%6cogin", "/api/nav/%72evoke", "/api/nav/%69nfo",
		"/api/na%76/ping", "/%61pi/nav/ping", "/api%2Fnav/ping", "/api/nav/%2e%2e/nav/ping",
		"/api/nav/hello/", "/api/nav/login/", "//api/nav/hello", "/api//nav/ping",
		"/api/nav/./hello", "/api/nav/../nav/hello", "/api/nav/ping/.", "/./api/nav/ping", "/api/",
		"/api/nav/hello%3F", "/api/nav/a%20b", "/api/nav/%25",
	} {
		for _, header := range []map[string]string{nil, {"Authorization": "Bearer " + rightCredential, "X-Test-Proof": "yes"}} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				w := navRequest{method: method, target: target, header: header}.serve(h)
				if w.Code != http.StatusBadRequest || w.Body.Len() != 0 {
					t.Errorf("%s %s: %d %q", method, target, w.Code, w.Body)
				}
				checkNavHeaders(t, target, w.Header())
			}
		}
	}
	if api.Load() != 0 || proven.Load() != 0 {
		t.Error("a non-canonical path reached a handler")
	}
}

func TestANilVerifierDeniesEverything(t *testing.T) {
	api := &atomic.Int32{}
	h := newNavHandler(navGate{api: markerHandler(api), revoke: markerHandler(api)}, "b", func(string, ...any) {})
	for _, c := range []navRequest{
		{target: "/api/nav/ping", header: bearer(rightCredential)},
		{method: http.MethodPost, target: "/api/nav/revoke", header: map[string]string{"X-Test-Proof": "yes"}},
	} {
		if w := c.serve(h); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", c.target, w.Code)
		}
	}
	if api.Load() != 0 {
		t.Error("a nil verifier let a request through")
	}
}

func TestPagesNeverReachTheAPI(t *testing.T) {
	h, api, _ := gateUnderTest(testVerifier{})
	for _, target := range []string{"/", "/API/nav/ping", "/Api/nav/ping", "/api", "/sessions", "/help"} {
		w := navRequest{target: target, header: bearer(rightCredential)}.serve(h)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), marker) {
			t.Errorf("%s: %d %q", target, w.Code, w.Body)
		}
		checkNavHeaders(t, target, w.Header())
	}
	if api.Load() != 0 {
		t.Error("a page path reached the API")
	}
}

// rawGet sends a request line exactly as written, so the server's own parsing of escapes
// is what the test exercises.
func rawGet(t *testing.T, addr, target, credential string) (int, string) {
	t.Helper()
	resp, body, err := rawResponse(t, addr, target, credential)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// The membership rule, proved against a mounted handler that serves every path, over a
// real socket: an escaped or differently spelled path never reaches it, and the
// canonical path does, which shows the handler was reachable.
func TestTheMountedHandlerIsReachedOnlyThroughTheGate(t *testing.T) {
	for _, v := range []NavVerifier{denyAll{}, testVerifier{}} {
		s := New(config.Default(), nil, log.New(io.Discard, "", 0))
		s.navVerifier = v
		s.MountNavigator(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(marker)) }))
		srv := httptest.NewServer(s.navigatorHandler())
		addr := strings.TrimPrefix(srv.URL, "http://")
		for _, target := range []string{"/api/na%76/sessions", "/%61pi/nav/ping", "/api%2Fnav/ping", "/API/nav/ping", "/api/nav/./ping", "/api/nav//ping", "/api/nav/hello%3F", "/api/nav/a%20b", "/api/nav/%25"} {
			for _, credential := range []string{"", rightCredential} {
				if code, body := rawGet(t, addr, target, credential); code == http.StatusOK || strings.Contains(body, marker) {
					t.Errorf("%T: %s with credential %v: %d %q", v, target, credential != "", code, body)
				}
			}
		}
		code, body := rawGet(t, addr, "/api/nav/ping", rightCredential)
		_, accepts := v.(testVerifier)
		if accepts && (code != http.StatusOK || body != marker) || !accepts && code != http.StatusUnauthorized {
			t.Errorf("%T: the canonical path got %d %q", v, code, body)
		}
		srv.Close()
	}
}

// The daemon's own navigator answers hello with its protocol and build, carries its
// headers, and shows the token in no header from either port.
func TestTheNavigatorAnswersHello(t *testing.T) {
	useDataDir(t)
	d := startDaemon(t, true)
	port, token := navOnFields(t, d.s, navFile(t))
	nav := "http://127.0.0.1:" + strconv.Itoa(port)
	resp, body := getURL(t, nav+"/api/nav/hello")
	if resp.StatusCode != 200 || navProtocol != 1 {
		t.Fatalf("hello: %d %q", resp.StatusCode, body)
	}
	checkHello(t, "hello", []byte(body), navBuild())
	checkNavHeaders(t, "hello", resp.Header)
	if resp.Header.Get("X-Aiq-Build") != navBuild() {
		t.Errorf("X-Aiq-Build %q, want %q", resp.Header.Get("X-Aiq-Build"), navBuild())
	}
	seen := []string{body}
	for _, vs := range resp.Header {
		seen = append(seen, vs...)
	}
	for _, target := range []string{"/api/nav/ping", "/api/nav/info", "/", "/api/na%76/ping", "/api/nav/ping?token=" + token} {
		req, err := http.NewRequest(http.MethodGet, nav+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		// The token presented as a credential is refused like any other value: deny-all
		// is what β ships.
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s: 200", target)
		}
		seen = append(seen, string(b))
		for _, vs := range resp.Header {
			seen = append(seen, vs...)
		}
	}
	resp, body = getURL(t, nav+"/api/nav/ping")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("ping without a credential: %d %q", resp.StatusCode, body)
	}
	for _, s := range append(seen, d.log.String()) {
		if strings.Contains(s, token) {
			t.Error("the token is in an answer from the navigator or in the log")
		}
	}
}

// The status page builds its link from navigator_port, and only for a port number. The
// check runs the page's own function in Node, where Node is installed.
func TestTheStatusPageLinksOnlyToAPortNumber(t *testing.T) {
	page := string(uiHTML)
	begin, end := strings.Index(page, "// sessionsLink:begin"), strings.Index(page, "// sessionsLink:end")
	if begin < 0 || end < begin {
		t.Fatal("the link's function is not marked in ui.html")
	}
	if n := strings.Count(page, "navigator_port"); n != 1 || !strings.Contains(page, "sessionsLink(daemon.navigator_port)") {
		t.Errorf("ui.html uses navigator_port %d times, not only through sessionsLink", n)
	}
	// A refresh that fails takes the link down, so it never points at a port the daemon
	// may no longer hold.
	if !strings.Contains(page, "} catch (e) {\n    sessionsLink(null);") {
		t.Error("refresh's catch does not take the link down")
	}
	fn := page[begin:end]
	for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "location"} {
		if strings.Contains(fn, banned) {
			t.Errorf("the link's function uses %s", banned)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		// CI images ship Node, so a CI run that skips this has not checked the link.
		if os.Getenv("CI") != "" {
			t.Fatal("node is not installed, and CI is set")
		}
		t.Skip("node is not installed")
	}
	script := `
let created = 0;
function element(tag) {
  created++;
  return {
    tag, className: '', href: '', textContent: '', attrs: {}, children: [],
    setAttribute(k, v) { this.attrs[k] = String(v); },
    appendChild(c) { this.children.push(c); },
    replaceChildren() { this.children = []; },
  };
}
const slot = element('span');
const document = {
  getElementById(id) { if (id !== 'navlink') throw new Error(id); return slot; },
  createElement: element,
};
` + fn + `
function shown() {
  const a = slot.children[0];
  if (!a) return '-';
  const parts = a.children.map(c => c.tag + JSON.stringify(c.attrs) + c.textContent);
  return a.tag + ' ' + a.className + ' ' + a.href + ' ' + a.textContent + ' ' + parts.join(',');
}
const results = [];
for (const port of [50001, 1, 65535, '50001', 50001.5, 0, -1, 65536, null, undefined, NaN, Infinity, 'javascript:alert(1)', {}, [50001], true]) {
  shownPort = undefined;
  sessionsLink(port);
  results.push(shown());
}
// A refresh with the same port leaves the link alone; a port that goes and comes back
// redraws it.
shownPort = undefined;
sessionsLink(null);
created = 0;
sessionsLink(50001);
sessionsLink(50001);
results.push('created ' + created);
sessionsLink(null);
results.push(shown());
sessionsLink(50001);
results.push('created ' + created + ' ' + shown());
console.log(JSON.stringify(results));
`
	path := filepath.Join(t.TempDir(), "link.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	link := func(port string) string {
		return "a nav http://127.0.0.1:" + port + `/ Sessions span{"aria-hidden":"true"}` + "\u00a0→"
	}
	want := []string{link("50001"), link("1"), link("65535")}
	for len(want) < 16 {
		want = append(want, "-")
	}
	want = append(want, "created 2", "-", "created 4 "+link("50001"))
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("links: %q", got)
	}
}

// rawResponse sends a request line exactly as written and reads the answer. err is set
// when the answer cannot be read in full.
func rawResponse(t *testing.T, addr, target, credential string) (resp *http.Response, body string, err error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	req := "GET " + target + " HTTP/1.1\r\nHost: " + addr + "\r\nConnection: close\r\n"
	if credential != "" {
		req += "Authorization: Bearer " + credential + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, string(b), err
}

// panicking serves routes that panic at each point of writing a response.
func panicking() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/nav/before":
			panic("a test panic carrying " + rightCredential)
		case "/api/nav/length":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			panic(errors.New("a test error"))
		case "/api/nav/header":
			w.WriteHeader(http.StatusOK)
		case "/api/nav/written":
			w.Write([]byte(`{"partial":`))
		case "/api/nav/flushed":
			w.Write([]byte(`{"partial":`))
			w.(http.Flusher).Flush()
		case "/api/nav/abort":
			panic(http.ErrAbortHandler)
		default:
			if strings.HasPrefix(r.URL.Path, "/api/nav/long/") {
				panic("long")
			}
			w.Write([]byte(marker))
			return
		}
		panic("a test panic after writing")
	})
}

// A handler that panics before writing answers a whole 500 with the navigator's headers,
// so the page sees an answer from a live daemon and not a dropped connection, and the
// port keeps serving. The log names the panic's type, never its value.
func TestAPanicBeforeWritingIsAnAnswer(t *testing.T) {
	logged := &lockedBuffer{}
	s := New(config.Default(), nil, log.New(logged, "", 0))
	s.navVerifier = testVerifier{}
	s.MountNavigator(panicking())
	srv := httptest.NewServer(s.navigatorHandler())
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, target := range []string{"/api/nav/before", "/api/nav/before", "/api/nav/length"} {
		resp, body, err := rawResponse(t, addr, target, rightCredential)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if resp.StatusCode != http.StatusInternalServerError || body != "" || resp.ContentLength > 0 || resp.Header.Get("Content-Type") != "" {
			t.Errorf("%s: %d, length %d, type %q, %q", target, resp.StatusCode, resp.ContentLength, resp.Header.Get("Content-Type"), body)
		}
		checkNavHeaders(t, target, resp.Header)
	}
	if resp, body, err := rawResponse(t, addr, "/api/nav/ping", rightCredential); err != nil || resp.StatusCode != http.StatusOK || body != marker {
		t.Errorf("after a panic: %v %v %q", err, resp, body)
	}
	got := logged.String()
	for _, want := range []string{`navigator: a handler panicked on "/api/nav/before": string`, `navigator: a handler panicked on "/api/nav/length": *errors.errorString`} {
		if !strings.Contains(got, want) {
			t.Errorf("the log has no %q: %s", want, got)
		}
	}
	if strings.Contains(got, "a test") || strings.Contains(got, rightCredential) {
		t.Errorf("the log carries a panic's value: %s", got)
	}

	long := "/api/nav/long/" + strings.Repeat("x", 4000)
	rawResponse(t, addr, long, rightCredential)
	_, line, _ := strings.Cut(logged.String(), "/api/nav/long/")
	line, _, _ = strings.Cut(line, "\n")
	if want := strconv.Quote(long[:navPanicPathMax])[len(`"/api/nav/long/`):] + ": string"; line != want {
		t.Errorf("a long path is logged as %d bytes: %.300s", len(line), line)
	}
}

// A handler that panics after its response has begun has its connection aborted, so the
// client cannot take a cut-short answer for a whole one. ErrAbortHandler aborts without
// a log line.
func TestAPanicAfterWritingAborts(t *testing.T) {
	logged := &lockedBuffer{}
	s := New(config.Default(), nil, log.New(logged, "", 0))
	s.navVerifier = testVerifier{}
	s.MountNavigator(panicking())
	srv := httptest.NewServer(s.navigatorHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, target := range []string{"/api/nav/header", "/api/nav/written", "/api/nav/flushed", "/api/nav/abort"} {
		if resp, body, err := rawResponse(t, addr, target, rightCredential); err == nil {
			t.Errorf("%s: read in full: %d %q", target, resp.StatusCode, body)
		}
	}
	got := logged.String()
	for _, target := range []string{"header", "written", "flushed"} {
		if !strings.Contains(got, `panicked on "/api/nav/`+target+`": string`) {
			t.Errorf("no log line for %s: %s", target, got)
		}
	}
	if strings.Contains(got, "abort") {
		t.Errorf("ErrAbortHandler was logged: %s", got)
	}
}

// panickingVerifier panics in both of its methods.
type panickingVerifier struct{}

func (panickingVerifier) Credential(string) bool   { panic("a test credential panic") }
func (panickingVerifier) Proof(*http.Request) bool { panic("a test proof panic") }

// A verifier that panics is inside the recover: the answer is a 500 with the navigator's
// headers, and nothing behind the gate runs.
func TestAPanickingVerifierIsAnAnswer(t *testing.T) {
	h, api, proven := gateUnderTest(panickingVerifier{})
	for _, c := range []navRequest{
		{target: "/api/nav/ping", header: bearer(rightCredential)},
		{method: http.MethodPost, target: "/api/nav/revoke", header: map[string]string{"X-Test-Proof": "yes"}},
		{target: "/api/nav/info"},
	} {
		w := c.serve(h)
		if w.Code != http.StatusInternalServerError || w.Body.Len() != 0 {
			t.Errorf("%s: %d %q", c.target, w.Code, w.Body)
		}
		checkNavHeaders(t, c.target, w.Header())
	}
	if api.Load() != 0 || proven.Load() != 0 {
		t.Error("a request passed a verifier that panicked")
	}
}
