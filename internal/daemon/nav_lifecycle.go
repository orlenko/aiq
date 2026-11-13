package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/paths"
)

// Configured before Run. File-writer and port-draw seams are non-reentrant;
// barriers in tests are released by another goroutine, never by a snapshot here.
type navLifecycleDeps struct {
	bind     func(string) (net.Listener, error)
	token    func() (string, error)
	hostname func() (string, error)
	write    func(string, []byte) error
}

// Nonzero size matters: distinct empty allocations need not have distinct addresses.
type navIdentity struct{ marker byte }
type navScope struct {
	tokenPath, portsPath, dataDir string
	pid                           int
	started                       int64
	deps                          navLifecycleDeps
}
type navHandle struct {
	id    *navIdentity
	scope *navScope
	gen   *navGeneration
}
type navGeneration struct {
	owner  *Server
	handle navHandle
	auth   *navAuthAdapter
	capped *capListener
	server *http.Server
	addr   net.TCPAddr
	port   int
	closed atomic.Bool
}

func (g *navGeneration) Accept() (net.Conn, error) { return g.capped.Accept() }
func (g *navGeneration) Addr() net.Addr {
	a := g.addr
	a.IP = append(net.IP(nil), a.IP...)
	return &a
}
func (g *navGeneration) Close() error {
	if !g.closed.CompareAndSwap(false, true) {
		return net.ErrClosed // Reservation is not a join of the first Close.
	}
	_, changed, failed := g.owner.finishNavigator(g.handle, navFailed, nil)
	// Close the admission wait signal as well as the raw listener. Serve and
	// retirement share THIS wrapper; neither wraps it in another capListener.
	err := g.capped.Close()
	g.owner.logNavigatorEnd(changed, failed, "serving it")
	return err
}

// beginNavigator is called only after Run acquired the instance lock. A losing
// process must not create a publication identity or write either navigator file.
func (s *Server) beginNavigator() navHandle {
	d := navLifecycleDeps{listenTCP, newNavToken, os.Hostname, fsutil.WriteFileAtomic}
	if p := s.navLifecycle; p != nil {
		if p.bind != nil {
			d.bind = p.bind
		}
		if p.token != nil {
			d.token = p.token
		}
		if p.hostname != nil {
			d.hostname = p.hostname
		}
		if p.write != nil {
			d.write = p.write
		}
	}
	data := strings.Clone(paths.DataDir())
	h := navHandle{id: &navIdentity{1}, scope: &navScope{
		tokenPath: filepath.Join(data, "ui-token"), portsPath: filepath.Join(data, "ui-ports"),
		dataDir: data, pid: os.Getpid(), started: s.started.Unix(), deps: d,
	}}
	s.navLifeMu.Lock()
	old := s.navCurrent
	if old != nil {
		old.auth.Close()
	}
	s.navOwner, s.navCurrent = h.id, nil
	s.mu.Lock()
	s.navPort, s.navToken = 0, ""
	s.mu.Unlock()
	s.navLifeMu.Unlock()
	if old != nil {
		old.Close()
	}
	return h
}

func (s *Server) ownsNavigatorLocked(h navHandle) bool {
	return h.id != nil && h.scope != nil && s.navOwner == h.id && s.navCurrent == h.gen
}
func (s *Server) ownsNavigator(h navHandle) bool {
	s.navLifeMu.Lock()
	defer s.navLifeMu.Unlock()
	return s.ownsNavigatorLocked(h)
}

// Every transition, including a failed file write, invalidates old callbacks.
// Only the synchronous caller receives its successor handle.
func (s *Server) finishNavigator(h navHandle, state string, unpublished *navAuthAdapter) (navHandle, bool, bool) {
	s.navLifeMu.Lock()
	defer s.navLifeMu.Unlock()
	return s.finishNavigatorLocked(h, state, unpublished)
}
func (s *Server) finishNavigatorLocked(h navHandle, state string, unpublished *navAuthAdapter) (navHandle, bool, bool) {
	if unpublished != nil {
		unpublished.Close()
	}
	if h.gen != nil {
		h.gen.auth.Close()
	}
	if !s.ownsNavigatorLocked(h) {
		return h, false, false
	}
	err := h.scope.deps.write(h.scope.tokenPath, []byte(fmt.Sprintf("%s %d %d\n", state, h.scope.pid, h.scope.started)))
	next := navHandle{id: &navIdentity{1}, scope: h.scope}
	s.navOwner, s.navCurrent = next.id, nil
	s.mu.Lock()
	s.navPort, s.navToken = 0, ""
	s.mu.Unlock()
	return next, true, err != nil
}

func (s *Server) logNavigatorEnd(changed, failed bool, phase string) {
	if !changed {
		return
	}
	if s.cfg.UI.Navigator {
		s.log.Printf("navigator not served: %s", phase)
	}
	if failed {
		s.log.Print("navigator: could not record that it is not served")
	}
}
func (s *Server) navigatorEnds(h navHandle, phase string) navHandle {
	next, changed, failed := s.finishNavigator(h, navFailed, nil)
	if h.gen != nil {
		h.gen.Close()
	}
	s.logNavigatorEnd(changed, failed, phase)
	return next
}
func (s *Server) navigatorPort() int {
	s.navLifeMu.Lock()
	defer s.navLifeMu.Unlock()
	if s.navCurrent == nil {
		return 0
	}
	return s.navCurrent.port
}

