package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/navroute"
)

func navRouteBridgeLeaf() { panic("invented navigator route bridge fault") }

func TestNavRouteKnownOriginThroughOwnedDispatch(t *testing.T) {
	ctx, state := navroute.WithRequest(context.Background())
	mux := navroute.NewMux()
	if err := mux.Handle(navroute.Conversation, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				diagnostic.Note(r.Context(), diagnostic.Capture(value))
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		navRouteBridgeLeaf()
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/nav/sessions/invented/fixture?private=invented", nil).WithContext(ctx))
	key, origin := state.Snapshot()
	file, line, known := origin.Location()
	if w.Code != 500 || key != "GET /api/nav/sessions/{provider}/{id}" || !known || file != "internal/daemon/nav_route_bridge_test.go" || line != 13 {
		t.Fatal("owned dispatch did not retain original allowlisted leaf and closed key")
	}
}
