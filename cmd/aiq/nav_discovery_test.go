package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func TestNavDiscoveryNewRootReplacesSelectedCopyOnNextPoll(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented initial copy")
	now := x.at.Add(time.Hour)
	in := x.input(nil)
	in.now = func() time.Time { return now }
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	first := selectionDetail(t, f, fullIDFixtureID)
	bundle, _ := f.get(context.Background())
	record, _ := bundle.selections.lookup("claude", fullIDFixtureID, first.Selection)
	data, err := os.ReadFile(record.path)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(x.root, "new-invented-account", "projects")
	newer := filepath.Join(root, filepath.Base(filepath.Dir(record.path)), filepath.Base(record.path))
	if err = os.MkdirAll(filepath.Dir(newer), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(newer, []byte(strings.ReplaceAll(string(data), "Invented initial copy", "Invented newer copy")), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err = os.Chtimes(newer, later, later); err != nil {
		t.Fatal(err)
	}
	x.roots.ClaudeProjects = append(x.roots.ClaudeProjects, root)
	now = now.Add(navRootsTTL)
	second := selectionDetail(t, f, fullIDFixtureID)
	if second.Selection == first.Selection {
		t.Fatal("new overlay root did not replace selection within one poll")
	}
	current, _ := f.get(context.Background())
	selected, ok := current.selections.lookup("claude", fullIDFixtureID, second.Selection)
	if !ok || selected.path != newer {
		t.Fatal("normal discovery did not select newer copy")
	}
	w := factoryGet(t, f, statePath(first.Selection))
	if !strings.Contains(w.Body.String(), `"match":false`) {
		t.Fatal("old retained view inherited new-root facts")
	}
}

func TestNavSettledDelta304SkipsEncodingButBindsCutoffAndAuxiliaryFacts(t *testing.T) {
	h, _, full := deltaFixture(t)
	path := "/api/nav/sessions/claude/" + detailTestID + "?from_key=" + full.envelope.TailKey + "&from_turn=1&last=50&markup=1"
	first := detailTestServe(h, httptest.NewRequest("GET", path, nil))
	detailTestOK(t, first)
	h.deps.marshal = func(any) ([]byte, error) { panic("settled conditional must not encode") }
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("If-None-Match", first.Header().Get("ETag"))
	if w := detailTestServe(h, r); w.status != 304 || w.writes != 0 {
		t.Fatal("settled delta was encoded instead of stat/cache304")
	}
	wrong := httptest.NewRequest("GET", strings.Replace(path, "from_turn=1", "from_turn=0", 1), nil)
	wrong.Header.Set("If-None-Match", first.Header().Get("ETag"))
	if w := detailTestServe(h, wrong); w.status == 304 {
		t.Fatal("delta ETag crossed a different cutoff")
	}
	// An unknown auxiliary state invalidates the settled proof even with the
	// same transcript stat and the same browser conditional header.
	h.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) {
		return navDetailFacts{}, nil
	}
	if w := detailTestServe(h, r); w.status == 304 {
		t.Fatal("unknown auxiliary facts reused settled304")
	}
}
