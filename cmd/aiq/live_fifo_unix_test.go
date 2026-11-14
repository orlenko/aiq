//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
)

// A pipe among Claude's session files, in the real home or an overlay
// home's, does not hold aiq convo: liveClaude skips it and reads the rest.
func TestLiveClaudeSkipsAPipe(t *testing.T) {
	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	overlay := filepath.Join(paths.ClaudeHomesDir(), "claude1", "sessions")
	var fifos []string
	for _, d := range []string{filepath.Join(config, "sessions"), overlay} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "1.json")
		if err := syscall.Mkfifo(f, 0o600); err != nil {
			t.Fatal(err)
		}
		fifos = append(fifos, f)
	}
	live := fmt.Sprintf(`{"pid":%d,"sessionId":"5e551011-0000-4000-8000-0000000000aa","cwd":"/w"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(overlay, "2.json"), []byte(live), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan []claudeLive, 1)
	go func() { done <- liveClaude(paths.RealClaudeHome(), paths.ClaudeHomesDir(), proc.Alive) }()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].PID != os.Getpid() {
			t.Errorf("liveClaude() = %+v, want the one regular session file", got)
		}
	case <-time.After(5 * time.Second):
		// Open the write sides, in the order liveClaude reads them, so each
		// blocked open returns.
		for _, f := range fifos {
			if w, err := os.OpenFile(f, os.O_WRONLY, 0); err == nil {
				w.Close()
			}
		}
		<-done
		t.Fatal("liveClaude blocked on a pipe in sessions/")
	}
}
