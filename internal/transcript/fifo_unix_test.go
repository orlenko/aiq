//go:build unix

package transcript

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO wherever a transcript, a rollout, the Antigravity history or a
// Claude session record belongs is skipped, not waited on: nothing writes to
// it, so reading it would hang the list.
func TestListSkipsFIFOs(t *testing.T) {
	r, work := listFixture(t)
	appData := r.AgyAppData[0]
	claudeHome := filepath.Dir(r.ClaudeProjects[0])
	fifos := []string{
		filepath.Join(r.ClaudeProjects[0], nonAlnum.ReplaceAllString(work, "-"), "fifo.jsonl"),
		filepath.Join(r.CodexSessions[0], "2026", "09", "16", "rollout-fifo.jsonl"),
		AgyTranscript(appData, "fifo"),
		filepath.Join(appData, "cache", "last_conversations.json"),
		filepath.Join(claudeHome, "sessions", "1.json"),
	}
	history := filepath.Join(appData, "history.jsonl")
	os.Rename(history, history+".kept")
	fifos = append(fifos, history)
	for _, p := range fifos {
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan []Session, 1)
	go func() {
		list, _, _ := List(r, work)
		Written(r, "codex", work, time.Time{})
		Written(r, "agy", work, time.Time{})
		ClaudeOpen(claudeHome)
		ClaudeTitle(fifos[0])
		// The navigator's index and cache open the same files.
		x, c := NewIndex(r), NewCache()
		ListWith(context.Background(), x, work, c.Summary)
		x.Written(context.Background(), "agy", work, time.Time{})
		c.Session(context.Background(), fifos[0])
		done <- list
	}()
	select {
	case list := <-done:
		if len(list) != 4 {
			t.Errorf("%d sessions listed, want the 4 regular Claude and Codex ones", len(list))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a FIFO hung the list")
	}
}
