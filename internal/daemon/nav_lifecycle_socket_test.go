package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
)

func lifeHTTP(t *testing.T, g *navGeneration, method, path, body, bearer string, proof *navauth.Proof, challenge string) (int, http.Header, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, "http://"+g.Addr().String()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if proof != nil {
		r.Header.Set("X-Aiq-Proof", proof.String())
	}
	if challenge != "" {
		r.Header.Set("X-Aiq-Challenge", challenge)
	}
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, navauth.MaxResponse+1))
	closeErr := resp.Body.Close()
	if err != nil || closeErr != nil || len(data) > navauth.MaxResponse {
		t.Fatal("invented response could not be read completely")
	}
	return resp.StatusCode, resp.Header, data
}
func lifeHTTPProof(t *testing.T, status int, h http.Header, body []byte, p navauth.Proof) {
	t.Helper()
	got, e := navauth.Decode32(h.Get("X-Aiq-Response-Proof"))
	want, e2 := navauth.ResponseMAC(inventedLifeKey, p, status, body)
	if e != nil || e2 != nil || !navauth.EqualMAC(got, want) {
		t.Fatal("native response proof mismatch")
	}
}
func TestNavigatorLiveHTTPAuthentication(t *testing.T) {
	s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("invented response")) }))
	g, _ := lifeStart(t, s)
	done := make(chan struct{})
	go func() { s.serveNavigator(g); close(done) }()
	t.Cleanup(func() { g.Close(); lifeWait(t, done) })
	challenge := [32]byte{77}
	status, header, body := lifeHTTP(t, g, "GET", "/api/nav/hello", "", "", nil, navauth.Encode32(challenge))
	got, e := navauth.Decode32(header.Get("X-Aiq-Hello-Proof"))
	want, e2 := navauth.HelloMAC(inventedLifeKey, navauth.Binding{Port: g.port, Build: navBuild()}, challenge, status, body)
	if status != 200 || e != nil || e2 != nil || !navauth.EqualMAC(got, want) {
		t.Fatal("native challenged hello did not authenticate")
	}
	if strings.Contains(string(body), "invented-host") || header.Get("Content-Security-Policy") != navCSP {
		t.Fatal("hello metadata/guard boundary failed")
	}
	const issue = `{"action":"issue"}`
	p := lifeProof(t, g, "POST", "/api/nav/login", issue, 1)
	status, header, body = lifeHTTP(t, g, "POST", "/api/nav/login", issue, "", &p, "")
	lifeHTTPProof(t, status, header, body, p)
	var issued struct {
		Code string `json:"code"`
	}
	if status != 200 || json.Unmarshal(body, &issued) != nil {
		t.Fatal("native issue failed")
	}
	if _, err := navauth.Decode32(issued.Code); err != nil {
		t.Fatal("native code shape failed")
	}
	exchange := `{"action":"exchange","code":"` + issued.Code + `"}`
	status, _, body = lifeHTTP(t, g, "POST", "/api/nav/login", exchange, "", nil, "")
	var exchanged struct {
		Credential string `json:"credential"`
	}
	if status != 200 || json.Unmarshal(body, &exchanged) != nil {
		t.Fatal("native exchange failed")
	}
	status, _, _ = lifeHTTP(t, g, "POST", "/api/nav/login", exchange, "", nil, "")
	if status != 401 {
		t.Fatal("native link was not one use")
	}
	status, _, body = lifeHTTP(t, g, "GET", "/api/nav/invented", "", exchanged.Credential, nil, "")
	if status != 200 || string(body) != "invented response" {
		t.Fatal("native bearer positive failed")
	}
	p = lifeProof(t, g, "GET", "/api/nav/info", "", 2)
	status, header, body = lifeHTTP(t, g, "GET", "/api/nav/info", "", "", &p, "")
	lifeHTTPProof(t, status, header, body, p)
	var info navAuthInfo
	if status != 200 || json.Unmarshal(body, &info) != nil || info.Root != "" || info.Hostname != "invented-host" {
		t.Fatal("native signed metadata failed")
	}
	p = lifeProof(t, g, "POST", "/api/nav/revoke", "{}", 3)
	status, header, body = lifeHTTP(t, g, "POST", "/api/nav/revoke", "{}", "", &p, "")
	lifeHTTPProof(t, status, header, body, p)
	if status != 200 || string(body) != `{"ok":true}` {
		t.Fatal("native revoke failed")
	}
	status, _, _ = lifeHTTP(t, g, "GET", "/api/nav/invented", "", exchanged.Credential, nil, "")
	if status != 401 {
		t.Fatal("revoked native bearer survived")
	}
}

