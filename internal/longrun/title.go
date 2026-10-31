package longrun

import (
	"path/filepath"
	"strings"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// sessionTitle takes the current saved name before a pane is respawned.
// Launch flags are only a fallback: /rename may have changed it since then.
func sessionTitle(l state.Lease, home string) string {
	var title string
	switch l.Provider {
	case "claude":
		title = transcript.ClaudeTitle(l.Transcript)
	case "codex":
		if home != "" && l.SessionID != "" {
			title = transcript.CodexTitle(filepath.Join(home, "session_index.jsonl"), l.SessionID)
		}
	}
	if title != "" {
		return title
	}
	if l.Provider == "claude" || l.Provider == "copilot" {
		args := splitArgs(l.Args)
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--":
				return title
			case (args[i] == "--name" || args[i] == "-n") && i+1 < len(args):
				i++
				title = args[i]
			case strings.HasPrefix(args[i], "--name="):
				title = strings.TrimPrefix(args[i], "--name=")
			}
		}
	}
	return title
}
