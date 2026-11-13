package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/navauth"
)

var inventedLifeKey = [32]byte{1, 2, 3, 4, 5, 6, 7, 8}

func lifeServer(t *testing.T, api http.Handler) *Server {
	t.Helper()
	useDataDir(t)
	cfg := config.Default()
	cfg.UI.Navigator = true
	s := New(cfg, nil, log.New(io.Discard, "", 0))
	s.started = time.Unix(123, 0)
	s.slots = newConnSlots(4, func(string, ...any) {})
	s.navLifecycle = &navLifecycleDeps{
		token:    func() (string, error) { return navauth.Encode32(inventedLifeKey), nil },
		hostname: func() (string, error) { return "invented-host", nil },
	}
	s.MountNavigator(api)
	return s
}
func lifeStart(t *testing.T, s *Server) (*navGeneration, navHandle) {
	t.Helper()
	g, h := s.startNavigator(s.beginNavigator())
	if g == nil {
		t.Fatal("invented navigator did not start")
	}
	t.Cleanup(func() { lifeCleanup(t, g) })
	return g, h
}
func lifeCleanup(t *testing.T, g *navGeneration) {
	t.Helper()
	done := make(chan struct{})
	go func() { g.Close(); g.server.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("generation cleanup did not complete")
	}
}
func lifeWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("lifecycle barrier timed out")
	}
}
func lifeProof(t *testing.T, g *navGeneration, method, path, body string, seed byte) navauth.Proof {
	t.Helper()
	now, err := g.auth.state.Now()
	if err != nil {
		t.Fatal(err)
	}
	p := navauth.Proof{Timestamp: now, Nonce: [32]byte{seed}}
	p.MAC, err = navauth.RequestMAC(inventedLifeKey, navauth.Binding{Port: g.port, Build: navBuild()}, method, path, now, p.Nonce, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func lifeRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr, r.Host = "127.0.0.1:12345", "127.0.0.1"
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}
func lifeCredential(t *testing.T, g *navGeneration, seed byte) string {
	t.Helper()
	const body = `{"action":"issue"}`
	p := lifeProof(t, g, "POST", "/api/nav/login", body, seed)
	f, err := g.auth.state.Admit("POST", "/api/nav/login", p, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	code, err := g.auth.state.Issue(f)
	if err != nil {
		t.Fatal(err)
	}
	value, err := g.auth.state.Exchange(code)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func lifeSigned(t *testing.T, w *httptest.ResponseRecorder, p navauth.Proof) {
	t.Helper()
	got, err := navauth.Decode32(w.Header().Get("X-Aiq-Response-Proof"))
	want, e := navauth.ResponseMAC(inventedLifeKey, p, w.Code, w.Body.Bytes())
	if err != nil || e != nil || !navauth.EqualMAC(got, want) {
		t.Fatal("response did not authenticate exact status and bytes")
	}
}

func TestNavigatorEmptyOwnershipAndFailedWrites(t *testing.T) {
	s := lifeServer(t, nil)
	var writes atomic.Int32
	s.navLifecycle.write = func(path string, body []byte) error { writes.Add(1); return fsutil.WriteFileAtomic(path, body) }
	stale := s.beginNavigator()
	current := s.beginNavigator()
	current, changed, failed := s.finishNavigator(current, navOff, nil)
	if !changed || failed {
		t.Fatal("off did not publish")
	}
	before, _ := os.ReadFile(navTokenPath())
	count := writes.Load()
	s.navigatorEnds(stale, "obsolete")
	after, _ := os.ReadFile(navTokenPath())
	if writes.Load() != count || string(after) != string(before) {
		t.Fatal("obsolete nil owner overwrote off")
	}
	// The synchronous successor may record its own later failure exactly once.
	next, changed, _ := s.finishNavigator(current, navFailed, nil)
	if !changed {
		t.Fatal("current empty owner could not publish")
	}
	count = writes.Load()
	s.navigatorEnds(current, "obsolete after terminal transition")
	if writes.Load() != count {
		t.Fatal("repeated terminal callback wrote")
	}
	// Even a failed terminal write changes identity; it is not an ABA shortcut.
	next.scope.deps.write = func(string, []byte) error { return fmt.Errorf("invented write refusal") }
	last, changed, failed := s.finishNavigator(next, navRefused, nil)
	if !changed || !failed || last.id == next.id {
		t.Fatal("failed write retained publication identity")
	}
	if _, changed, _ = s.finishNavigator(next, navFailed, nil); changed {
		t.Fatal("old handle survived failed write")
	}
}

func TestNavigatorOldGenerationCannotTouchReplacementOrEmptyState(t *testing.T) {
	s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("invented")) }))
	a, old := lifeStart(t, s)
	credential := lifeCredential(t, a, 1)
	oldHandler := a.server.Handler
	b, current := lifeStart(t, s)
	before, _ := os.ReadFile(navTokenPath())
	s.navigatorEnds(old, "obsolete listener")
	after, _ := os.ReadFile(navTokenPath())
	if string(before) != string(after) || s.navigatorPort() != b.port {
		t.Fatal("old generation replaced current publication")
	}
	for _, h := range []http.Handler{oldHandler, b.server.Handler} {
		r := lifeRequest("GET", "/api/nav/invented", "")
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("old credential accepted by a retained/replacement handler")
		}
	}
	newCredential := lifeCredential(t, b, 2)
	r := lifeRequest("GET", "/api/nav/invented", "")
	r.Header.Set("Authorization", "Bearer "+newCredential)
	w := httptest.NewRecorder()
	b.server.Handler.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "invented" {
		t.Fatal("replacement positive control failed")
	}
	_, changed, _ := s.finishNavigator(current, navOff, nil)
	if !changed {
		t.Fatal("current generation could not retire")
	}
	b.Close()
	before, _ = os.ReadFile(navTokenPath())
	s.navigatorEnds(old, "obsolete after replacement ended")
	after, _ = os.ReadFile(navTokenPath())
	if string(before) != string(after) || !strings.HasPrefix(string(after), "off ") {
		t.Fatal("old live owner survived live-to-empty ABA")
	}
}

