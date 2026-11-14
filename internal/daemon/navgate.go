package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navroute"
	"github.com/orlenko/aiq/internal/transcript"
	"github.com/orlenko/aiq/internal/version"
)

// The navigator port has one outer diagnostic/recovery envelope around
// connection selection, then localOnly, the navigator headers, a
// refusal of any path that is not canonical, then the gate. The gate answers its few
// exempt routes itself and lets nothing else under /api/ through without a credential
// the verifier accepts. Paths outside /api/ go to the page handler, which never reaches
// an API handler.

// NavVerifier decides who may use the navigator's API.
type NavVerifier interface {
	// Credential says whether a presented bearer value is a live credential.
	Credential(bearer string) bool
	// Proof says whether r, a request to a route only aiq's own CLI may use, is accepted.
	Proof(r *http.Request) bool
}

// denyAll accepts nothing, so the navigator serves nothing behind the gate.
type denyAll struct{}

func (denyAll) Credential(string) bool   { return false }
func (denyAll) Proof(*http.Request) bool { return false }

// navBuildAssets are the navigator's embedded files, which the build string covers so a
// page can tell when it is served by another build.
var navBuildAssets = func() [][]byte {
	out := [][]byte{navShellTemplate, navBootstrapJS, navSessionJS, navCodecJS, navAuthCSS}
	for _, a := range navAdditionalAssets {
		out = append(out, a.body)
	}
	return out
}()

// navBuild is "<version>+<hash of the navigator's files>".
func navBuild() string {
	h := sha256.New()
	for _, a := range navBuildAssets {
		h.Write(a)
	}
	return version.Version + "+" + hex.EncodeToString(h.Sum(nil))[:12]
}

// navCSP is the navigator's content security policy, on every response.
const navCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; require-trusted-types-for 'script'; trusted-types 'none'"

// navHeaders sets the navigator's headers before anything answers, so refusals carry
// them too.
func navHeaders(build string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Origin-Agent-Cluster", "?1")
		h.Set("Content-Security-Policy", navCSP)
		h.Set("X-Aiq-Build", build)
		next.ServeHTTP(w, r)
	})
}

// canonicalPath refuses a path the mux would read differently from the gate: one with
// any escape, or one that cleaning would change. Go's ServeMux matches on the decoded
// path, so /api/na%76/x would otherwise reach a handler for /api/nav/x. No navigator
// route needs such a path.
func canonicalPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		target, _, _ := strings.Cut(r.RequestURI, "?")
		if r.URL.RawPath != "" || strings.Contains(target, "%") || strings.Contains(r.URL.EscapedPath(), "%") ||
			!strings.HasPrefix(p, "/") || (p != "/" && path.Clean(p) != p) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// navGate holds the exempt routes, the verifier and what lies behind it.
type navGate struct {
	verifier NavVerifier
	// hello and login need no credential; revoke and info need the CLI's proof.
	hello, login, revoke, info http.Handler
	// api is served for a request with a credential the verifier accepts.
	api http.Handler
	// pages serves every path outside /api/.
	pages http.Handler
}

// navRoute is an exempt route: a method and an exact path.
type navRoute struct{ method, path string }

func (g *navGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/api/") {
		g.pages.ServeHTTP(w, r)
		return
	}
	// A page on another site can make a GET, which localOnly lets through.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch (navRoute{r.Method, p}) {
	case navRoute{http.MethodGet, "/api/nav/hello"}:
		g.hello.ServeHTTP(w, r)
		return
	case navRoute{http.MethodPost, "/api/nav/login"}:
		g.login.ServeHTTP(w, r)
		return
	case navRoute{http.MethodPost, "/api/nav/revoke"}:
		g.proven(g.revoke, w, r)
		return
	case navRoute{http.MethodGet, "/api/nav/info"}:
		g.proven(g.info, w, r)
		return
	}
	if verifier, ok := g.verifier.(interface {
		AuthorizeRead(*http.Request) (*http.Request, bool)
	}); ok {
		owned, accepted := verifier.AuthorizeRead(r)
		if !accepted || owned == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		g.api.ServeHTTP(w, owned)
		return
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" || len(r.Header.Values("Authorization")) != 1 || !g.verifier.Credential(bearer) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	g.api.ServeHTTP(w, r)
}

