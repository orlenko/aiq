package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/navauth"
	"github.com/orlenko/aiq/internal/transcript"
)

func selectionDetail(t *testing.T, f *navFactory, id string) navDetailEnvelope {
	t.Helper()
	w := factoryGet(t, f, "/api/nav/sessions/claude/"+id+"?last=50&markup=0")
	var body navDetailEnvelope
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("detail failed: %d %s", w.Code, w.Body.String())
	}
	if _, err := navauth.Decode32(body.Selection); err != nil {
		t.Fatal("detail lacks an opaque selection")
	}
	return body
}
func TestNavSelectionNewerCopyCannotRebindAnotherRetainedView(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	second := filepath.Join(x.root, "other-claude", "projects")
	if err := os.MkdirAll(second, 0700); err != nil {
		t.Fatal(err)
	}
	x.roots.ClaudeProjects = append(x.roots.ClaudeProjects, second)
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := f.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Direct discovery models the next expired Index lookup without sleeping
	// for the production discovery TTL. Copy choice still uses FindByID.
	bundle.detail.(*navDetailHandler).deps.resolve = func(ctx context.Context, provider, id string) (transcript.Hit, error) {
		return transcript.FindByID(ctx, transcript.DirectWalk(x.roots), provider, id, f.cache.Summary)
	}
	first := selectionDetail(t, f, fullIDFixtureID)
	b, err := f.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, ok := b.selections.lookup("claude", fullIDFixtureID, first.Selection)
	if !ok || !b.selections.current(record) {
		t.Fatal("original selection missing")
	}
	if again := selectionDetail(t, f, fullIDFixtureID); again.Selection != first.Selection {
		t.Fatal("unchanged parse changed token")
	}
	data, err := os.ReadFile(record.path)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(second, filepath.Base(filepath.Dir(record.path)), filepath.Base(record.path))
	if err := os.MkdirAll(filepath.Dir(other), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, data, 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(other, later, later); err != nil {
		t.Fatal(err)
	}
	newer := selectionDetail(t, f, fullIDFixtureID)
	if newer.Selection == first.Selection {
		t.Fatal("newer transcript copy reused token")
	}
	if _, ok := b.selections.lookup("claude", fullIDFixtureID, first.Selection); ok || b.selections.current(record) {
		t.Fatal("old tab silently rebound to newer copy")
	}
	current, ok := b.selections.lookup("claude", fullIDFixtureID, newer.Selection)
	if !ok || current.path != other || !b.selections.current(current) {
		t.Fatal("new copy not independently bound")
	}
	if _, ok := b.selections.lookup("codex", fullIDFixtureID, newer.Selection); ok {
		t.Fatal("token crossed provider scope")
	}
}
func TestNavSelectionEvictionReAdmissionAndRootScope(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	in := x.input(nil)
	now := x.at.Add(time.Hour)
	in.now = func() time.Time { return now }
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	first := selectionDetail(t, f, fullIDFixtureID)
	b, err := f.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	record, ok := b.selections.lookup("claude", fullIDFixtureID, first.Selection)
	if !ok {
		t.Fatal("missing initial record")
	}
	// Populate the actual full-parse cache past its32 entries, without giving
	// the selection registry new authority for the invented files.
	for i := 1; i <= 33; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
		file := filepath.Join(x.root, "other-cache", id+".jsonl")
		goldenWrite(t, file, fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2031-04-05T12:00:00Z","message":{"role":"user","content":"Invented cache entry"}}`, id, x.work))
		if _, _, err := f.cache.Session(context.Background(), file); err != nil {
			t.Fatal(err)
		}
	}
	if b.selections.current(record) {
		t.Fatal("evicted parse still current")
	}
	second := selectionDetail(t, f, fullIDFixtureID)
	if second.Selection == first.Selection {
		t.Fatal("equal-stat re-admission reused token")
	}
	if _, ok := b.selections.lookup("claude", fullIDFixtureID, first.Selection); ok {
		t.Fatal("old incarnation still matches")
	}
	// An unchanged root bundle preserves correlation; a changed root set does not.
	now = now.Add(transcript.DiscoveryTTL)
	same, err := f.get(context.Background())
	if err != nil || same != b || same.selections != b.selections {
		t.Fatal("unchanged roots discarded registry")
	}
	x.roots.Labels = map[string]string{x.roots.ClaudeProjects[0]: "invented-new-label"}
	now = now.Add(transcript.DiscoveryTTL)
	changed, err := f.get(context.Background())
	if err != nil || changed == b || changed.selections == b.selections {
		t.Fatal("changed roots retained registry")
	}
	if _, ok := changed.selections.lookup("claude", fullIDFixtureID, second.Selection); ok {
		t.Fatal("selection crossed changed roots")
	}
}
func TestNavSelectionRegistryBoundsAndEntropyFailure(t *testing.T) {
	cache := transcript.NewCache()
	if _, err := newNavSelections(cache, bytes.NewReader(nil)); err == nil {
		t.Fatal("entropy failure yielded registry")
	}
	r, err := newNavSelections(cache, bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	var firstToken, firstID string
	for i := 1; i <= 33; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
		path := filepath.Join(root, id+".jsonl")
		goldenWrite(t, path, fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/invented/work","message":{"role":"user","content":"Invented question"}}`, id))
		s, snap, err := cache.Session(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		token, err := r.register(context.Background(), transcript.Hit{Path: path}, s, snap, "")
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			firstToken, firstID = token, id
		}
		if strings.Contains(token, root) || len(token) != 43 {
			t.Fatal("token disclosed a path or changed format")
		}
	}
	if len(r.rows) != 32 || r.lru.Len() != 32 {
		t.Fatal("unbounded registry")
	}
	if _, ok := r.lookup("claude", firstID, firstToken); ok {
		t.Fatal("oldest registration not evicted")
	}
	for _, token := range []string{"", firstToken + "=", url.QueryEscape(firstToken) + " "} {
		if _, ok := r.lookup("claude", firstID, token); ok {
			t.Fatal("malformed token matched")
		}
	}
}
