package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/orlenko/aiq/internal/version"
)

// The navigator's port answers through one chain: a recover, localOnly, the navigator's headers, a
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
var navBuildAssets [][]byte

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

// navPanicPathMax caps the path in the log line for a panic.
const navPanicPathMax = 128

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

// navRecover answers 500 when a handler panics before it has written anything. Left to
// the server, a panic drops the connection, which a page cannot tell from a daemon that
// has gone. A panic after the response has begun aborts the connection, so the client
// sees a failed read and not a whole-looking answer cut short. The log names the
// panic's type and not its value, which could carry what a handler was serving.
func navRecover(logf func(string, ...any), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &startedWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			p := r.URL.Path
			if len(p) > navPanicPathMax {
				p = p[:navPanicPathMax]
			}
			logf("navigator: a handler panicked on %q: %T", p, v)
			if sw.started {
				panic(http.ErrAbortHandler)
			}
			for _, k := range navEntityHeaders {
				w.Header().Del(k)
			}
			w.WriteHeader(http.StatusInternalServerError)
		}()
		next.ServeHTTP(sw, r)
	})
}

// newNavHandler is the navigator port's whole handler. A nil verifier denies everything,
// so a wiring slip cannot skip the check; a nil handler answers 404.
func newNavHandler(g navGate, build string, logf func(string, ...any)) http.Handler {
	if g.verifier == nil {
		g.verifier = denyAll{}
	}
	for _, h := range []*http.Handler{&g.hello, &g.login, &g.revoke, &g.info, &g.api, &g.pages} {
		if *h == nil {
			*h = http.NotFoundHandler()
		}
	}
	return navRecover(logf, localOnly(navHeaders(build, canonicalPath(&g))))
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

// navigatorHandler is what the navigator's port serves.
func (s *Server) navigatorHandler() http.Handler {
	build := navBuild()
	return newNavHandler(navGate{
		verifier: s.navVerifier,
		hello:    navHello(build),
		login:    navLoginClosed(),
		api:      s.navAPI,
	}, build, s.log.Printf)
}

// navigatorRefusal keeps the navigator's guards and response envelope while
// refusing retired connections without invoking an application route.
func (s *Server) navigatorRefusal() http.Handler {
	refuse := connectionUnavailable()
	return newNavHandler(navGate{
		verifier: s.navVerifier,
		hello:    refuse, login: refuse, revoke: refuse, info: refuse,
		api: refuse, pages: refuse,
	}, navBuild(), s.log.Printf)
}
