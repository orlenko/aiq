package codex

import "strings"

// Session returns the thread a launch resumes (`codex resume <id>`), or ""
// when the launch starts a new thread or picks one interactively. Codex
// has no flag to choose a new thread's id, so a new thread is matched to
// its launch later by directory and time.
func Session(args []string) string {
	for i, a := range args {
		if a == "--" {
			return ""
		}
		if a != "resume" {
			if !strings.HasPrefix(a, "-") && (i == 0 || !strings.HasPrefix(args[i-1], "-")) {
				return "" // a prompt or another subcommand came first
			}
			continue
		}
		for _, next := range args[i+1:] {
			switch {
			case next == "--last" || next == "--all":
				return ""
			case strings.HasPrefix(next, "-"):
				continue
			}
			return next
		}
		return ""
	}
	return ""
}
