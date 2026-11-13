package daemon

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/navauth"
)

type lifeOpaqueAddress struct{ calls *atomic.Int32 }

func (a lifeOpaqueAddress) Network() string { a.calls.Add(1); return "invented" }
func (a lifeOpaqueAddress) String() string {
	a.calls.Add(1)
	return "invented endpoint must not be formatted"
}

func TestNavigatorRejectedProductionEndpointAllowsReentrantReplacement(t *testing.T) {
	for _, kind := range []string{"typed nil", "wrong port", "foreign address", "opaque address"} {
		t.Run(kind, func(t *testing.T) {
			s := lifeServer(t, nil)
			var binds, observations, methods atomic.Int32
			var first *lifeTrackedListener
			var replacement *navGeneration
			s.navLifecycle.bind = func(addr string) (net.Listener, error) {
				_, text, _ := net.SplitHostPort(addr)
				port, _ := strconv.Atoi(text)
				endpoint := net.Addr(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
				if binds.Add(1) != 1 {
					return &fakeListener{addr: endpoint}, nil
				}
				switch kind {
				case "typed nil":
					var missing *net.TCPAddr
					endpoint = missing
				case "wrong port":
					endpoint = &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port + 1}
				case "foreign address":
					endpoint = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 5), Port: port}
				case "opaque address":
					endpoint = lifeOpaqueAddress{&methods}
				}
				first = &lifeTrackedListener{Listener: &fakeListener{addr: endpoint}}
				first.address = func() net.Addr { observations.Add(1); s.navigatorPort(); return endpoint }
				first.closing = func() {
					if s.navigatorPort() != 0 {
						t.Error("rejected endpoint retained exposure")
					}
					body, err := os.ReadFile(navTokenPath())
					if err != nil || !strings.HasPrefix(string(body), "failed ") {
						t.Error("failure was not published before endpoint cleanup")
					}
					replacement, _ = s.startNavigator(s.beginNavigator())
				}
				return first, nil
			}
			done := make(chan struct{})
			var rejected *navGeneration
			go func() { rejected, _ = s.startNavigator(s.beginNavigator()); close(done) }()
			lifeWait(t, done)
			if replacement != nil {
				t.Cleanup(func() { lifeCleanup(t, replacement) })
			}
			if rejected != nil || replacement == nil || observations.Load() != 1 || methods.Load() != 0 || first.closes.Load() != 1 || s.navigatorPort() != replacement.port {
				t.Fatal("rejected production endpoint violated ownership/cleanup")
			}
			p := lifeProof(t, replacement, "GET", "/api/nav/info", "", 1)
			r := lifeRequest("GET", "/api/nav/info", "")
			r.Header.Set("X-Aiq-Proof", p.String())
			w := httptest.NewRecorder()
			replacement.server.Handler.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal("following replacement did not authenticate")
			}
			lifeSigned(t, w, p)
		})
	}
}

func TestNavigatorAutomaticServeCloseRetiresBeforeBlockedRawClose(t *testing.T) {
	s := lifeServer(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, rawEntered sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var binds atomic.Int32
	var first *lifeTrackedListener
	s.navLifecycle.bind = func(addr string) (net.Listener, error) {
		_, text, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(text)
		ln := &lifeTrackedListener{Listener: &fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}}
		if binds.Add(1) == 1 {
			first = ln
			ln.closing = func() { rawEntered.Do(func() { close(entered) }); <-release }
		}
		return ln, nil
	}
	a, _ := lifeStart(t, s)
	t.Cleanup(unblock) // installed before Serve can enter the barrier
	done := make(chan struct{})
	go func() { s.serveNavigator(a); close(done) }()
	lifeWait(t, entered) // fake Accept failed; net/http's OWN deferred Close ran first
	if s.navigatorPort() != 0 {
		t.Fatal("automatic Close reached raw transport before retirement")
	}
	if _, err := a.auth.state.Now(); err == nil {
		t.Fatal("auth remained active during blocked automatic Close")
	}
	failed, err := os.ReadFile(navTokenPath())
	if err != nil || !strings.HasPrefix(string(failed), "failed ") {
		t.Fatal("automatic Close did not publish failure first")
	}
	b, _ := lifeStart(t, s)
	before, _ := os.ReadFile(navTokenPath())
	// A later Close is not a join; it must return without touching B while the
	// actual first raw Close is still blocked.
	repeated := make(chan struct{})
	go func() { a.Close(); close(repeated) }()
	lifeWait(t, repeated)
	select {
	case <-done:
		t.Fatal("blocked first Close was falsely considered complete")
	default:
	}
	after, _ := os.ReadFile(navTokenPath())
	if string(before) != string(after) {
		t.Fatal("repeated Close changed replacement")
	}
	unblock()
	lifeWait(t, done)
	after, _ = os.ReadFile(navTokenPath())
	if first.closes.Load() != 1 || string(before) != string(after) || s.navigatorPort() != b.port {
		t.Fatal("automatic Close completion retired replacement")
	}
}

