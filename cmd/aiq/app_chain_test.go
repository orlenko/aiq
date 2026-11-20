package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// appChainFixture is three sessions an aiq long chain joins (claude, codex,
// claude) and the links between them, with a missing session first.
func appChainFixture(t *testing.T) (appHistoryDeps, transcript.Roots, []string) {
	t.Helper()
	root := t.TempDir()
	projects, sessions := filepath.Join(root, "projects"), filepath.Join(root, "sessions")
	roots := transcript.Roots{ClaudeProjects: []string{projects}, CodexSessions: []string{sessions}}
	gone, a, b, c := appHistoryFixtureID(9, 0), appHistoryFixtureID(9, 1), appHistoryFixtureID(9, 2), appHistoryFixtureID(9, 3)
	claude := func(id, prompt, reply string, at time.Time) {
		path := filepath.Join(projects, "-invented-work", id+".jsonl")
		appHistoryWriteLines(t, path,
			fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":"/invented/work","timestamp":%q,"message":{"content":%q}}`, id, at.Format(time.RFC3339), prompt),
			fmt.Sprintf(`{"type":"assistant","sessionId":%q,"timestamp":%q,"message":{"content":[{"type":"text","text":%q}]}}`, id, at.Add(time.Second).Format(time.RFC3339), reply))
		os.Chtimes(path, at, at)
	}
	claude(a, "Invented first request.", "Invented first reply.", time.Date(2031, 7, 1, 10, 0, 0, 0, time.UTC))
	codexPath := filepath.Join(sessions, "2031", "07", "01", "rollout-2031-07-01T11-00-00-"+b+".jsonl")
	appHistoryWriteLines(t, codexPath, strings.Split(strings.ReplaceAll(strings.Join(codexConvoFixture(), "\n"), `"id":"c"`, `"id":"`+b+`"`), "\n")...)
	at := time.Date(2031, 7, 1, 11, 0, 0, 0, time.UTC)
	os.Chtimes(codexPath, at, at)
	claude(c, "Invented last request.", "Invented last reply.", time.Date(2031, 7, 1, 12, 0, 0, 0, time.UTC))
	links := []state.SessionLinkRow{ // newest first, as the store reads them
		{Provider: "claude", SessionID: c, PrevProvider: "codex", PrevSessionID: b, LinkedAtMS: 3},
		{Provider: "codex", SessionID: b, PrevProvider: "claude", PrevSessionID: a, LinkedAtMS: 2},
		{Provider: "claude", SessionID: a, PrevProvider: "claude", PrevSessionID: gone, LinkedAtMS: 1},
	}
	d := appHistoryDeps{input: navFactoryInput{now: time.Now, asksPath: filepath.Join(root, "absent.db"),
		links: func(context.Context) ([]state.SessionLinkRow, error) { return links, nil }},
		cache: transcript.NewCache(), source: transcript.NewIndex(roots)}
	return d, roots, []string{gone, a, b, c}
}

func appChainJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every session of a chain carries the same key, its place and the members.
func TestAppSessionsMarkRolloverChains(t *testing.T) {
	d, roots, ids := appChainFixture(t)
	out, err := d.run(context.Background(), roots, "sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := appChainJSON(t, out.Data)["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	key := ""
	for _, raw := range rows {
		r := raw.(map[string]any)
		c, ok := r["chain"].(map[string]any)
		if !ok {
			t.Fatalf("row without chain: %v", r)
		}
		if key == "" {
			key = c["key"].(string)
		}
		want := map[string]float64{ids[1]: 2, ids[2]: 3, ids[3]: 4}[r["id"].(string)]
		if c["key"] != key || c["size"] != float64(4) || c["seq"] != want || len(c["members"].([]any)) != 4 {
			t.Fatalf("chain of %v = %v", r["id"], c)
		}
	}
}

// The chain conversation holds every session's messages in order, marks the
// one it cannot read, and pages across sessions.
func TestAppConversationReadsTheWholeChain(t *testing.T) {
	d, roots, ids := appChainFixture(t)
	out, err := d.run(context.Background(), roots, "conversation", []string{"--provider", "claude", "--id", ids[3], "--chain", "1"})
	if err != nil {
		t.Fatal(err)
	}
	data := appChainJSON(t, out.Data)
	sessions := data["sessions"].([]any)
	if len(sessions) != 4 || sessions[0].(map[string]any)["available"] != false || sessions[2].(map[string]any)["provider"] != "codex" {
		t.Fatalf("sessions = %v", sessions)
	}
	if !out.Partial || out.Omitted["sessions"] != 1 {
		t.Fatalf("missing session not reported: %v %v", out.Partial, out.Omitted)
	}
	msgs := data["messages"].([]any)
	first, last := msgs[0].(map[string]any), msgs[len(msgs)-1].(map[string]any)
	if first["text"] != "Invented first request." || first["session"] != float64(1) || !strings.HasPrefix(first["key"].(string), "s1-m") {
		t.Fatalf("first = %v", first)
	}
	if last["text"] != "Invented last reply." || last["session"] != float64(3) {
		t.Fatalf("last = %v", last)
	}
	codex := false
	for _, m := range msgs {
		codex = codex || m.(map[string]any)["text"] == "Deployed to staging." && m.(map[string]any)["session"] == float64(2)
	}
	if !codex {
		t.Fatalf("the codex session's messages are missing: %v", msgs)
	}

	// A cursor from inside one session pages into the one before it.
	before := ""
	for _, m := range msgs {
		if m.(map[string]any)["session"] == float64(3) {
			before = m.(map[string]any)["key"].(string)
			break
		}
	}
	older, err := d.run(context.Background(), roots, "conversation", []string{"--provider", "claude", "--id", ids[3], "--chain", "1", "--before", before})
	if err != nil {
		t.Fatal(err)
	}
	page := appChainJSON(t, older.Data)["messages"].([]any)
	if len(page) == 0 || page[len(page)-1].(map[string]any)["session"] != float64(2) {
		t.Fatalf("page before %s = %v", before, page)
	}

	for _, bad := range [][]string{{"--chain", "2"}, {"--chain", "1", "--at", "5"}} {
		args := append([]string{"--provider", "claude", "--id", ids[3]}, bad...)
		if _, err := d.run(context.Background(), roots, "conversation", args); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	// A session with no chain reads as a chain of one.
	d.input.links = nil
	alone, err := d.run(context.Background(), roots, "conversation", []string{"--provider", "claude", "--id", ids[3], "--chain", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := appChainJSON(t, alone.Data); len(got["sessions"].([]any)) != 1 || len(got["messages"].([]any)) != 2 {
		t.Fatalf("lone session = %v", got)
	}
}

// Links that loop or fork still make finite chains, and a successor that
// names another predecessor does not extend this one.
func TestAppChainsCutLoopsAndForks(t *testing.T) {
	id := func(i int) string { return appHistoryFixtureID(8, i) }
	c := newAppChains([]state.SessionLinkRow{
		{Provider: "claude", SessionID: id(1), PrevProvider: "codex", PrevSessionID: id(2)},
		{Provider: "codex", SessionID: id(2), PrevProvider: "claude", PrevSessionID: id(1)},
		{Provider: "claude", SessionID: id(3), PrevProvider: "claude", PrevSessionID: id(4)},
		{Provider: "claude", SessionID: id(5), PrevProvider: "claude", PrevSessionID: id(4)},
	})
	if loop, ok := c.of("claude", id(1)); !ok || loop.Size != 2 {
		t.Fatalf("loop = %+v", loop)
	}
	fork, ok := c.of("claude", id(4))
	if !ok || fork.Size != 2 || fork.Seq != 1 {
		t.Fatalf("fork = %+v", fork)
	}
	if _, ok := c.of("claude", id(9)); ok {
		t.Fatal("a session no link names has a chain")
	}
}