func (g *navGate) proven(h http.Handler, w http.ResponseWriter, r *http.Request) {
	if !g.verifier.Proof(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.ServeHTTP(w, r)
}

// navEntityHeaders describe a body, so a 500 in place of that body must not carry them.
var navEntityHeaders = []string{
	"Content-Length", "Content-Type", "Content-Encoding", "Content-Disposition",
	"Content-Range", "Transfer-Encoding", "ETag", "Last-Modified",
}

// startedWriter records whether a response has begun.
type startedWriter struct {
	http.ResponseWriter
	started bool
}

func (w *startedWriter) WriteHeader(code int) {
	w.started = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *startedWriter) Write(b []byte) (int, error) {
	w.started = true
	return w.ResponseWriter.Write(b)
}

func (w *startedWriter) Flush() {
	w.started = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *startedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// navRecover owns the request state and one completion, even when an inner
// handler reduces its failure to an ordinary response. No raw request or panic
// data enters the observer. Secondary recovery failures abort without logging.
func navRecover(observer *navObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input, creator := ownNavInput(r)
		var completion *navCompletion
		defer func() {
			if creator {
				input.close()
			}
			if completion != nil {
				completion.close()
			}
		}()
		ctx, state := navroute.WithRequest(r.Context())
		completion = &navCompletion{}
		ctx = context.WithValue(ctx, navCompletionKey{}, completion)
		r = r.WithContext(ctx)
		sw := &startedWriter{ResponseWriter: w}
		// The first defer clears input before ending slot protection, after
		// recovery/observation, exact abort and secondary response failure.
		defer func() {
			value := recover()
			if value != nil && value != http.ErrAbortHandler {
				origin := transcript.CapturePanic(value)
				diagnostic.Note(ctx, origin)
			}
			key, origin := state.Snapshot()
			observeNavOrigin(observer, key, origin)
			if value == nil {
				return
			}
			if value == http.ErrAbortHandler || sw.started {
				panic(http.ErrAbortHandler)
			}
			navRecoveryResponse(w)
		}()
		next.ServeHTTP(sw, r)
	})
}

func observeNavOrigin(observer *navObserver, key string, origin diagnostic.Origin) {
	defer func() { recover() }()
	observer.Observe(key, origin)
}

func navRecoveryResponse(w http.ResponseWriter) {
	defer func() {
		if recover() != nil {
			panic(http.ErrAbortHandler)
		}
	}()
	headers := w.Header()
	for _, key := range navEntityHeaders {
		headers.Del(key)
	}
	w.WriteHeader(http.StatusInternalServerError)
}

// newNavHandler is the navigator port's whole handler. A nil verifier denies everything,
// so a wiring slip cannot skip the check; a nil handler answers 404.
func newNavHandler(g navGate, build string, observer *navObserver) http.Handler {
	return navRecover(observer, navHandlerCore(g, build))
}

// Production composes these cores inside connSlots before adding ONE envelope.
func navHandlerCore(g navGate, build string) http.Handler {
	if g.verifier == nil {
		g.verifier = denyAll{}
	}
	for _, h := range []*http.Handler{&g.hello, &g.login, &g.revoke, &g.info, &g.api, &g.pages} {
		if *h == nil {
			*h = http.NotFoundHandler()
		}
	}
	for _, entry := range []struct {
		route   navroute.Route
		handler *http.Handler
	}{{navroute.Hello, &g.hello}, {navroute.Login, &g.login}, {navroute.Revoke, &g.revoke}, {navroute.Info, &g.info}} {
		mux := navroute.NewMux()
		if err := mux.Handle(entry.route, *entry.handler); err != nil {
			panic(err) // code-owned descriptors; never request input
		}
		*entry.handler = mux
	}
	return localOnly(navHeaders(build, canonicalPath(&g)))
}

// navHello tells aiq which navigator protocol and build answer on this port.
func navHello(build string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"nav_version": navProtocol, "build": build})
	})
}

// navLoginClosed refuses every login: nothing issues credentials yet.
func navLoginClosed() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
}

// MountNavigator sets what the navigator serves behind its gate, under /api/nav/. Call
// it before Run.
func (s *Server) MountNavigator(h http.Handler) { s.navAPI = h }

// MountNavigatorPages configures the static page dependency before Run. APIs
// retain their own credential gate; refused connections never invoke pages.
func (s *Server) MountNavigatorPages(h http.Handler) { s.navPages = h }

// navigatorHandler is what the navigator's port serves.
func (s *Server) navigatorHandler() http.Handler {
	return navRecover(s.navigatorObserver(), s.navigatorCore())
}
func (s *Server) navigatorCore() http.Handler {
	build := navBuild()
	return navHandlerCore(navGate{
		verifier: s.navVerifier,
		hello:    navHello(build),
		login:    navLoginClosed(),
		api:      s.navAPI,
		pages:    s.navPages,
	}, build)
}

// navigatorRefusalCore keeps the navigator guards while
// refusing retired connections without invoking an application route.
func (s *Server) navigatorRefusalCore() http.Handler {
	refuse := connectionUnavailable()
	return navHandlerCore(navGate{
		verifier: s.navVerifier,
		hello:    refuse, login: refuse, revoke: refuse, info: refuse,
		api: refuse, pages: refuse,
	}, navBuild())
}

// The one process owner survives server reconstruction and listener retirement.
var processNavObservations navObserverOwner

func (s *Server) navigatorObserver() *navObserver {
	if s.navObservations != nil {
		return s.navObservations
	}
	return processNavObservations.acquire(navroute.Catalog(), s.log.Printf, nil, nil)
}

// CloseNavigatorDiagnostics is for final CLI process shutdown, never a Run or
// listener lifetime. False means cancellation prevented proving join/flush.
func CloseNavigatorDiagnostics(ctx context.Context) bool {
	return processNavObservations.Close(ctx)
}

func (s *Server) navigatorServer() *http.Server {
	observer := s.navigatorObserver()
	server := s.slots.server(s.navigatorCore(), s.navigatorRefusalCore())
	server.Handler = navRecover(observer, server.Handler)
	return server
}