func TestNavigatorStaleEmptyCallbacksNeverWriteIdenticalTerminalState(t *testing.T) {
	for _, state := range []string{navOff, navRefused, navFailed, "live then failed"} {
		t.Run(state, func(t *testing.T) {
			s := lifeServer(t, nil)
			var writes atomic.Int32
			s.navLifecycle.write = func(path string, body []byte) error { writes.Add(1); return fsutil.WriteFileAtomic(path, body) }
			stale := s.beginNavigator()
			current := s.beginNavigator()
			want := state
			if state == "live then failed" {
				g, h := s.startNavigator(current)
				if g == nil {
					t.Fatal("live ABA control failed")
				}
				t.Cleanup(func() { lifeCleanup(t, g) })
				current = s.navigatorEnds(h, "current generation stopped")
				want = navFailed
			} else {
				var changed bool
				current, changed, _ = s.finishNavigator(current, state, nil)
				if !changed {
					t.Fatal("current terminal publication failed")
				}
			}
			before, _ := os.ReadFile(navTokenPath())
			count := writes.Load()
			for i := 0; i < 2; i++ {
				s.navigatorEnds(stale, "obsolete empty callback")
			}
			after, _ := os.ReadFile(navTokenPath())
			if count != writes.Load() || string(before) != string(after) || !strings.HasPrefix(string(after), want+" ") {
				t.Fatal("stale empty callback wrote even an identical terminal record")
			}
			if _, changed, _ := s.finishNavigator(current, navFailed, nil); !changed {
				t.Fatal("unsuperseded synchronous successor lost authority")
			}
		})
	}
}

