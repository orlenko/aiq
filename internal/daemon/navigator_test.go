package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/filelock"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// lockedBuffer collects a running daemon's log.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// useDataDir points aiq at a fresh data directory, and gives it back.
func useDataDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", root)
	t.Setenv("PATH", os.Getenv("PATH")) // Run may reorder PATH
	return root
}

// freeLoopback is a loopback address nothing listens on, for the status page.
func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func releaseInstanceLock() {
	if instanceLock != nil {
		instanceLock.Close()
		instanceLock = nil
	}
}

// testDaemon is Run going in this process with its loops left out.
type testDaemon struct {
	t      *testing.T
	s      *Server
	listen string
	log    *lockedBuffer
	stop   chan struct{}
	done   chan error

	mu     sync.Mutex
	navSrv *http.Server
}

// startDaemon runs a daemon on the current data directory until the test stops it.
func startDaemon(t *testing.T, navigator bool) *testDaemon {
	t.Helper()
	cfg := config.Default()
	cfg.Daemon.Listen = freeLoopback(t)
	cfg.UI.Navigator = navigator
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &testDaemon{t: t, listen: cfg.Daemon.Listen, log: &lockedBuffer{}, stop: make(chan struct{}), done: make(chan error, 1)}
	d.s = New(cfg, &pool.Pool{Cfg: cfg, St: st}, log.New(d.log, "", 0))

	loops, status, nav := startLoops, serveStatus, serveNav
	ready, navReady := make(chan struct{}), make(chan struct{})
	startLoops = func(*Server) {}
	serveStatus = func(srv *http.Server, ln net.Listener) error {
		go srv.Serve(ln)
		close(ready)
		<-d.stop
		srv.Close()
		return errors.New("stopped by the test")
	}
	serveNav = func(srv *http.Server, ln net.Listener) error {
		d.mu.Lock()
		d.navSrv = srv
		d.mu.Unlock()
		close(navReady)
		return srv.Serve(ln)
	}
	t.Cleanup(func() {
		d.stopAll()
		startLoops, serveStatus, serveNav = loops, status, nav
	})
	go func() { d.done <- d.s.Run() }()
	select {
	case <-ready:
	case err := <-d.done:
		t.Fatalf("Run returned %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not start serving")
	}
	d.s.mu.Lock()
	served := d.s.navPort != 0
	d.s.mu.Unlock()
	if served {
		select {
		case <-navReady:
		case <-time.After(10 * time.Second):
			t.Fatal("the navigator did not start serving")
		}
	}
	return d
}