func TestNavigatorRetirementEndsCappedWaitBeforeAdmittedResponses(t *testing.T) {
	entered, release, queued := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce, queuedOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.Write([]byte("admitted response"))
	}))
	s.slots = newConnSlots(1, func(string, ...any) { queuedOnce.Do(func() { close(queued) }) })
	var raw *lifeTrackedListener
	s.navLifecycle.bind = func(addr string) (net.Listener, error) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { ln.Close() })
		raw = &lifeTrackedListener{Listener: ln}
		return raw, nil
	}
	g, _ := lifeStart(t, s)
	credential := lifeCredential(t, g, 1)
	serveDone, clientDone := make(chan struct{}), make(chan struct{})
	// Release before helper server cleanup, even when an assertion fails.
	t.Cleanup(unblock)
	go func() { s.serveNavigator(g); close(serveDone) }()
	var clientStatus int
	var clientBody string
	var clientErr error
	go func() {
		tr := &http.Transport{Proxy: nil, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		c := &http.Client{Transport: tr, Timeout: 5 * time.Second}
		r, _ := http.NewRequest("GET", "http://"+g.Addr().String()+"/api/nav/invented", nil)
		r.Header.Set("Authorization", "Bearer "+credential)
		resp, err := c.Do(r)
		clientErr = err
		if err == nil {
			clientStatus = resp.StatusCode
			b, e := io.ReadAll(resp.Body)
			clientErr = e
			resp.Body.Close()
			clientBody = string(b)
		}
		close(clientDone)
	}()
	lifeWait(t, entered)
	pending, err := net.DialTimeout("tcp", g.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pending.Close() })
	pending.SetDeadline(time.Now().Add(3 * time.Second))
	lifeWait(t, queued)
	s.slots.mu.Lock()
	waiting, used := s.slots.waiting, s.slots.used
	s.slots.mu.Unlock()
	if waiting != 1 || used != 1 {
		t.Fatal("pending Accept was not positively established")
	}
	g.Close()
	lifeWait(t, serveDone)
	var one [1]byte
	if _, err = pending.Read(one[:]); err == nil {
		t.Fatal("queued socket survived retirement")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("queued socket reached its deadline instead of closing")
	}
	s.slots.mu.Lock()
	waiting, used = s.slots.waiting, s.slots.used
	s.slots.mu.Unlock()
	if waiting != 0 || used != 1 || raw.closes.Load() != 1 {
		t.Fatal("retirement failed capped-wait/one-close ownership")
	}
	select {
	case <-clientDone:
		t.Fatal("retirement interrupted the admitted response")
	default:
	}
	unblock()
	lifeWait(t, clientDone)
	if clientErr != nil || clientStatus != 200 || clientBody != "admitted response" {
		t.Fatal("admitted response could not finish")
	}
	g.Close()
	if raw.closes.Load() != 1 {
		t.Fatal("Serve auto-close/repeated retirement bypassed shared close ownership")
	}
}

type lifeSuccessLog struct {
	entered, release chan struct{}
	once             sync.Once
}

func (l *lifeSuccessLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "aiq daemon listening") {
		l.once.Do(func() { close(l.entered); <-l.release })
	}
	return len(p), nil
}

