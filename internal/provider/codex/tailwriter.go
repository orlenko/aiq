package codex

import (
	"regexp"
	"strings"
	"sync"
)

// tailWriter keeps the last max bytes written to it. os/exec copies a child's
// stderr into it from its own goroutine, so the writes are locked.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// lastLine is the final non-empty line, stripped of the colour codes Codex
// writes and cut to one line of an error message. It is what a poll failure
// shows as the reason.
func (w *tailWriter) lastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := strings.Split(ansiRE.ReplaceAllString(string(w.buf), ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		return line
	}
	return ""
}