// stopNavigator makes the navigator's Serve fail, and waits until the daemon has
// recorded it.
func (d *testDaemon) stopNavigator() {
	d.t.Helper()
	d.mu.Lock()
	srv := d.navSrv
	d.navSrv = nil
	d.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Close()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(d.log.String(), "navigator not served: serving it") {
		if time.Now().After(deadline) {
			d.t.Fatal("the navigator's end was not logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// It logs under the lock, and writes the file before letting go.
	d.s.mu.Lock()
	d.s.mu.Unlock()
}

// stopAll ends Run and lets go of the lock, so another start can follow.
func (d *testDaemon) stopAll() {
	d.t.Helper()
	d.stopNavigator()
	select {
	case <-d.stop:
		return
	default:
	}
	close(d.stop)
	select {
	case <-d.done:
	case <-time.After(10 * time.Second):
		d.t.Error("Run did not return")
	}
	releaseInstanceLock()
}

func (d *testDaemon) get(path string) (*http.Response, string) {
	d.t.Helper()
	return getURL(d.t, "http://"+d.listen+path)
}

func getURL(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := localClient(5 * time.Second).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

// daemonObject is the daemon object of /api/state.
func (d *testDaemon) daemonObject() map[string]any {
	d.t.Helper()
	resp, body := d.get("/api/state")
	if resp.StatusCode != 200 {
		d.t.Fatalf("/api/state: %d %s", resp.StatusCode, body)
	}
	var doc struct {
		Daemon map[string]any `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		d.t.Fatal(err)
	}
	return doc.Daemon
}

var (
	navOnLine    = regexp.MustCompile(`^on (\d+) (\d+) (\d+) ([A-Za-z0-9_-]{43})\n$`)
	navStateLine = regexp.MustCompile(`^(off|refused|failed) (\d+) (\d+)\n$`)
)

// navFile reads ui-token, checking it is the owner's alone.
func navFile(t *testing.T) string {
	t.Helper()
	info, err := os.Stat(navTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("ui-token has mode %v", info.Mode().Perm())
	}
	b, err := os.ReadFile(navTokenPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// navOnFields checks an on line and gives its port and token.
func navOnFields(t *testing.T, s *Server, line string) (port int, token string) {
	t.Helper()
	m := navOnLine.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("ui-token is not an on line: %q", line)
	}
	if m[1] != strconv.Itoa(os.Getpid()) || m[2] != strconv.FormatInt(s.started.Unix(), 10) {
		t.Errorf("ui-token names pid %s start %s, want %d %d", m[1], m[2], os.Getpid(), s.started.Unix())
	}
	port, _ = strconv.Atoi(m[3])
	return port, m[4]
}

func wantNavState(t *testing.T, s *Server, want string) {
	t.Helper()
	line := navFile(t)
	m := navStateLine.FindStringSubmatch(line)
	if m == nil || m[1] != want {
		t.Fatalf("ui-token is %q, want %s", line, want)
	}
	if m[2] != strconv.Itoa(os.Getpid()) || m[3] != strconv.FormatInt(s.started.Unix(), 10) {
		t.Errorf("ui-token names pid %s start %s, want %d %d", m[2], m[3], os.Getpid(), s.started.Unix())
	}
}

// checkIdentity is what aiq daemon status, aiq doctor and the installer's readiness
// wait rely on: the identity header as before, and the navigator's beside it.
func checkIdentity(t *testing.T, d *testDaemon) {
	t.Helper()
	resp, body := d.get("/healthz")
	if body != "ok\n" || resp.Header.Get(identityHeader) != Identity() || len(resp.Header.Values(identityHeader)) != 1 {
		t.Errorf("/healthz: %q, %s %q", body, identityHeader, resp.Header.Values(identityHeader))
	}
	want := fmt.Sprintf("pid=%d; start=%d; nav=0", os.Getpid(), d.s.started.Unix())
	if got := resp.Header.Get(navHeader); got != want {
		t.Errorf("%s is %q, want %q", navHeader, got, want)
	}
	if alive, identified := Health(d.listen); !alive || !identified {
		t.Errorf("Health: alive %v, identified %v", alive, identified)
	}
	if olderDaemonAnswers(d.listen) {
		t.Error("the daemon reads as an older one")
	}
	for _, path := range []string{"/", "/help", "/api/state"} {
		if resp, _ := d.get(path); resp.Header.Get(identityHeader) != "" || resp.Header.Get(navHeader) != "" {
			t.Errorf("%s sends the health headers", path)
		}
	}
}

func TestNavigatorIsOffByDefault(t *testing.T) {
	useDataDir(t)
	d := startDaemon(t, false)
	wantNavState(t, d.s, navOff)
	if _, ok := d.daemonObject()["navigator_port"]; ok {
		t.Error("/api/state names a navigator port")
	}
	if _, err := os.Lstat(navPortsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ui-ports exists with the navigator off: %v", err)
	}
	checkIdentity(t, d)
}

func TestNavigatorServesNothingYetOnItsOwnPort(t *testing.T) {
	useDataDir(t)
	if _, err := os.Lstat(navPortsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("this is not a first start")
	}
	d := startDaemon(t, true)
	port, token := navOnFields(t, d.s, navFile(t))
	if port < navPortMin || port > navPortMax {
		t.Errorf("port %d is outside the navigator's range", port)
	}
	if got := d.daemonObject()["navigator_port"]; got != float64(port) {
		t.Errorf("/api/state navigator_port is %v, want %d", got, port)
	}
	ports, torn, err := readNavPorts(navPortsPath())
	if err != nil || torn || len(ports) != 1 || ports[0].port != port {
		t.Errorf("ui-ports after a first start: %v, torn %v, %v", ports, torn, err)
	}
	checkIdentity(t, d)

	nav := "http://127.0.0.1:" + strconv.Itoa(port)
	resp, body := getURL(t, nav+"/")
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("the navigator answered %d with %v", resp.StatusCode, resp.Header)
	}
	bodies := []string{body}
	req, _ := http.NewRequest("GET", nav+"/", nil)
	req.Host = "evil.example:" + strconv.Itoa(port)
	rebound, err := localClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rebound.Body)
	rebound.Body.Close()
	if rebound.StatusCode != http.StatusForbidden {
		t.Errorf("a rebound name got %d from the navigator", rebound.StatusCode)
	}
	bodies = append(bodies, string(b))
	for _, path := range []string{"/", "/help", "/api/state", "/healthz", "/api/nav/hello"} {
		resp, body := d.get(path)
		bodies = append(bodies, body)
		for _, vs := range resp.Header {
			bodies = append(bodies, vs...)
		}
	}
	for i, body := range append(bodies, d.log.String()) {
		if strings.Contains(body, token) {
			t.Errorf("answer %d or the log holds the token", i)
		}
	}
}

// The navigator's handler keeps the status page's checks, so a page on another
// site, a rebound name or another machine gets nothing from it.
func TestNavigatorHandlerIsLocalOnly(t *testing.T) {
	h := navigatorHandler()
	for _, c := range []struct {
		name, peer, host string
		want             int
	}{
		{"a loopback request", "127.0.0.1:50000", "127.0.0.1:50001", http.StatusNotFound},
		{"a rebound name", "127.0.0.1:50000", "evil.example:50001", http.StatusForbidden},
		{"a network peer", "192.0.2.5:50000", "127.0.0.1:50001", http.StatusForbidden},
	} {
		r := httptest.NewRequest("GET", "/api/nav/hello", nil)
		r.RemoteAddr, r.Host = c.peer, c.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestARestartMovesTheNavigatorAndRotatesItsToken(t *testing.T) {
	useDataDir(t)
	first := startDaemon(t, true)
	port1, token1 := navOnFields(t, first.s, navFile(t))
	first.stopAll()
	second := startDaemon(t, true)
	port2, token2 := navOnFields(t, second.s, navFile(t))
	if port1 == port2 || token1 == token2 {
		t.Errorf("a restart kept the port (%d, %d) or the token", port1, port2)
	}
	ports, _, err := readNavPorts(navPortsPath())
	if err != nil || len(ports) != 2 || ports[0].port != port1 || ports[1].port != port2 {
		t.Errorf("ui-ports: %v, %v", ports, err)
	}
	if conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port1))); err == nil {
		conn.Close()
		t.Error("the first start's navigator still answers")
	}
}

func TestANavigatorFailureLeavesTheDaemonServing(t *testing.T) {
	useDataDir(t)
	d := startDaemon(t, true)
	navOnFields(t, d.s, navFile(t))
	d.stopNavigator()
	wantNavState(t, d.s, navFailed)
	if _, ok := d.daemonObject()["navigator_port"]; ok {
		t.Error("/api/state still names the navigator's port")
	}
	checkIdentity(t, d)
}

func TestTheStatusPageStoppingMarksTheNavigatorFailed(t *testing.T) {
	useDataDir(t)
	d := startDaemon(t, true)
	d.stopNavigator() // so only the status page's end writes the file below
	if err := os.WriteFile(navTokenPath(), []byte("on 1 2 3 x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(d.stop)
	if err := <-d.done; err == nil {
		t.Error("Run returned no error")
	}
	releaseInstanceLock()
	wantNavState(t, d.s, navFailed)
}

func TestACorruptPortListFailsClosed(t *testing.T) {
	useDataDir(t)
	if err := os.WriteFile(navPortsPath(), []byte("not a port\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := startDaemon(t, true)
	wantNavState(t, d.s, navFailed)
	if _, ok := d.daemonObject()["navigator_port"]; ok {
		t.Error("/api/state names a navigator port")
	}
	if l := d.log.String(); !strings.Contains(l, "remove or fix "+navPortsPath()) {
		t.Errorf("the log does not say what to do: %s", l)
	}
	checkIdentity(t, d)
}

// With the token unwritable, nothing tells aiq ui how to reach the navigator, so it
// must not be left listening.
func TestAnUnwritableTokenFileClosesTheNavigator(t *testing.T) {
	useDataDir(t)
	if err := os.Mkdir(navTokenPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	d := startDaemon(t, true)
	ports, _, err := readNavPorts(navPortsPath())
	if err != nil || len(ports) != 1 {
		t.Fatalf("ui-ports: %v, %v", ports, err)
	}
	if conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[0].port))); err == nil {
		conn.Close()
		t.Error("the navigator listens with no token file")
	}
	if _, ok := d.daemonObject()["navigator_port"]; ok {
		t.Error("/api/state names a navigator port")
	}
	if l := d.log.String(); !strings.Contains(l, "navigator not served: writing") {
		t.Errorf("the log does not say why: %s", l)
	}
}

// A start that loses the instance lock must leave the winner's file alone.
func TestALosingStartLeavesTheTokenFile(t *testing.T) {
	useDataDir(t)
	const winner = "on 1 2 50000 AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n"
	if err := os.WriteFile(navTokenPath(), []byte(winner), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := filelock.TryExclusive(f); err != nil {
		t.Fatal(err)
	}
	defer func(pause func()) { lockRetryPause = pause }(lockRetryPause)
	lockRetryPause = func() {}
	cfg := config.Default()
	cfg.Daemon.Listen = freeLoopback(t)
	cfg.UI.Navigator = true
	if err := New(cfg, nil, log.New(io.Discard, "", 0)).Run(); err == nil {
		releaseInstanceLock()
		t.Fatal("a second start took the lock")
	}
	if got := navFile(t); got != winner {
		t.Errorf("ui-token is now %q", got)
	}
	if _, err := os.Lstat(navPortsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the losing start drew a port: %v", err)
	}
}

// drawsFrom makes randomPort return ports in order, then fail.
func drawsFrom(t *testing.T, ports ...int) *int {
	t.Helper()
	n := 0
	saved := randomPort
	t.Cleanup(func() { randomPort = saved })
	randomPort = func() (int, error) {
		if n == len(ports) {
			return 0, errors.New("no more draws")
		}
		n++
		return ports[n-1], nil
	}
	return &n
}

// holdPort listens on a port in the navigator's range.
func holdPort(t *testing.T) int {
	t.Helper()
	for range 100 {
		port, err := randomPort()
		if err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			t.Cleanup(func() { ln.Close() })
			return port
		}
	}
	t.Fatal("no port in the range to hold")
	return 0
}

func TestRandomPortStaysInTheRange(t *testing.T) {
	low, high := false, false
	for range 2000 {
		port, err := randomPort()
		if err != nil {
			t.Fatal(err)
		}
		if port < navPortMin || port > navPortMax {
			t.Fatalf("drew %d", port)
		}
		low = low || port < (navPortMin+navPortMax)/2
		high = high || port >= (navPortMin+navPortMax)/2
	}
	if !low || !high {
		t.Error("the draws covered only half the range")
	}
}

func TestNavigatorPortsNeverRepeat(t *testing.T) {
	useDataDir(t)
	seen := map[int]bool{}
	for i := range 100 {
		ln, port, err := bindNavigator()
		if err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		ln.Close()
		if seen[port] || port == 0 {
			t.Fatalf("start %d drew %d again", i, port)
		}
		seen[port] = true
	}
	info, err := os.Stat(navPortsPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ui-ports: %v, %v", info, err)
	}
	ports, torn, err := readNavPorts(navPortsPath())
	if err != nil || torn || len(ports) < 100 {
		t.Errorf("ui-ports has %d lines, torn %v, %v", len(ports), torn, err)
	}
}

// A port that is taken is recorded all the same, and the next try draws afresh
// rather than trying the port above it.
func TestATakenPortIsRecordedAndRedrawn(t *testing.T) {
	useDataDir(t)
	held := holdPort(t)
	other := held + 7
	if other > navPortMax {
		other = held - 7
	}
	n := drawsFrom(t, held, other)
	ln, port, err := bindNavigator()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if port != other || *n != 2 {
		t.Errorf("bound %d after %d draws, want %d after 2", port, *n, other)
	}
	ports, _, err := readNavPorts(navPortsPath())
	if err != nil || len(ports) != 2 || ports[0].port != held || ports[1].port != other {
		t.Errorf("ui-ports: %v, %v", ports, err)
	}
}

func TestAListedPortIsNeverDrawn(t *testing.T) {
	useDataDir(t)
	if err := os.WriteFile(navPortsPath(), []byte("50001 1\n50002 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drawsFrom(t, 50001, 50002, 50003)
	ln, port, err := bindNavigator()
	if err != nil {
		t.Skipf("port 50003 is taken here: %v", err)
	}
	ln.Close()
	if port != 50003 {
		t.Errorf("bound %d", port)
	}
}

func TestTheNavigatorGivesUpAfterSixteenTakenPorts(t *testing.T) {
	useDataDir(t)
	var held []int
	for len(held) < navBindTries {
		held = append(held, holdPort(t))
	}
	drawsFrom(t, append(held, held[0]+1)...)
	if ln, _, err := bindNavigator(); err == nil {
		ln.Close()
		t.Fatal("bound past sixteen taken ports")
	}
	ports, _, err := readNavPorts(navPortsPath())
	if err != nil || len(ports) != navBindTries {
		t.Errorf("ui-ports: %d lines, %v", len(ports), err)
	}
}

func TestATornPortListIsCleanedBeforeTheDraw(t *testing.T) {
	useDataDir(t)
	if err := os.WriteFile(navPortsPath(), []byte("50001 1\n50002 2\n5000"), 0o600); err != nil {
		t.Fatal(err)
	}
	ports, torn, err := readNavPorts(navPortsPath())
	if err != nil || !torn || len(ports) != 2 {
		t.Fatalf("read %v, torn %v, %v", ports, torn, err)
	}
	ln, port, err := bindNavigator()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	b, err := os.ReadFile(navPortsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "50001 1\n50002 2\n"+strconv.Itoa(port)+" ") || strings.Count(string(b), "\n") != 3 {
		t.Errorf("ui-ports is now %q", b)
	}
}

func TestThePortListKeepsTheNewest(t *testing.T) {
	useDataDir(t)
	var ports []navPort
	for i := range navPortsKept + 10 {
		ports = append(ports, navPort{navPortMin + i, int64(i)})
	}
	if err := writeNavPorts(navPortsPath(), ports); err != nil {
		t.Fatal(err)
	}
	got, _, err := readNavPorts(navPortsPath())
	if err != nil || len(got) != navPortsKept || got[0].port != navPortMin+10 || got[len(got)-1].port != navPortMin+navPortsKept+9 {
		t.Errorf("kept %d, from %v, %v", len(got), got[:1], err)
	}
}

func TestAnUnreadablePortListFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"no line parses": "garbage\n",
		"spaces":         "50001  1\n",
		"a signed port":  "+50001 1\n",
		"too large":      strings.Repeat("#", navPortsMaxBytes+1),
	} {
		useDataDir(t)
		if err := os.WriteFile(navPortsPath(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if ln, _, err := bindNavigator(); err == nil {
			ln.Close()
			t.Errorf("%s: bound", name)
		} else if !strings.Contains(err.Error(), "remove or fix "+navPortsPath()) {
			t.Errorf("%s: %v", name, err)
		}
	}
	useDataDir(t)
	if err := os.Mkdir(navPortsPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if ln, _, err := bindNavigator(); err == nil {
		ln.Close()
		t.Error("a directory read as an empty list")
	}
}

func TestEmptyPortListIsEmpty(t *testing.T) {
	useDataDir(t)
	if err := os.WriteFile(navPortsPath(), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ports, torn, err := readNavPorts(navPortsPath())
	if err != nil || torn || len(ports) != 0 {
		t.Errorf("%v, torn %v, %v", ports, torn, err)
	}
}

func TestRefusedAndFailedStartsSayWhyInTheTokenFile(t *testing.T) {
	useDataDir(t)
	cfg := config.Default()
	cfg.UI.Navigator = true
	defer releaseInstanceLock()
	defer func(loops func(*Server), wait func()) { startLoops, waitWithoutHTTP = loops, wait }(startLoops, waitWithoutHTTP)
	startLoops, waitWithoutHTTP = func(*Server) {}, func() {}

	_, port, _ := net.SplitHostPort(freeLoopback(t))
	cfg.Daemon.Listen = net.JoinHostPort("0.0.0.0", port)
	s := New(cfg, nil, log.New(io.Discard, "", 0))
	if err := s.Run(); err != nil {
		t.Fatal(err)
	}
	releaseInstanceLock()
	wantNavState(t, s, navRefused)

	if err := os.WriteFile(navTokenPath(), []byte("on 1 2 50000 x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg.Daemon.Listen = taken.Addr().String()
	s = New(cfg, nil, log.New(io.Discard, "", 0))
	if err := s.Run(); err == nil {
		t.Fatal("Run returned no error")
	}
	wantNavState(t, s, navFailed)
	if _, err := os.Lstat(navPortsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a start that could not serve drew a navigator port: %v", err)
	}
}
