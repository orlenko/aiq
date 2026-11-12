package daemon

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type retirementBarrierConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
}

func (c *retirementBarrierConn) SetReadDeadline(at time.Time) error {
	close(c.entered)
	<-c.release
	return c.Conn.SetReadDeadline(at)
}

func TestRetirementAdmissionAndClosedListener(t *testing.T) {
	s, _ := testSlots(1)
	ln := capLoopback(t, s)
	held, _ := acceptOne(t, ln)
	sc := held.(*slotConn)
	barrier := &retirementBarrierConn{Conn: sc.Conn, entered: make(chan struct{}), release: make(chan struct{})}
	sc.Conn = barrier
	defer func() {
		ln.Close()
		held.Close()
		select {
		case <-barrier.release:
		default:
			close(barrier.release)
		}
	}()
	pending, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	result := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if c != nil {
			c.Close()
		}
		result <- err
	}()
	select {
	case <-barrier.entered:
	case <-time.After(time.Second):
		t.Fatal("retirement did not start")
	}
	called := false
	req := httptest.NewRequest("GET", "http://127.0.0.1/ok", nil)
	req = req.WithContext(s.connContext(req.Context(), sc))
	response := httptest.NewRecorder()
	s.handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), nil).ServeHTTP(response, req)
	if called {
		t.Error("application handler admitted after retirement selection")
	}
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Connection") != "close" {
		t.Errorf("retired request: status %d, headers %v", response.Code, response.Header())
	}
	ln.Close()
	held.Close()
	close(barrier.release)
	select {
	case err := <-result:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("closed listener admitted pending connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending Accept did not finish")
	}
	if st := s.stats(); st.used != 0 || st.waiting != 0 {
		t.Errorf("cancelled admission retained slots: %+v", st)
	}
	s.mu.Lock()
	retiring := s.retiring
	s.mu.Unlock()
	if retiring != 0 {
		t.Errorf("%d retired connections remain counted", retiring)
	}
	pending.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := pending.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("cancelled pending socket read: %v, want EOF", err)
	}
	// Closing this listener does not disable shared slots for the other one.
	other := capLoopback(t, s)
	acceptOne(t, other)
}

func TestRetiredNavigatorKeepsItsGuardsAndHeaders(t *testing.T) {
	slots, _ := testSlots(1)
	called := false
	s := &Server{log: log.New(io.Discard, "", 0), navVerifier: testVerifier{},
		navAPI: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })}
	h := slots.server(s.navigatorHandler(), s.navigatorRefusal()).Handler
	for _, tc := range []struct {
		name, method, path, auth, proof, origin, site, host string
		status                                              int
		build                                               bool
	}{
		{name: "hello", method: "GET", path: "/api/nav/hello", status: 503, build: true},
		{name: "page", method: "GET", path: "/", status: 503, build: true},
		{name: "login", method: "POST", path: "/api/nav/login", status: 503, build: true},
		{name: "missing credential", method: "GET", path: "/api/nav/session", status: 401, build: true},
		{name: "invalid credential", method: "GET", path: "/api/nav/session", auth: "Bearer invented-wrong", status: 401, build: true},
		{name: "valid credential", method: "GET", path: "/api/nav/session", auth: "Bearer " + rightCredential, status: 503, build: true},
		{name: "missing proof", method: "GET", path: "/api/nav/info", status: 401, build: true},
		{name: "invalid proof", method: "GET", path: "/api/nav/info", proof: "no", status: 401, build: true},
		{name: "valid proof", method: "GET", path: "/api/nav/info", proof: "yes", status: 503, build: true},
		{name: "revoke without proof", method: "POST", path: "/api/nav/revoke", status: 401, build: true},
		{name: "revoke with proof", method: "POST", path: "/api/nav/revoke", proof: "yes", status: 503, build: true},
		{name: "cross site", method: "GET", path: "/api/nav/hello", site: "cross-site", status: 403, build: true},
		{name: "foreign origin", method: "GET", path: "/api/nav/hello", origin: "https://example.invalid", status: 403},
		{name: "foreign host", method: "GET", path: "/api/nav/hello", host: "example.invalid", status: 403},
		{name: "noncanonical", method: "GET", path: "/api//nav/hello", status: 400, build: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1"+tc.path, nil)
			r.RemoteAddr = "127.0.0.1:12345"
			if tc.host != "" {
				r.Host = tc.host
			}
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set("X-Test-Proof", tc.proof)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			r = r.WithContext(slots.connContext(r.Context(), &slotConn{slots: slots, chosen: true}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Connection") != "close" {
				t.Fatalf("status %d, headers %v", w.Code, w.Header())
			}
			if tc.build {
				for key, want := range map[string]string{
					"X-Aiq-Build": navBuild(), "Cache-Control": "no-store",
					"Content-Security-Policy": navCSP, "X-Content-Type-Options": "nosniff",
					"Referrer-Policy": "no-referrer", "Cross-Origin-Opener-Policy": "same-origin",
					"Origin-Agent-Cluster": "?1",
				} {
					if got := w.Header().Get(key); got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
			}
			if w.Code == 503 && w.Body.Len() != 0 {
				t.Errorf("retirement body: %q", w.Body.String())
			}
			if called {
				t.Fatal("retired connection invoked application handler")
			}
		})
	}
	// The production nil verifier remains fail-closed on the refusal path too.
	s.navVerifier = nil
	h = slots.server(s.navigatorHandler(), s.navigatorRefusal()).Handler
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/nav/session", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Authorization", "Bearer "+rightCredential)
	r = r.WithContext(slots.connContext(r.Context(), &slotConn{slots: slots, chosen: true}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("X-Aiq-Build") != navBuild() || called {
		t.Fatalf("nil verifier refusal: status %d, headers %v, application called %v", w.Code, w.Header(), called)
	}
}

func TestRetiredStatusConnectionKeepsLoopbackGuards(t *testing.T) {
	slots, _ := testSlots(1)
	called := false
	app := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
	h := slots.server(localOnly(app), localOnly(connectionUnavailable())).Handler
	for _, tc := range []struct {
		name, host, origin string
		status             int
	}{
		{name: "local", status: 503},
		{name: "foreign host", host: "example.invalid", status: 403},
		{name: "foreign origin", origin: "https://example.invalid", status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil)
			r.RemoteAddr = "127.0.0.1:12345"
			if tc.host != "" {
				r.Host = tc.host
			}
			r.Header.Set("Origin", tc.origin)
			r = r.WithContext(slots.connContext(r.Context(), &slotConn{slots: slots, chosen: true}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Connection") != "close" || called {
				t.Fatalf("status %d, headers %v, application called %v", w.Code, w.Header(), called)
			}
		})
	}
}
