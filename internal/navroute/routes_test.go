package navroute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }
func routeOrigin(fn func()) (out diagnostic.Origin) {
	defer func() {
		if v := recover(); v != nil {
			out = diagnostic.Capture(v)
		}
	}()
	fn()
	return
}
func routeLeaf() { panic("invented route leaf") }

func TestRouteOwnDispatchAndDerivedRequestState(t *testing.T) {
	a := routeOrigin(routeLeaf)
	if _, _, known := a.Location(); !a.Present() || known {
		t.Fatal("unallowlisted helper leaf did not remain present-unknown")
	}
	ctx, state := WithRequest(context.Background())
	mux := NewMux()
	calls := 0
	if err := mux.Handle(Conversation, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		key, _ := state.Snapshot()
		if key != "GET /api/nav/sessions/{provider}/{id}" {
			t.Fatal("registration not noted before entry")
		}
		r.Pattern = "invented request-controlled pattern"
		diagnostic.Note(r.WithContext(context.WithValue(r.Context(), struct{}{}, true)).Context(), a)
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/nav/sessions/invented/invented?token=invented", nil)
	r.Pattern = "POST /api/nav/login"
	r = r.Clone(context.WithValue(ctx, struct{}{}, "derived"))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	key, got := state.Snapshot()
	if calls != 1 || w.Code != 204 || key != "GET /api/nav/sessions/{provider}/{id}" || got != a {
		t.Fatal("real dispatch/clone did not preserve trusted key and origin")
	}
	catalog := Catalog()
	if len(catalog) != 10 || catalog[0] != "unregistered" || catalog[1] != "GET /api/nav/hello" || catalog[6] != "GET /api/nav/sessions/{provider}/{id}" || catalog[7] != "GET /api/nav/asks" || catalog[8] != "GET /api/nav/asks/{id}/turn" || catalog[9] != "GET /api/nav/directories" {
		t.Fatal("closed catalog changed")
	}
	catalog[1] = "invented mutable catalog input"
	if Catalog()[1] != "GET /api/nav/hello" {
		t.Fatal("exported catalog aliases internal storage")
	}
}

func TestRouteNestedDispatchRetainsLeafAndFirstUnknown(t *testing.T) {
	unknown := routeOrigin(func() { strings.Repeat("invented", -1) })
	if _, _, known := unknown.Location(); !unknown.Present() || known {
		t.Fatal("external origin not unknown")
	}
	a := diagnostic.Capture(nil) // a distinct closed unknown fixture
	if a == unknown {
		t.Fatal("first-origin fixtures are indistinguishable")
	}
	ctx, state := WithRequest(context.Background())
	outer, inner := NewMux(), NewMux()
	if err := inner.Handle(Conversation, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		diagnostic.Note(r.Context(), a)
		w.WriteHeader(http.StatusNoContent)
	})); err != nil {
		t.Fatal(err)
	}
	if err := outer.Handle(Hello, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		diagnostic.Note(r.Context(), unknown)
		// Both muxes dispatch themselves. No broad production descriptor or
		// standalone verified wrapper is created just for this fixture.
		child := r.Clone(context.WithValue(r.Context(), struct{}{}, true))
		child.URL.Path = "/api/nav/sessions/invented/fixture"
		inner.ServeHTTP(w, child)
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	outer.ServeHTTP(w, httptest.NewRequest("GET", "/api/nav/hello", nil).WithContext(ctx))
	key, origin := state.Snapshot()
	if w.Code != 204 || key != "GET /api/nav/sessions/{provider}/{id}" || origin != unknown {
		t.Fatal("outer unwind replaced leaf or first unknown")
	}
}

func TestRouteOpaqueSpoofUnknownAndMissingContextStayFallback(t *testing.T) {
	a := routeOrigin(routeLeaf)
	mux := NewMux()
	calls := 0
	if err := mux.Handle(Hello, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; diagnostic.Note(r.Context(), a) })); err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		ctx, state := WithRequest(context.Background())
		r := httptest.NewRequest("GET", "/unknown/"+strconv.Itoa(i)+"?token=invented", nil).WithContext(ctx)
		r.Pattern = "GET /api/nav/hello"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		key, origin := state.Snapshot()
		if w.Code != 404 || key != "unregistered" || origin.Present() {
			t.Fatal("unknown request acquired registered identity")
		}
		// Opaque code can set Pattern and Note an origin, but cannot set the
		// private registration state without actual owned mux dispatch.
		opaque := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			r.Pattern = "GET /api/nav/hello"
			diagnostic.Note(r.Context(), a)
		})
		opaque.ServeHTTP(w, r)
		key, origin = state.Snapshot()
		if key != "unregistered" || origin != a {
			t.Fatal("opaque Pattern spoof became trusted")
		}
	}
	ctx, state := WithRequest(context.Background())
	_ = ctx
	r := httptest.NewRequest("GET", "/api/nav/hello", nil).WithContext(context.Background())
	mux.ServeHTTP(httptest.NewRecorder(), r)
	key, origin := state.Snapshot()
	if calls != 1 || key != "unregistered" || origin.Present() {
		t.Fatal("replaced Background context acquired outer state")
	}
	if key, origin := (*RequestState)(nil).Snapshot(); key != "unregistered" || origin.Present() {
		t.Fatal("absent state fabricated metadata")
	}
}

func TestRouteRegistrationValidationAndNoImplicitMount(t *testing.T) {
	mux := NewMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, route := range []Route{Unregistered, Route(255)} {
		if err := mux.Handle(route, handler); err == nil {
			t.Fatal("invalid descriptor registered")
		}
	}
	if err := mux.Handle(Hello, nil); err == nil {
		t.Fatal("nil handler registered")
	}
	if err := mux.Handle(Hello, handler); err != nil {
		t.Fatal(err)
	}
	if err := mux.Handle(Hello, handler); err == nil {
		t.Fatal("duplicate descriptor accepted")
	}
	ctx, state := WithRequest(context.Background())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/nav/sessions", nil).WithContext(ctx))
	if key, origin := state.Snapshot(); w.Code != 404 || key != "unregistered" || origin.Present() {
		t.Fatal("catalog inclusion mounted dormant data")
	}
	if Route(255).Key() != "unregistered" {
		t.Fatal("unknown descriptor escaped fallback")
	}
	// The helper supplies registration identity, not method/proof exemptions.
	// This fixture keeps an explicit gate in front, as production must do.
	gate := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/nav/hello" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
	for _, method := range []string{"HEAD", "POST"} {
		ctx, state := WithRequest(context.Background())
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(method, "/api/nav/hello", nil).WithContext(ctx))
		if key, _ := state.Snapshot(); w.Code != 403 || key != "unregistered" {
			t.Fatal("mux broadened upstream gate")
		}
	}
}