type lifeTrackedListener struct {
	net.Listener
	address func() net.Addr
	closing func()
	closes  atomic.Int32
}

func (l *lifeTrackedListener) Addr() net.Addr {
	if l.address != nil {
		return l.address()
	}
	return l.Listener.Addr()
}
func (l *lifeTrackedListener) Close() error {
	l.closes.Add(1)
	if l.closing != nil {
		l.closing()
	}
	return l.Listener.Close()
}

func TestNavigatorAcquisitionCallbacksOutsideSerializer(t *testing.T) {
	s := lifeServer(t, nil)
	var endpointCalls atomic.Int32
	var tracked *lifeTrackedListener
	s.navLifecycle.bind = func(addr string) (net.Listener, error) {
		if s.navigatorPort() != 0 {
			t.Error("binder saw a premature port")
		}
		raw, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		endpoint := raw.Addr()
		tracked = &lifeTrackedListener{Listener: raw}
		tracked.address = func() net.Addr { endpointCalls.Add(1); s.navigatorPort(); return endpoint }
		tracked.closing = func() { s.navigatorPort() }
		// Native bypass cleanup cannot be trapped in an intentionally broken callback.
		t.Cleanup(func() { raw.Close() })
		return tracked, nil
	}
	done := make(chan struct{})
	var g *navGeneration
	go func() { g, _ = s.startNavigator(s.beginNavigator()); close(done) }()
	lifeWait(t, done)
	if g == nil || endpointCalls.Load() != 1 {
		t.Fatal("startup endpoint positive control failed")
	}
	g.Close()
	g.Close()
	if tracked.closes.Load() != 1 {
		t.Fatal("physical listener closed more than once")
	}
}

func TestNavigatorParkedAcquisitionCannotPublishAfterReplacement(t *testing.T) {
	for _, at := range []string{"binder", "address", "entropy"} {
		t.Run(at, func(t *testing.T) {
			s := lifeServer(t, nil)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var tracked *lifeTrackedListener
			park := func() { close(entered); <-release }
			s.navLifecycle.bind = func(addr string) (net.Listener, error) {
				raw, err := net.Listen("tcp", addr)
				if err != nil {
					return nil, err
				}
				t.Cleanup(func() { raw.Close() })
				endpoint := raw.Addr()
				tracked = &lifeTrackedListener{Listener: raw}
				tracked.address = func() net.Addr {
					if at == "address" {
						park()
					}
					return endpoint
				}
				if at == "binder" {
					park()
				}
				return tracked, nil
			}
			if at == "entropy" {
				s.navLifecycle.token = func() (string, error) { park(); return navauth.Encode32(inventedLifeKey), nil }
			}
			stale := s.beginNavigator()
			var result *navGeneration
			go func() { result, _ = s.startNavigator(stale); close(done) }()
			lifeWait(t, entered)
			current := s.beginNavigator()
			current, changed, _ := s.finishNavigator(current, navOff, nil)
			if !changed {
				t.Fatal("replacement off did not publish")
			}
			before, _ := os.ReadFile(navTokenPath())
			history, _ := os.ReadFile(navPortsPath())
			unblock()
			lifeWait(t, done)
			after, _ := os.ReadFile(navTokenPath())
			historyAfter, _ := os.ReadFile(navPortsPath())
			if result != nil || string(before) != string(after) || string(history) != string(historyAfter) || tracked.closes.Load() != 1 {
				t.Fatal("stale preparation wrote or leaked its listener")
			}
			s.navigatorEnds(current, "current empty-state positive")
		})
	}
}

type lifeBodySpy struct {
	io.Reader
	reads int
}

func (b *lifeBodySpy) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *lifeBodySpy) Close() error               { return nil }

func TestNavigatorChosenConnectionNeverCallsLiveAuthentication(t *testing.T) {
	for _, route := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/nav/info", "", 401},
		{"POST", "/api/nav/revoke", "{}", 401},
		{"POST", "/api/nav/login", `{"action":"issue"}`, 503},
	} {
		t.Run(route.path+route.body, func(t *testing.T) {
			s := lifeServer(t, nil)
			g, _ := lifeStart(t, s)
			p := lifeProof(t, g, route.method, route.path, route.body, 1)
			r := lifeRequest(route.method, route.path, route.body)
			r.Header.Set("X-Aiq-Proof", p.String())
			spy := &lifeBodySpy{Reader: strings.NewReader(route.body)}
			r.Body = spy
			r = r.WithContext(context.WithValue(r.Context(), slotKey{}, &slotConn{slots: s.slots, chosen: true}))
			w := httptest.NewRecorder()
			g.server.Handler.ServeHTTP(w, r)
			if w.Code != route.status || spy.reads != 0 || w.Header().Get("X-Aiq-Response-Proof") != "" {
				t.Fatal("retired connection performed authentication/body work")
			}
			r = lifeRequest(route.method, route.path, route.body)
			r.Header.Set("X-Aiq-Proof", p.String())
			w = httptest.NewRecorder()
			g.server.Handler.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Fatal("retired connection consumed the healthy proof")
			}
			lifeSigned(t, w, p)
		})
	}
	t.Run("credential eviction order", func(t *testing.T) {
		s := lifeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
		g, _ := lifeStart(t, s)
		var credentials []string
		for i := 1; i <= navauth.CredentialLimit; i++ {
			credentials = append(credentials, lifeCredential(t, g, byte(i)))
		}
		r := lifeRequest("GET", "/api/nav/invented", "")
		r.Header.Set("Authorization", "Bearer "+credentials[0])
		r = r.WithContext(context.WithValue(r.Context(), slotKey{}, &slotConn{slots: s.slots, chosen: true}))
		w := httptest.NewRecorder()
		g.server.Handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("chosen bearer request was not refused")
		}
		lifeCredential(t, g, 40)
		if g.auth.Credential(credentials[0]) || !g.auth.Credential(credentials[1]) {
			t.Fatal("refusal refreshed a credential's LRU/activity")
		}
	})
}

func TestNavigatorProductionInfoRootHasNoSelectedProject(t *testing.T) {
	s := lifeServer(t, nil)
	g, _ := lifeStart(t, s)
	p := lifeProof(t, g, "GET", "/api/nav/info", "", 1)
	r := lifeRequest("GET", "/api/nav/info", "")
	r.Header.Set("X-Aiq-Proof", p.String())
	w := httptest.NewRecorder()
	g.server.Handler.ServeHTTP(w, r)
	lifeSigned(t, w, p)
	var body navAuthInfo
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Root != "" || body.Hostname != "invented-host" || body.DataDir != g.handle.scope.dataDir {
		t.Fatal("signed metadata did not preserve no-selection scope")
	}
	credential := lifeCredential(t, g, 2)
	for _, path := range []string{"/", "/api/nav/unregistered"} {
		r = lifeRequest("GET", path, "")
		r.Header.Set("Authorization", "Bearer "+credential)
		w = httptest.NewRecorder()
		g.server.Handler.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatal("unregistered production route became a data mount")
		}
	}
}

