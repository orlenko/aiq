// Package navroute binds navigator diagnostics to code-owned registrations.
// Descriptor inclusion does not mount a handler or change a gate decision.
package navroute

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/orlenko/aiq/internal/diagnostic"
)

type Route uint8

const (
	Unregistered Route = iota
	Hello
	Login
	Revoke
	Info
	Sessions
	Conversation
	Asks
	AskTurn
	Directories
	ConversationState
)

var patterns = [...]string{
	Unregistered:      "unregistered",
	Hello:             "GET /api/nav/hello",
	Login:             "POST /api/nav/login",
	Revoke:            "POST /api/nav/revoke",
	Info:              "GET /api/nav/info",
	Sessions:          "GET /api/nav/sessions",
	Conversation:      "GET /api/nav/sessions/{provider}/{id}",
	Asks:              "GET /api/nav/asks",
	AskTurn:           "GET /api/nav/asks/{id}/turn",
	Directories:       "GET /api/nav/directories",
	ConversationState: "GET /api/nav/sessions/{provider}/{id}/state",
}

func (route Route) Key() string {
	if int(route) >= len(patterns) {
		return patterns[Unregistered]
	}
	return patterns[route]
}
func Catalog() []string { return append([]string(nil), patterns[:]...) }

type stateKey struct{}

// RequestState retains only the first Origin and last trusted registration.
// It has no back-pointer to a request or its context.
type RequestState struct {
	mu     sync.Mutex
	route  Route
	origin *diagnostic.Holder
}

func WithRequest(parent context.Context) (context.Context, *RequestState) {
	ctx, origin := diagnostic.WithHolder(parent)
	s := &RequestState{origin: origin}
	return context.WithValue(ctx, stateKey{}, s), s
}
func (s *RequestState) Snapshot() (string, diagnostic.Origin) {
	if s == nil {
		return Unregistered.Key(), diagnostic.Origin{}
	}
	s.mu.Lock()
	route := s.route
	s.mu.Unlock()
	if s.origin == nil {
		return route.Key(), diagnostic.Origin{}
	}
	return route.Key(), s.origin.Origin()
}
func registered(ctx context.Context, route Route) {
	if s, ok := ctx.Value(stateKey{}).(*RequestState); ok && s != nil {
		s.mu.Lock()
		s.route = route
		s.mu.Unlock()
	}
}

var errRegistration = errors.New("invalid navigator registration")

// Mux owns its dispatch wrappers. It exposes neither a trust setter nor a
// reusable verified-handler wrapper. Configure it before serving.
type Mux struct {
	mux        *http.ServeMux
	registered map[Route]bool
}

func NewMux() *Mux { return &Mux{mux: http.NewServeMux(), registered: map[Route]bool{}} }
func (m *Mux) Handle(route Route, handler http.Handler) error {
	if route == Unregistered || int(route) >= len(patterns) || handler == nil || m.registered[route] {
		return errRegistration
	}
	pattern := patterns[route]
	m.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Pattern == pattern {
			registered(r.Context(), route)
		}
		handler.ServeHTTP(w, r)
	}))
	m.registered[route] = true
	return nil
}
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }
