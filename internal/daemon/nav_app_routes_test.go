package daemon

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
)

func TestNavigatorAppRouteVectors(t *testing.T) {
	raw, err := os.ReadFile("ui/app_route_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Route string
		OK    bool
	}
	if json.Unmarshal(raw, &rows) != nil || len(rows) != 12 {
		t.Fatal("app route vector fixture incomplete")
	}
	for _, row := range rows {
		u, err := url.Parse(row.Route)
		got := err == nil && navPageRoute(u)
		if got != row.OK {
			t.Error("app route admission differs", row.Route)
		}
	}
}
