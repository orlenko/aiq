package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

const navAppFixtureQuote = "Which invented option should be used?"

type navAppFixture struct {
	root, work, asksPath string
	roots                transcript.Roots
	at                   time.Time
}

func newNavAppFixture(t *testing.T, count int, quote string) *navAppFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	f := &navAppFixture{root: root, work: filepath.Join(root, "invented-work"), asksPath: filepath.Join(root, "data", "asks.db"), at: time.Date(2031, 4, 5, 12, 0, 0, 123000000, time.UTC)}
	if err = os.Mkdir(f.work, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Dir(f.asksPath), 0700); err != nil {
		t.Fatal(err)
	}
	f.roots = transcript.Roots{ClaudeProjects: []string{filepath.Join(root, "real-claude", "projects")}, CodexSessions: []string{filepath.Join(root, "real-codex", "sessions")}, CodexIndex: []string{filepath.Join(root, "real-codex", "session_index.jsonl")}}
	user, _ := json.Marshal(map[string]any{"type": "user", "sessionId": fullIDFixtureID, "cwd": f.work, "timestamp": f.at.Format(time.RFC3339Nano), "origin": map[string]string{"kind": "human"}, "promptSource": "typed", "message": map[string]string{"role": "user", "content": "Invented project request"}})
	answer, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": fullIDFixtureID, "cwd": f.work, "timestamp": f.at.Add(time.Second).Format(time.RFC3339Nano), "message": map[string]any{"id": "invented-message", "model": "invented-model", "role": "assistant", "stop_reason": "end_turn", "content": []map[string]string{{"type": "text", "text": quote}}}})
	meta, _ := json.Marshal(map[string]any{"type": "user", "isMeta": true, "timestamp": f.at.Add(2 * time.Second).Format(time.RFC3339Nano), "message": map[string]string{"role": "user", "content": "Invented bookkeeping"}})
	goldenWrite(t, filepath.Join(f.roots.ClaudeProjects[0], claudeNonAlnum.ReplaceAllString(f.work, "-"), fullIDFixtureID+".jsonl"), string(user), string(answer), string(meta))
	if count < 0 {
		return f
	}
	store, err := state.Open(f.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.EnsureAsks(); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i <= count; i++ {
		if _, err = tx.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats,asks_reader,risk)
	 VALUES(?,?,?,'claude',?,?,'open',?,?,?,?, '[]',1,1,0)`, i, fmt.Sprintf("invented-chain-%d", i), f.work, fullIDFixtureID, f.at.Unix()+1, f.at.Unix()+1, quote, quote, f.at.Unix()+1); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *navAppFixture) input(calls *atomic.Int32) navFactoryInput {
	return navFactoryInput{roots: func(ctx context.Context) (transcript.Roots, error) {
		if calls != nil {
			calls.Add(1)
		}
		return f.roots, ctx.Err()
	}, asksPath: f.asksPath, now: func() time.Time { return f.at.Add(time.Hour) }, location: time.UTC}
}
func factoryGet(t *testing.T, f *navFactory, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.api.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestNavFactoryManualSnapshotAndAskPlacement(t *testing.T) {
	x := newNavAppFixture(t, 1, navAppFixtureQuote)
	var calls atomic.Int32
	f, err := newNavFactory(x.input(&calls), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("constructor enumerated roots")
	}
	if w := factoryGet(t, f, "/api/nav/unknown"); w.Code != 404 || calls.Load() != 0 {
		t.Fatal("unregistered request initialized data")
	}
	w := factoryGet(t, f, "/api/nav/sessions?dir="+url.QueryEscape(x.work)+"&workers=0")
	var list navSessionsBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Sessions) != 1 || list.Sessions[0].ID != fullIDFixtureID {
		t.Fatal("real list failed", w.Code, w.Body.String())
	}
	// Mutating the caller graph cannot repoint the already-created Index.
	x.roots.ClaudeProjects[0] = "/invented/unreachable"
	x.roots.Labels = map[string]string{"/invented": "mutated"}
	w = factoryGet(t, f, "/api/nav/sessions/claude/"+fullIDFixtureID+"?last=50&markup=0")
	var detail navDetailEnvelope
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || len(detail.Messages) == 0 || detail.Session.Cwd != x.work {
		t.Fatal("real conversation failed", w.Code, w.Body.String())
	}
	w = factoryGet(t, f, "/api/nav/asks?dir="+url.QueryEscape(x.work))
	var asks navAsksUnknownBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &asks) != nil || len(asks.Sections) == 0 || len(asks.Sections[0].Items) != 1 {
		t.Fatal("real read-only Asks failed", w.Code, w.Body.String())
	}
	w = factoryGet(t, f, "/api/nav/asks/1/turn")
	var placement navAskTurnBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &placement) != nil || placement.Anchor == nil || *placement.Anchor != x.at.UnixMilli() || placement.Confidence != "time+text" {
		t.Fatal("exact prompt placement failed", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatal("factory did not share one Index/cache")
	}
	if strings.Contains(w.Body.String(), navAppFixtureQuote) {
		t.Fatal("placement returned stored quote")
	}
}

func TestNavFactoryMissingAsksNeverFabricatesOrCreates(t *testing.T) {
	x := newNavAppFixture(t, -1, navAppFixtureQuote)
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := factoryGet(t, f, "/api/nav/asks")
	if w.Code != 503 || w.Body.String() != `{"error":"asks_unavailable"}` {
		t.Fatal("unread database fabricated facts", w.Code, w.Body.String())
	}
	if _, err = os.Stat(x.asksPath); !os.IsNotExist(err) {
		t.Fatal("read created missing Asks DB")
	}
}

func TestNavProductionAsksWorkloadAdmitsCompleteQuotes(t *testing.T) {
	x := newNavAppFixture(t, 256, strings.Repeat("x", 4096))
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := factoryGet(t, f, "/api/nav/asks")
	var body navAsksUnknownBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal("production complete-quote workload refused", w.Code, w.Body.String())
	}
	if len(body.Sections[0].Items) != 256 {
		t.Fatal("workload silently discarded rows")
	}
	for _, item := range body.Sections[0].Items {
		if strings.Count(item.First.Copy, "x") != 4096 || strings.Count(item.Latest.Copy, "x") != 4096 {
			t.Fatal("complete quote truncated")
		}
	}
}

func TestNavFactoryInitializationWaitIsCancelable(t *testing.T) {
	x := newNavAppFixture(t, 0, navAppFixtureQuote)
	in := x.input(nil)
	entered, release := make(chan struct{}), make(chan struct{})
	in.roots = func(context.Context) (transcript.Roots, error) { close(entered); <-release; return x.roots, nil }
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = f.get(context.Background()) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = f.get(ctx); err != context.Canceled {
		t.Fatal("initialization waiter ignored cancellation")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("initializer did not join")
	}
	if _, err = f.get(context.Background()); err != nil {
		t.Fatal("initialized factory unusable", err)
	}
}

func TestNavFactoryAsksAndPlacementShareAdmission(t *testing.T) {
	x := newNavAppFixture(t, 1, navAppFixtureQuote)
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	asks := b.asks.(*navAsksHandler)
	turn := b.turn.(*navAskTurnHandler)
	if asks.slots != turn.slots || cap(asks.slots) != 1 {
		t.Fatal("second Asks allowance")
	}
	asks.slots <- struct{}{}
	defer func() { <-asks.slots }()
	for _, path := range []string{"/api/nav/asks", "/api/nav/asks/1/turn"} {
		w := factoryGet(t, f, path)
		if w.Code != 503 || w.Body.String() != `{"error":"asks_busy"}` {
			t.Fatal("shared request admission bypassed", w.Code, w.Body.String())
		}
	}
}

func TestNavFactoryPagePolicyIsPublicAndComplete(t *testing.T) {
	x := newNavAppFixture(t, 0, navAppFixtureQuote)
	f, err := newNavFactory(x.input(nil), navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.pages.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-aiq-limits="16777216:65536:262144:16777216:50000"`) || strings.Contains(w.Body.String(), x.root) {
		t.Fatal("page policy incomplete or private root exposed")
	}
}
