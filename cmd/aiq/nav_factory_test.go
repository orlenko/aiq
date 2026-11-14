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

func TestNavFactoryRefreshRootClassAndSharedAdmission(t *testing.T) {
	for _, mode := range []string{"new-provider-parent", "new-overlay-home", "new-transcript-child", "removed-root", "label-change", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			x := newNavAppFixture(t, 0, navAppFixtureQuote)
			in := x.input(nil)
			now := x.at
			in.now = func() time.Time { return now }
			roots := cloneNavRoots(x.roots)
			in.roots = func(context.Context) (transcript.Roots, error) { return roots, nil }
			f, err := newNavFactory(in, navProductionPolicy())
			if err != nil {
				t.Fatal(err)
			}
			old, err := f.get(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "new-provider-parent":
				roots.CodexSessions = append(roots.CodexSessions, filepath.Join(x.root, "invented-new-parent", "account", "sessions"))
			case "new-overlay-home":
				roots.ClaudeProjects = append(roots.ClaudeProjects, filepath.Join(x.root, "invented-new-home", "projects"))
			case "new-transcript-child":
				roots.ClaudeProjects = append(roots.ClaudeProjects, filepath.Join(x.root, "invented-account", "projects"))
			case "removed-root":
				roots.ClaudeProjects = nil
			case "label-change":
				roots.Labels = map[string]string{x.roots.ClaudeProjects[0]: "invented-label"}
			}
			within, err := f.get(context.Background())
			if err != nil || within != old {
				t.Fatal("roots did not retain the discovery TTL")
			}
			now = now.Add(transcript.DiscoveryTTL)
			next, err := f.get(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if (next == old) != (mode == "unchanged") {
				t.Fatal("root generation refresh/reuse wrong", mode)
			}
			a, b := old.sessions.(*navSessionsHandler), next.sessions.(*navSessionsHandler)
			da, db := old.detail.(*navDetailHandler), next.detail.(*navDetailHandler)
			aa, ab := old.asks.(*navAsksHandler), next.asks.(*navAsksHandler)
			if a.slots != b.slots || da.slots != db.slots || aa.slots != ab.slots || ab.slots != next.turn.(*navAskTurnHandler).slots {
				t.Fatal("root refresh multiplied admission")
			}
			aa.slots <- struct{}{}
			w := factoryGet(t, f, "/api/nav/asks")
			<-aa.slots
			if w.Code != 503 || w.Body.String() != "{\"error\":\"asks_busy\"}" {
				t.Fatal("old generation admission bypassed")
			}
		})
	}
}

func TestNavFactoryProductionOverlayAppearsAfterInitialRead(t *testing.T) {
	for _, stage := range []string{"parent", "home", "projects", "metadata-file"} {
		t.Run(stage, func(t *testing.T) {
			x := newNavAppFixture(t, 0, navAppFixtureQuote)
			data := filepath.Join(x.root, "isolated-data")
			t.Setenv("AIQ_DATA_DIR", data)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(x.root, "empty-native-claude"))
			t.Setenv("CODEX_HOME", filepath.Join(x.root, "empty-native-codex"))
			t.Setenv("HOME", filepath.Join(x.root, "empty-home"))
			parent := filepath.Join(data, "claude")
			home := filepath.Join(parent, "invented-account")
			if stage == "home" {
				if err := os.MkdirAll(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "projects" {
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "metadata-file" {
				if err := os.MkdirAll(parent, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(parent, ".DS_Store"), []byte("invented metadata"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			in := navProductionInput()
			in.asksPath = x.asksPath
			now := x.at
			in.now = func() time.Time { return now }
			in.location = time.UTC
			f, err := newNavFactory(in, navProductionPolicy())
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/nav/sessions?dir=" + url.QueryEscape(x.work) + "&workers=0"
			first := factoryGet(t, f, path)
			var before navSessionsBody
			if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &before) != nil || len(before.Sessions) != 0 {
				t.Fatal("empty fixture was not empty")
			}
			original := filepath.Join(x.roots.ClaudeProjects[0], claudeNonAlnum.ReplaceAllString(x.work, "-"), fullIDFixtureID+".jsonl")
			raw, err := os.ReadFile(original)
			if err != nil {
				t.Fatal(err)
			}
			created := filepath.Join(home, "projects", claudeNonAlnum.ReplaceAllString(x.work, "-"), fullIDFixtureID+".jsonl")
			if err = os.MkdirAll(filepath.Dir(created), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(created, raw, 0600); err != nil {
				t.Fatal(err)
			}
			now = now.Add(transcript.DiscoveryTTL)
			after := factoryGet(t, f, path)
			var list navSessionsBody
			if after.Code != 200 || json.Unmarshal(after.Body.Bytes(), &list) != nil || len(list.Sessions) != 1 || list.Sessions[0].ID != fullIDFixtureID {
				t.Fatal("new production overlay stayed invisible")
			}
			detail := factoryGet(t, f, "/api/nav/sessions/claude/"+fullIDFixtureID+"?last=50&markup=0")
			if detail.Code != 200 {
				t.Fatal("refreshed roots did not reach shared conversation lookup")
			}
		})
	}
}
