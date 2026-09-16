package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// ClaudeOpen maps the ids of Claude sessions that are open right now to
// the pid holding them. Claude Code writes sessions/<pid>.json while it
// runs; a file whose process is gone is ignored.
func ClaudeOpen(realClaude string) map[string]int {
	out := map[string]int{}
	files, _ := filepath.Glob(filepath.Join(realClaude, "sessions", "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var e struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(data, &e) != nil || e.PID <= 0 || e.SessionID == "" {
			continue
		}
		if err := syscall.Kill(e.PID, 0); err == nil || err == syscall.EPERM {
			out[e.SessionID] = e.PID
		}
	}
	return out
}
