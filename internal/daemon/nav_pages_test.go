package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNavigatorStaticPagesBuildAndAllowlist(t *testing.T) {
	h := NavigatorPages()
	build := NavigatorBuild()
	sum := sha256.New()
	for _, body := range [][]byte{navShellTemplate, navBootstrapJS, navSessionJS, navCodecJS, navAuthCSS} {
		sum.Write(body)
	}
	// Independent explicit production closure, not the implementation's registry.
	for _, name := range []string{"app.js", "tab_proof.js", "read_broker.js", "session_controller.js", "controller_data.js", "feed.js", "feed_data.js", "dom.js", "asks_controller.js", "asks_view.js", "asks_data.js", "app.css", "session_controller.css", "feed.css", "dom.css", "asks_controller.css", "asks_view.css"} {
		body, err := os.ReadFile("ui/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum.Write(body)
	}
	if !strings.HasSuffix(build, "+"+hex.EncodeToString(sum.Sum(nil))[:12]) {
		t.Fatal("getter does not cover exact served dependencies")
	}
	for _, path := range []string{"/", "/sessions?dir=%2Finvented", "/asks?all=true", "/assets/" + build + "/auth_bootstrap.js", "/assets/" + build + "/auth_session.js", "/assets/" + build + "/auth_codec.js", "/assets/" + build + "/auth.css"} {
		get := httptest.NewRecorder()
		h.ServeHTTP(get, httptest.NewRequest("GET", path, nil))
		if get.Code != 200 || get.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("allowlisted page refused")
		}
		head := httptest.NewRecorder()
		h.ServeHTTP(head, httptest.NewRequest("HEAD", path, nil))
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
			t.Fatal("HEAD mismatch")
		}
		if strings.HasPrefix(path, "/assets/") && strings.HasSuffix(path, ".js") && get.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Fatal("module MIME")
		}
	}
	root := httptest.NewRecorder()
	h.ServeHTTP(root, httptest.NewRequest("GET", "/", nil))
	body := root.Body.String()
	if strings.Contains(body, "__AIQ_NAV_BUILD__") || !strings.Contains(body, `data-aiq-build="`+build+`"`) {
		t.Fatal("captured shell build missing")
	}
	if strings.Index(body, "nav-auth-status") > strings.Index(body, "<script") || strings.Index(body, "<script") > strings.Index(body, "stylesheet") || strings.Contains(body, "<script type=") || strings.Contains(body, " defer") || strings.Contains(body, " async") {
		t.Fatal("first classic ordering")
	}
	for _, path := range []string{"/api/nav/info", "/assets/wrong/auth_session.js", "/assets/" + build + "/auth_test.mjs", "/assets/" + build + "/auth_session.js?anything=1", "/sessions?unknown=x"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatal("nonallowlisted bytes served")
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 405 {
		t.Fatal("static method policy")
	}
}

func TestNavigatorSharedRouteVectors(t *testing.T) {
	raw, err := os.ReadFile("ui/auth_route_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Route string `json:"route"`
		OK    bool   `json:"ok"`
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 21 {
		t.Fatal("route vector fixture incomplete")
	}
	for _, row := range rows {
		u, e := url.Parse(row.Route)
		got := e == nil && navPageRoute(u)
		if got != row.OK {
			t.Errorf("route admission differs for invented vector %q", row.Route)
		}
	}
}

func TestNavigatorPagesCapturedAndRefusedNatively(t *testing.T) {
	s := lifeServer(t, nil)
	var calls atomic.Int32
	pages := NavigatorPages()
	s.MountNavigatorPages(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); pages.ServeHTTP(w, r) }))
	g, _ := lifeStart(t, s)
	var choose atomic.Bool
	original := g.server.ConnContext
	g.server.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		ctx = original(ctx, c)
		if choose.Swap(false) {
			if sc, ok := ctx.Value(slotKey{}).(*slotConn); ok {
				s.slots.mu.Lock()
				sc.chosen = true
				s.slots.retiring++
				s.slots.mu.Unlock()
			}
		}
		return ctx
	}
	done := make(chan struct{})
	go func() { s.serveNavigator(g); close(done) }()
	t.Cleanup(func() { g.Close(); lifeWait(t, done) })
	s.MountNavigatorPages(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("old generation consulted replacement pages") }))
	status, header, body := lifeHTTP(t, g, "GET", "/", "", "", nil, "")
	if status != 200 || header.Get("Content-Security-Policy") != navCSP || !bytes.Contains(body, []byte("aiq navigator")) {
		t.Fatal("native captured pages not served")
	}
	before := calls.Load()
	// Force selection through the actual connection context; this is no claim
	// about native retirement victim choice or socket deadlines.
	for _, target := range []string{"/", "/assets/" + navBuild() + "/auth_bootstrap.js"} {
		choose.Store(true)
		status, _, _ := lifeHTTP(t, g, "GET", target, "", "", nil, "")
		if status != 503 || calls.Load() != before {
			t.Fatal("refusal invoked normal static pages")
		}
	}
	status, _, body = lifeHTTP(t, g, "GET", "/assets/"+navBuild()+"/auth_codec.js", "", "", nil, "")
	if status != 200 || len(body) == 0 {
		t.Fatal("healthy native asset control")
	}
}

func TestNavigatorAuthNodeFixtures(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node required in CI")
		}
		t.Skip("node unavailable; auth fixtures NOT RUN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--experimental-vm-modules", "--test", "--test-reporter=tap", "ui/auth_test.mjs")
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("auth fixtures failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "# pass 25") || !strings.Contains(string(out), "# fail 0") || !strings.Contains(string(out), "# skipped 0") {
		t.Fatalf("auth fixtures incomplete:\n%s", out)
	}
}