func TestNavigatorServesBeforeBlockedSuccessLogAndOldRunExit(t *testing.T) {
	s := lifeServer(t, nil)
	s.cfg.Daemon.Listen = freeLoopback(t)
	gate := &lifeSuccessLog{entered: make(chan struct{}), release: make(chan struct{})}
	s.log = log.New(gate, "", 0)
	savedLoops, savedNav, savedStatus := startLoops, serveNav, serveStatus
	stop, done := make(chan struct{}), make(chan struct{})
	ready := make(chan *navGeneration, 2)
	var releaseOnce, stopOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(gate.release) }) }
	finish := func() { stopOnce.Do(func() { close(stop) }) }
	var a, b *navGeneration
	var runErr error
	startLoops = func(*Server) {}
	serveNav = func(srv *http.Server, ln net.Listener) error { ready <- ln.(*navGeneration); return srv.Serve(ln) }
	serveStatus = func(_ *http.Server, ln net.Listener) error { <-stop; ln.Close(); return io.EOF }
	t.Cleanup(func() {
		unblock()
		finish()
		if b != nil {
			b.Close()
			b.server.Close()
		}
		if a != nil {
			a.Close()
			a.server.Close()
		}
		lifeWait(t, done)
		releaseInstanceLock()
		startLoops, serveNav, serveStatus = savedLoops, savedNav, savedStatus
	})
	go func() { runErr = s.Run(); close(done) }()
	lifeWait(t, gate.entered)
	select {
	case a = <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve was not scheduled before success log")
	}
	challenge := [32]byte{90}
	status, header, body := lifeHTTP(t, a, "GET", "/api/nav/hello", "", "", nil, navauth.Encode32(challenge))
	got, e := navauth.Decode32(header.Get("X-Aiq-Hello-Proof"))
	want, e2 := navauth.HelloMAC(inventedLifeKey, navauth.Binding{Port: a.port, Build: navBuild()}, challenge, status, body)
	if status != 200 || e != nil || e2 != nil || !navauth.EqualMAC(got, want) {
		t.Fatal("published auth depended on the parked logger")
	}
	// Replacement completes while the older Run is still parked in its log.
	b, _ = s.startNavigator(s.beginNavigator())
	if b == nil {
		t.Fatal("replacement failed while old log was parked")
	}
	before, err := os.ReadFile(navTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	finish()
	lifeWait(t, done)
	after, err := os.ReadFile(navTokenPath())
	if runErr == nil || err != nil || string(before) != string(after) || s.navigatorPort() != b.port {
		t.Fatal("old Run exit retired the replacement")
	}
	if _, err = b.auth.state.Now(); err != nil {
		t.Fatal("replacement auth was closed by old Run")
	}
}

func TestNavigatorNativeRefusalDoesNotConsumeProof(t *testing.T) {
	s := lifeServer(t, nil)
	g, _ := lifeStart(t, s)
	original := g.server.ConnContext
	var connections atomic.Int32
	// Select the first real accepted connection before dispatch. This exercises
	// the production ConnContext/slot/refusal path without racing TCP deadlines.
	g.server.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		ctx = original(ctx, c)
		if connections.Add(1) == 1 {
			sc, _ := ctx.Value(slotKey{}).(*slotConn)
			if sc != nil {
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
	p := lifeProof(t, g, "GET", "/api/nav/info", "", 1)
	status, header, body := lifeHTTP(t, g, "GET", "/api/nav/info", "", "", &p, "")
	if status != 401 || len(body) != 0 || header.Get("X-Aiq-Response-Proof") != "" {
		t.Fatal("native chosen connection did not preserve unsigned refusal")
	}
	status, header, body = lifeHTTP(t, g, "GET", "/api/nav/info", "", "", &p, "")
	if status != 200 {
		t.Fatal("native refusal consumed the healthy proof")
	}
	lifeHTTPProof(t, status, header, body, p)
}

func TestNavigatorNativeParserTrailersRefuseBeforeAction(t *testing.T) {
	s := lifeServer(t, nil)
	g, _ := lifeStart(t, s)
	var actions, signs atomic.Int32
	issue, sign := g.auth.issue, g.auth.sign
	g.auth.issue = func(f *navauth.Facts) (string, error) { actions.Add(1); return issue(f) }
	g.auth.sign = func(f *navauth.Facts, status int, body []byte) ([32]byte, error) {
		signs.Add(1)
		return sign(f, status, body)
	}
	done := make(chan struct{})
	go func() { s.serveNavigator(g); close(done) }()
	t.Cleanup(func() { g.Close(); lifeWait(t, done) })
	const body = `{"action":"issue"}`
	p := lifeProof(t, g, "POST", "/api/nav/login", body, 1)
	c, err := net.DialTimeout("tcp", g.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(c, "POST /api/nav/login HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nX-Aiq-Proof: %s\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n0\r\nX-Invented-Late: value\r\n\r\n", g.Addr().String(), p.String(), len(body), body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	c.Close()
	if err != nil || resp.StatusCode != 400 || string(data) != `{"error":"invalid_request"}` || actions.Load() != 0 || signs.Load() != 0 || resp.Header.Get("X-Aiq-Response-Proof") != "" {
		t.Fatal("native late trailer reached admission/action/signing")
	}
	status, header, data := lifeHTTP(t, g, "POST", "/api/nav/login", body, "", &p, "")
	if status != 200 || actions.Load() != 1 || signs.Load() != 1 {
		t.Fatal("native trailer refusal spent the proof")
	}
	lifeHTTPProof(t, status, header, data, p)
}
