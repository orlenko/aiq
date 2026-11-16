package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

func actionFactory(t *testing.T, count int) (*navFactory, *navAppFixture, *atomic.Int32) {
	t.Helper()
	x := newNavAppFixture(t, count, navAppFixtureQuote)
	calls := new(atomic.Int32)
	f, err := newNavFactory(x.input(calls), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return f, x, calls
}
func actionRequest(t *testing.T, f *navFactory, id, revision, action string) *httptest.ResponseRecorder {
	t.Helper()
	out := httptest.NewRecorder()
	f.api.ServeHTTP(out, httptest.NewRequest("POST", "/api/nav/asks/action?action="+action+"&id="+id+"&revision="+revision, nil))
	return out
}
func TestNavigatorAskActionRevisionAndHistory(t *testing.T) {
	f, _, calls := actionFactory(t, 1)
	read := factoryGet(t, f, "/api/nav/asks")
	var data struct {
		Sections []struct {
			Items []navAsksItem `json:"items"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	var item navAsksItem
	for _, section := range data.Sections {
		if len(section.Items) > 0 {
			item = section.Items[0]
		}
	}
	if len(item.Revision) != 64 {
		t.Fatal("missing revision")
	}
	before := calls.Load()
	dismiss := actionRequest(t, f, item.ID, item.Revision, "dismiss")
	if dismiss.Code != 200 {
		t.Fatal("dismiss", dismiss.Code, dismiss.Body.String())
	}
	var result struct{ ID, State, Revision string }
	if json.Unmarshal(dismiss.Body.Bytes(), &result) != nil || result.State != "dismissed" || result.Revision == item.Revision {
		t.Fatal("wrong mutation result")
	}
	if actionRequest(t, f, item.ID, item.Revision, "dismiss").Code != 409 {
		t.Fatal("stale action accepted")
	}
	if actionRequest(t, f, item.ID, result.Revision, "restore").Code != 200 {
		t.Fatal("restore failed")
	}
	if calls.Load() != before {
		t.Fatal("mutation started transcript discovery")
	}
}
func TestNavigatorAskActionDoesNotCreateOrMigrateRegistry(t *testing.T) {
	f, x, _ := actionFactory(t, -1)
	revision := strings.Repeat("a", 64)
	if out := actionRequest(t, f, "1", revision, "dismiss"); out.Code != 503 {
		t.Fatal("missing registry", out.Code)
	}
	if _, err := os.Stat(x.asksPath); !os.IsNotExist(err) {
		t.Fatal("missing registry created", err)
	}
	store, err := state.Open(x.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if out := actionRequest(t, f, "1", revision, "dismiss"); out.Code != 503 {
		t.Fatal("missing schema", out.Code)
	}
	ro, err := state.OpenReadOnly(x.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.GetAsk(1); err == nil {
		t.Fatal("unexpected asks schema")
	}
}
func TestNavigatorAskActionRejectsBodyAndAdmissionPressure(t *testing.T) {
	f, _, _ := actionFactory(t, 1)
	target := "/api/nav/asks/action?action=dismiss&id=1&revision=" + strings.Repeat("a", 64)
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader("x"))
	out := httptest.NewRecorder()
	f.api.ServeHTTP(out, r)
	if out.Code != 400 {
		t.Fatal("body accepted", out.Code)
	}
	f.askSlots <- struct{}{}
	out = actionRequest(t, f, "1", strings.Repeat("a", 64), "dismiss")
	<-f.askSlots
	if out.Code != 503 {
		t.Fatal("physical slot bypassed", out.Code)
	}
}