// Reservation is recorded fully before its bind. No transport method is called
// under the serializer; a superseded acquisition cannot reserve again.
func (s *Server) reserveNavigatorPort(h navHandle, used map[int]bool) (int, string) {
	s.navLifeMu.Lock()
	defer s.navLifeMu.Unlock()
	if !s.ownsNavigatorLocked(h) {
		return 0, ""
	}
	ports, _, err := readNavPorts(h.scope.portsPath)
	if err != nil {
		return 0, "port history unavailable; remove or fix ui-ports"
	}
	for _, p := range ports {
		used[p.port] = true
	}
	port, err := drawNavPort(used)
	if err != nil {
		return 0, "drawing navigator port"
	}
	used[port] = true
	ports = append(ports, navPort{port, time.Now().Unix()})
	if len(ports) > navPortsKept {
		ports = ports[len(ports)-navPortsKept:]
	}
	var body strings.Builder
	for _, p := range ports {
		fmt.Fprintf(&body, "%d %d\n", p.port, p.at)
	}
	if h.scope.deps.write(h.scope.portsPath, []byte(body.String())) != nil {
		return 0, "recording navigator port"
	}
	return port, ""
}

func (s *Server) failNavigatorStart(h navHandle, raw net.Listener, g *navGeneration, phase string) navHandle {
	var auth *navAuthAdapter
	if g != nil {
		auth = g.auth
	}
	next, changed, failed := s.finishNavigator(h, navFailed, auth)
	if g != nil {
		g.Close()
	} else if raw != nil {
		raw.Close()
	}
	s.logNavigatorEnd(changed, failed, phase)
	return next
}

func (s *Server) startNavigator(h navHandle) (*navGeneration, navHandle) {
	if !s.cfg.UI.Navigator {
		next, _, failed := s.finishNavigator(h, navOff, nil)
		if failed {
			s.log.Print("navigator: could not record that it is off")
		}
		return nil, next
	}
	slots, api := s.slots, s.navAPI
	if slots == nil {
		return nil, s.failNavigatorStart(h, nil, nil, "missing navigator connection owner")
	}
	observer, build := s.navigatorObserver(), navBuild()
	used := make(map[int]bool, navPortsKept+navBindTries)
	for range navBindTries {
		port, phase := s.reserveNavigatorPort(h, used)
		if port == 0 {
			if phase == "" {
				return nil, h
			}
			return nil, s.failNavigatorStart(h, nil, nil, phase)
		}
		raw, err := h.scope.deps.bind(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			if raw != nil {
				raw.Close()
			}
			continue
		}
		if raw == nil {
			return nil, s.failNavigatorStart(h, nil, nil, "binding navigator")
		}
		endpoint, ok := raw.Addr().(*net.TCPAddr) // Exactly one startup observation, outside locks.
		if !ok || endpoint == nil || !endpoint.IP.IsLoopback() || endpoint.Port != port {
			return nil, s.failNavigatorStart(h, raw, nil, "navigator endpoint refused")
		}
		addr := *endpoint
		addr.IP = append(net.IP(nil), endpoint.IP...)
		if !s.ownsNavigator(h) {
			raw.Close()
			return nil, h
		}
		token, err := h.scope.deps.token()
		key, decodeErr := navauth.Decode32(token)
		if err != nil || decodeErr != nil {
			return nil, s.failNavigatorStart(h, raw, nil, "creating navigator authentication")
		}
		host, err := h.scope.deps.hostname()
		if err != nil {
			return nil, s.failNavigatorStart(h, raw, nil, "reading navigator host information")
		}
		auth, err := newNavAuthAdapter(key, navauth.Binding{Port: port, Build: build}, navAuthInfo{DataDir: h.scope.dataDir, Hostname: host})
		if err != nil {
			return nil, s.failNavigatorStart(h, raw, nil, "creating navigator authentication")
		}
		g := &navGeneration{owner: s, auth: auth, addr: addr, port: port, capped: slots.listener(raw)}
		g.handle = navHandle{id: h.id, scope: h.scope, gen: g}
		core := navHandlerCore(navGate{verifier: auth,
			hello: http.HandlerFunc(auth.Hello), login: http.HandlerFunc(auth.Login),
			info: http.HandlerFunc(auth.Info), revoke: http.HandlerFunc(auth.Revoke), api: api,
		}, build)
		refuse := connectionUnavailable()
		refusal := navHandlerCore(navGate{verifier: denyAll{}, hello: refuse, login: refuse, info: refuse, revoke: refuse, api: refuse, pages: refuse}, build)
		g.server = slots.server(core, refusal)
		g.server.Handler = navRecover(observer, g.server.Handler)
		s.navLifeMu.Lock()
		if !s.ownsNavigatorLocked(h) {
			s.navLifeMu.Unlock()
			g.Close()
			return nil, h
		}
		body := []byte(fmt.Sprintf("%s %d %d %d %s\n", navOn, h.scope.pid, h.scope.started, port, token))
		if h.scope.deps.write(h.scope.tokenPath, body) != nil {
			next, changed, failed := s.finishNavigatorLocked(h, navFailed, auth)
			s.navLifeMu.Unlock()
			g.Close()
			s.logNavigatorEnd(changed, failed, "writing navigator state")
			return nil, next
		}
		g.handle.id = &navIdentity{1}
		s.navOwner, s.navCurrent = g.handle.id, g
		s.mu.Lock()
		s.navPort, s.navToken = port, token
		s.mu.Unlock()
		s.navLifeMu.Unlock()
		return g, g.handle
	}
	return nil, s.failNavigatorStart(h, nil, nil, "binding navigator")
}

func (s *Server) serveNavigator(g *navGeneration) {
	serveNav(g.server, g)
	g.Close()
}
