package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// claudeTranscript takes an id, never a path or a pattern: each of these
// would otherwise find s1.jsonl or a file outside the project folders.
func TestClaudeTranscriptTakesOnlyAnID(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"-w-a/s1.jsonl", "outside.jsonl"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	roots := []string{root}
	if got := claudeTranscript(roots, "/w/a", "s1"); got != filepath.Join(root, "-w-a", "s1.jsonl") {
		t.Fatalf("s1: %q", got)
	}
	for _, id := range []string{"*", "s?", "[s]1", "../outside", "../-w-a/s1"} {
		if got := claudeTranscript(roots, "/w/a", id); got != "" {
			t.Errorf("id %q found %q", id, got)
		}
	}
}

// A Claude session file whose session id is not an id is skipped.
func TestLiveClaudeSkipsSessionFilesWithoutAnID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	dir := filepath.Join(home, "sessions")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]claudeLive{
		"1.json": {PID: os.Getpid(), SessionID: "../../x/*", Cwd: "/w", Entrypoint: "cli"},
		"2.json": {PID: os.Getppid(), SessionID: "good-1", Cwd: "/w", Entrypoint: "cli"},
	} {
		data, _ := json.Marshal(c)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := liveClaude()
	if len(got) != 1 || got[0].SessionID != "good-1" {
		t.Fatalf("liveClaude = %+v, want only good-1", got)
	}
}
