package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestAppGoldenSavedNames(t *testing.T) {
	env, x := appTestEnv(t)
	path := filepath.Join(x.roots.ClaudeProjects[0], "invented-names", fullIDFixtureID+".jsonl")
	// This newer copy is found by the exact session id, regardless of the ask's
	// workspace. A later incomplete rename must not replace the saved title.
	appHistoryWriteLines(t, path, `{"type":"custom-title","customTitle":"Old invented name"}`, `{"type":"custom-title","customTitle":"Harp"}`, `{"type":"custom-title","customTitle":`)
	appHistoryWriteLines(t, x.roots.CodexIndex[0], `{"id":"`+appTestCodexID+`","thread_name":"Invented migration"}`)
	doc := appGolden(t, "asks.named", env, x, "asks")
	rows := appData(t, doc)["rows"].([]any)
	if rows[0].(map[string]any)["session"].(map[string]any)["name"] != "Harp" {
		t.Fatal("saved Claude name missing")
	}
	if rows[1].(map[string]any)["session"].(map[string]any)["name"] != "Invented migration" {
		t.Fatal("saved Codex name missing")
	}
}

func TestAppNamesAreOptionalExactMetadata(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	id := appHistoryFixtureID(9, 1)
	path := filepath.Join(projects, "invented", id+".jsonl")
	appHistoryWriteLines(t, path, `{"type":"user","customTitle":"Prompt is not a name"}`, `{"type":"custom-title","customTitle":"Invented saved name"}`, `{"type":"custom-title","sessionId":"`+appHistoryFixtureID(9, 2)+`","customTitle":"Other session"}`)
	env := appEnv{input: navFactoryInput{roots: func(context.Context) (transcript.Roots, error) {
		return transcript.Roots{ClaudeProjects: []string{projects}}, nil
	}}}
	asks := []state.Ask{{Provider: "claude", SessionID: id}, {Provider: "claude", SessionID: id}, {Provider: "claude", SessionID: "../not-an-id"}}
	names := env.askNames(context.Background(), asks)
	if len(names) != 1 || names["claude/"+id].Name != "Invented saved name" || names["claude/"+id].Source != "saved" {
		t.Fatal("wrong saved metadata", names)
	}
	appHistoryWriteLines(t, path, `{"type":"custom-title","customTitle":""}`)
	if name := env.askNames(context.Background(), asks)["claude/"+id].Name; name != "" {
		t.Fatal("cleared title retained", name)
	}
	os.Remove(path)
	if got := env.askNames(context.Background(), asks); len(got) != 0 {
		t.Fatal("missing metadata invented a name")
	}
}

func TestAppNameRecordsBoundedSuffixAndCancellation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "invented.jsonl")
	title := `{"type":"custom-title","customTitle":"Invented tail name"}`
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 3<<20)+"\n"+title+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	budget := int64(8 << 20)
	calls := 0
	appNameRecords(context.Background(), p, &budget, func(line []byte) bool { calls++; return string(line) == title })
	if calls != 1 || budget != 6<<20 {
		t.Fatal("suffix was not bounded", calls, budget)
	}
	budget = 8
	calls = 0
	appNameRecords(context.Background(), p, &budget, func([]byte) bool { calls++; return false })
	if calls != 0 || budget != 0 {
		t.Fatal("partial boundary record was parsed", calls, budget)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	budget = 123
	appNameRecords(ctx, p, &budget, func([]byte) bool { t.Fatal("cancelled read invoked parser"); return false })
	if budget != 123 {
		t.Fatal("cancelled read consumed input")
	}
}