func TestNavigatorTokenWriteSerializesReplacementWithoutPollingLock(t *testing.T) {
	s := lifeServer(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.navLifecycle.write = func(path string, body []byte) error {
		if strings.HasPrefix(string(body), "on ") && first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return fsutil.WriteFileAtomic(path, body)
	}
	aResult, bResult := make(chan *navGeneration, 1), make(chan *navGeneration, 1)
	var a, b *navGeneration
	bWasLaunched := false
	t.Cleanup(func() {
		unblock()
		for _, item := range []struct {
			value    **navGeneration
			result   <-chan *navGeneration
			launched bool
		}{
			{&a, aResult, true}, {&b, bResult, bWasLaunched},
		} {
			if !item.launched {
				continue
			}
			if *item.value == nil {
				select {
				case *item.value = <-item.result:
				case <-time.After(3 * time.Second):
					t.Error("startup worker did not complete")
					continue
				}
			}
			if g := *item.value; g != nil {
				lifeCleanup(t, g)
			}
		}
	})
	go func() { g, _ := s.startNavigator(s.beginNavigator()); aResult <- g }()
	lifeWait(t, entered)
	if s.navLifeMu.TryLock() {
		s.navLifeMu.Unlock()
		t.Fatal("token write escaped lifecycle transaction")
	}
	if !s.mu.TryLock() {
		t.Fatal("token write held the general polling mutex")
	}
	s.mu.Unlock()
	bLaunched := make(chan struct{})
	bWasLaunched = true
	go func() { close(bLaunched); g, _ := s.startNavigator(s.beginNavigator()); bResult <- g }()
	lifeWait(t, bLaunched)
	unblock()
	receive := func(ch <-chan *navGeneration) *navGeneration {
		select {
		case g := <-ch:
			if g == nil {
				t.Fatal("serialized start failed")
			}
			return g
		case <-time.After(3 * time.Second):
			t.Fatal("serialized start did not finish")
			return nil
		}
	}
	a, b = receive(aResult), receive(bResult)
	before, _ := os.ReadFile(navTokenPath())
	if s.navigatorPort() != b.port {
		t.Fatal("replacement did not win serialization")
	}
	// A worker first scheduled AFTER replacement cannot revive its listener/state.
	done := make(chan struct{})
	go func() { s.serveNavigator(a); close(done) }()
	lifeWait(t, done)
	after, _ := os.ReadFile(navTokenPath())
	if string(before) != string(after) {
		t.Fatal("delayed old Serve republished")
	}
	if _, err := a.auth.state.Now(); err == nil {
		t.Fatal("old authentication state remained active")
	}
	if _, err := b.auth.state.Now(); err != nil {
		t.Fatal("replacement authentication was retired")
	}
}

type lifeHostileFailure struct{ calls *atomic.Int32 }

func (e lifeHostileFailure) Error() string {
	e.calls.Add(1)
	panic("invented error formatter must not run")
}

func TestNavigatorStartupFailuresRetireUnpublishedListeners(t *testing.T) {
	for _, kind := range []string{"entropy", "bad key", "hostname", "invalid metadata", "token write"} {
		t.Run(kind, func(t *testing.T) {
			s := lifeServer(t, nil)
			var calls atomic.Int32
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
			switch kind {
			case "entropy":
				s.navLifecycle.token = func() (string, error) { return "", lifeHostileFailure{&calls} }
			case "bad key":
				s.navLifecycle.token = func() (string, error) { return "invented-invalid", nil }
			case "hostname":
				s.navLifecycle.hostname = func() (string, error) { return "", lifeHostileFailure{&calls} }
			case "invalid metadata":
				s.navLifecycle.hostname = func() (string, error) { return string([]byte{255}), nil }
			case "token write":
				s.navLifecycle.write = func(path string, body []byte) error {
					if strings.HasPrefix(string(body), "on ") {
						return lifeHostileFailure{&calls}
					}
					return fsutil.WriteFileAtomic(path, body)
				}
			}
			g, _ := s.startNavigator(s.beginNavigator())
			body, err := os.ReadFile(navTokenPath())
			if g != nil || err != nil || !strings.HasPrefix(string(body), "failed ") || s.navigatorPort() != 0 || raw == nil || raw.closes.Load() != 1 || calls.Load() != 0 {
				t.Fatal("startup failure leaked exposure/resource/cause")
			}
		})
	}
}

func TestNavigatorRawCloseMayStartReplacementWithoutRecursiveWait(t *testing.T) {
	s := lifeServer(t, nil)
	var first *lifeTrackedListener
	var bindings atomic.Int32
	s.navLifecycle.bind = func(addr string) (net.Listener, error) {
		raw, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { raw.Close() })
		ln := &lifeTrackedListener{Listener: raw}
		if bindings.Add(1) == 1 {
			first = ln
		}
		return ln, nil
	}
	a, _ := lifeStart(t, s)
	var b *navGeneration
	first.closing = func() {
		s.navigatorPort()
		// The first Close already owns cleanup and invalidated A. This recursive
		// call must return, rather than wait inside sync.Once on its own callback.
		a.Close()
		b, _ = s.startNavigator(s.beginNavigator())
	}
	done := make(chan struct{})
	go func() { a.Close(); close(done) }()
	lifeWait(t, done)
	if b == nil || first.closes.Load() != 1 || s.navigatorPort() != b.port {
		t.Fatal("reentrant Close did not preserve replacement")
	}
	t.Cleanup(func() { b.Close(); b.server.Close() })
}