func TestNavigatorRetainedKeepaliveCannotBorrowReplacementCredentials(t *testing.T) {
	s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("invented response")) }))
	a, _ := lifeStart(t, s)
	oldDone := make(chan struct{})
	go func() { s.serveNavigator(a); close(oldDone) }()
	t.Cleanup(func() { a.Close(); lifeWait(t, oldDone) })
	oldCredential := lifeCredential(t, a, 1)
	oldProof := lifeProof(t, a, "GET", "/api/nav/info", "", 2)
	pendingCode := func(g *navGeneration, seed byte) string {
		const body = `{"action":"issue"}`
		p := lifeProof(t, g, "POST", "/api/nav/login", body, seed)
		facts, err := g.auth.state.Admit("POST", "/api/nav/login", p, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		code, err := g.auth.state.Issue(facts)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	oldCode := pendingCode(a, 3)
	c, err := net.DialTimeout("tcp", a.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	reader := bufio.NewReader(c)
	request := func(method, path, body, credential string, proof *navauth.Proof) int {
		c.SetDeadline(time.Now().Add(3 * time.Second))
		var header strings.Builder
		fmt.Fprintf(&header, "%s %s HTTP/1.1\r\nHost: %s\r\n", method, path, a.Addr().String())
		if credential != "" {
			fmt.Fprintf(&header, "Authorization: Bearer %s\r\n", credential)
		}
		if proof != nil {
			fmt.Fprintf(&header, "X-Aiq-Proof: %s\r\n", proof.String())
		}
		if method == "POST" {
			fmt.Fprintf(&header, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
		}
		if _, err := fmt.Fprintf(c, "%s\r\n%s", header.String(), body); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(reader, &http.Request{Method: method})
		if err != nil {
			t.Fatal("retained connection closed instead of refusing a new admission")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if err != nil || resp.Close {
			t.Fatal("keepalive control did not remain usable")
		}
		if resp.StatusCode == 200 && string(data) != "invented response" {
			t.Fatal("unexpected invented response")
		}
		return resp.StatusCode
	}
	if request("GET", "/api/nav/invented", "", oldCredential, nil) != 200 {
		t.Fatal("old keepalive never authenticated")
	}
	b, _ := lifeStart(t, s)
	newDone := make(chan struct{})
	go func() { s.serveNavigator(b); close(newDone) }()
	t.Cleanup(func() { b.Close(); lifeWait(t, newDone) })
	lifeWait(t, oldDone) // listener Serve ended; this admitted connection remains
	newCredential := lifeCredential(t, b, 2)
	if request("GET", "/api/nav/invented", "", oldCredential, nil) != 401 || request("GET", "/api/nav/invented", "", newCredential, nil) != 401 {
		t.Fatal("old connection borrowed current authentication")
	}
	newProof := lifeProof(t, b, "GET", "/api/nav/info", "", 10)
	if request("GET", "/api/nav/info", "", "", &oldProof) != 401 || request("GET", "/api/nav/info", "", "", &newProof) != 401 {
		t.Fatal("old connection borrowed replacement proof authority")
	}
	status, header, body := lifeHTTP(t, b, "GET", "/api/nav/info", "", "", &newProof, "")
	if status != 200 {
		t.Fatal("old connection consumed replacement proof")
	}
	lifeHTTPProof(t, status, header, body, newProof)
	newCode := pendingCode(b, 11)
	for _, code := range []string{oldCode, newCode} {
		body := `{"action":"exchange","code":"` + code + `"}`
		if request("POST", "/api/nav/login", body, "", nil) != 401 {
			t.Fatal("old connection accepted old/replacement code")
		}
	}
	status, _, _ = lifeHTTP(t, b, "POST", "/api/nav/login", `{"action":"exchange","code":"`+newCode+`"}`, "", nil, "")
	if status != 200 {
		t.Fatal("old connection consumed replacement code")
	}
	status, _, body = lifeHTTP(t, b, "GET", "/api/nav/invented", "", newCredential, nil, "")
	if status != 200 || string(body) != "invented response" {
		t.Fatal("new generation healthy credential failed")
	}
}

func TestNavigatorParkedPreparationHasZeroWritesAfterLiveReplacement(t *testing.T) {
	for _, at := range []string{"bind success", "bind failure", "address", "entropy", "metadata"} {
		t.Run(at, func(t *testing.T) {
			s := lifeServer(t, nil)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var unblockOnce sync.Once
			unblock := func() { unblockOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			park := func() { close(entered); <-release }
			var binds, tokens, hosts, writes atomic.Int32
			var first *lifeTrackedListener
			s.navLifecycle.write = func(path string, body []byte) error { writes.Add(1); return fsutil.WriteFileAtomic(path, body) }
			s.navLifecycle.bind = func(addr string) (net.Listener, error) {
				_, text, _ := net.SplitHostPort(addr)
				port, _ := strconv.Atoi(text)
				n := binds.Add(1)
				endpoint := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
				ln := &lifeTrackedListener{Listener: &fakeListener{addr: endpoint}}
				ln.address = func() net.Addr {
					if n == 1 && at == "address" {
						park()
					}
					return endpoint
				}
				if n == 1 {
					first = ln
					if at == "bind success" || at == "bind failure" {
						park()
					}
					if at == "bind failure" {
						return ln, io.EOF
					}
				}
				return ln, nil
			}
			s.navLifecycle.token = func() (string, error) {
				if tokens.Add(1) == 1 && at == "entropy" {
					park()
				}
				return navauth.Encode32(inventedLifeKey), nil
			}
			s.navLifecycle.hostname = func() (string, error) {
				if hosts.Add(1) == 1 && at == "metadata" {
					park()
				}
				return "invented-host", nil
			}
			stale := s.beginNavigator()
			var obsolete *navGeneration
			go func() { obsolete, _ = s.startNavigator(stale); close(done) }()
			lifeWait(t, entered)
			b, _ := lifeStart(t, s)
			p := lifeProof(t, b, "GET", "/api/nav/info", "", 1)
			r := lifeRequest("GET", "/api/nav/info", "")
			r.Header.Set("X-Aiq-Proof", p.String())
			w := httptest.NewRecorder()
			b.server.Handler.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal("replacement did not authenticate during parked preparation")
			}
			before, _ := os.ReadFile(navTokenPath())
			history, _ := os.ReadFile(navPortsPath())
			count := writes.Load()
			unblock()
			lifeWait(t, done)
			after, _ := os.ReadFile(navTokenPath())
			historyAfter, _ := os.ReadFile(navPortsPath())
			if obsolete != nil || first.closes.Load() != 1 || writes.Load() != count || string(before) != string(after) || string(history) != string(historyAfter) || s.navigatorPort() != b.port {
				t.Fatal("stale acquisition/preparation performed a later write or touched replacement")
			}
		})
	}
}
