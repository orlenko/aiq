package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func statePath(selection string) string {
	return "/api/nav/sessions/claude/" + fullIDFixtureID + "/state?selection=" + selection
}

func TestNavStateOnlyObservesSelectedCachedParse(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented reply")
	var roots atomic.Int32
	in := x.input(&roots)
	now := x.at.Add(time.Hour)
	in.now = func() time.Time { return now }
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Even the first valid state read cannot enumerate roots or initialize data.
	w := factoryGet(t, f, statePath(strings.Repeat("A", 43)))
	if w.Code != 200 || roots.Load() != 0 || !strings.Contains(w.Body.String(), `"match":false`) {
		t.Fatal("state initialized data", w.Code, w.Body.String())
	}
	detail := selectionDetail(t, f, fullIDFixtureID)
	b, _ := f.get(context.Background())
	record, _ := b.selections.lookup("claude", fullIDFixtureID, detail.Selection)
	before := f.cache.Stats()
	// Crossing the discovery TTL must not refresh roots on this endpoint.
	now = now.Add(2 * transcript.DiscoveryTTL)
	get := func() navStateReady {
		t.Helper()
		w := factoryGet(t, f, statePath(detail.Selection))
		var out navStateReady
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !out.Match || !out.Cached || out.CachedTurns != 1 || out.Live != "unknown" || out.WorkingKnown {
			t.Fatal("cached state failed", w.Code, w.Body.String())
		}
		for _, forbidden := range []string{x.root, "Invented reply", "pane", "account", "workspace", "pid"} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatal("state disclosed detail metadata")
			}
		}
		return out
	}
	if get().DiskChanged {
		t.Fatal("unchanged disk marked changed")
	}
	file, err := os.OpenFile(record.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n{\"type\":\"invented-unparsed\"}\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if !get().DiskChanged {
		t.Fatal("append was not observed")
	}
	if roots.Load() != 1 || f.cache.Stats() != before {
		t.Fatal("state performed discovery, parsing, or view work")
	}
	if err := os.Remove(record.path); err != nil {
		t.Fatal(err)
	}
	w = factoryGet(t, f, statePath(detail.Selection))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"match":false`) || strings.Contains(w.Body.String(), "cached_turns") {
		t.Fatal("missing selected file fabricated state", w.Body.String())
	}
}

func TestNavStateCodecRefusesAliasesAndExtraParameters(t *testing.T) {
	good := statePath(strings.Repeat("A", 43))
	for _, suffix := range []string{"&selection=" + strings.Repeat("A", 43), "&last=50", "=", "&", "%20"} {
		if _, ok := navStateDecode(httptest.NewRequest("GET", good+suffix, nil)); ok {
			t.Fatal("accepted ambiguous state query")
		}
	}
	for _, path := range []string{strings.Replace(good, "/state?", "/state/?", 1), strings.Replace(good, "selection=", "Selection=", 1), strings.Replace(good, "/claude/", "/other/", 1)} {
		if _, ok := navStateDecode(httptest.NewRequest("GET", path, nil)); ok {
			t.Fatal("accepted invalid state path")
		}
	}
	if _, ok := navStateDecode(httptest.NewRequest("GET", good, nil)); !ok {
		t.Fatal("literal query refused")
	}
	if _, ok := navStateDecode(httptest.NewRequest("POST", good, nil)); ok {
		t.Fatal("accepted state write")
	}
}

func TestNavStatePrecommitRetirementAndSlotLifetime(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	detail := selectionDetail(t, f, fullIDFixtureID)
	w := &detailTestWriter{onHeader: func() {
		f.mu.Lock()
		f.bundle = nil
		f.mu.Unlock()
	}, onWrite: func() {
		if len(f.detailSlots) != 1 {
			t.Fatal("state released shared admission before body write")
		}
	}}
	f.api.ServeHTTP(w, httptest.NewRequest("GET", statePath(detail.Selection), nil))
	if w.status != 200 || !strings.Contains(string(w.body), `"match":false`) || strings.Contains(string(w.body), "cached_turns") || len(f.detailSlots) != 0 {
		t.Fatal("retired bundle published state or leaked slot")
	}
}

func TestNavStateCancellationAndMetadataAdmission(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	in := x.input(nil)
	var oversized atomic.Bool
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		if oversized.Load() {
			return nil, state.ErrNavigatorSnapshotTooLarge
		}
		return nil, state.ErrNavigatorSnapshotUnavailable
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	detail := selectionDetail(t, f, fullIDFixtureID)
	w := factoryGet(t, f, statePath(detail.Selection))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"live":"unknown"`) {
		t.Fatal("unavailable metadata claimed ended")
	}
	oversized.Store(true)
	w = factoryGet(t, f, statePath(detail.Selection))
	if w.Code != 503 || strings.Contains(w.Body.String(), "cached_turns") {
		t.Fatal("oversized metadata became partial response")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	f.api.ServeHTTP(w, httptest.NewRequest("GET", statePath(detail.Selection), nil).WithContext(ctx))
	if w.Code != 503 || len(f.detailSlots) != 0 {
		t.Fatal("state ignored cancellation")
	}
}