type lifePausedEntropy struct {
	at               int32
	calls            atomic.Int32
	entered, release chan struct{}
}

func (r *lifePausedEntropy) Read(p []byte) (int, error) {
	n := r.calls.Add(1)
	if n == r.at {
		close(r.entered)
		<-r.release
	}
	for i := range p {
		p[i] = byte(int(n) + i + 10)
	}
	return len(p), nil
}

func TestNavigatorRetirementRejectsActualEntropyCommit(t *testing.T) {
	for _, action := range []string{"issue", "exchange"} {
		t.Run(action, func(t *testing.T) {
			s := lifeServer(t, nil)
			reader := &lifePausedEntropy{at: 1, entered: make(chan struct{}), release: make(chan struct{})}
			if action == "exchange" {
				reader.at = 2
			}
			var once sync.Once
			unblock := func() { once.Do(func() { close(reader.release) }) }
			t.Cleanup(unblock)
			// State.New captures this invented reader. Restore BOTH process globals
			// before launching workers; use no production constructor/testing hook.
			var g *navGeneration
			func() {
				oldRandom, oldPort := rand.Reader, randomPort
				defer func() { rand.Reader, randomPort = oldRandom, oldPort }()
				rand.Reader = reader
				randomPort = func() (int, error) { return 50123, nil }
				s.navLifecycle.bind = func(string) (net.Listener, error) {
					return &fakeListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50123}}, nil
				}
				g, _ = lifeStart(t, s)
			}()
			body := `{"action":"issue"}`
			var proof *navauth.Proof
			if action == "issue" {
				p := lifeProof(t, g, "POST", "/api/nav/login", body, 1)
				proof = &p
			} else {
				p := lifeProof(t, g, "POST", "/api/nav/login", body, 1)
				facts, err := g.auth.state.Admit("POST", "/api/nav/login", p, []byte(body))
				if err != nil {
					t.Fatal(err)
				}
				code, err := g.auth.state.Issue(facts)
				if err != nil {
					t.Fatal(err)
				}
				body = `{"action":"exchange","code":"` + code + `"}`
			}
			r := lifeRequest("POST", "/api/nav/login", body)
			if proof != nil {
				r.Header.Set("X-Aiq-Proof", proof.String())
			}
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { g.server.Handler.ServeHTTP(w, r); close(done) }()
			lifeWait(t, reader.entered)
			g.Close()
			unblock()
			lifeWait(t, done)
			if action == "issue" {
				if w.Code != 500 || w.Body.String() != `{"error":"internal_error"}` {
					t.Fatal("retired admitted issue committed after entropy")
				}
				lifeSigned(t, w, *proof)
			} else if w.Code != 401 || w.Body.String() != `{"error":"invalid_link"}` {
				t.Fatal("retired exchange committed after entropy")
			}
		})
	}
}
