package claude

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// subcommands are the claude words that are not a session: a first
// positional argument naming one of them gets no session id.
var subcommands = map[string]bool{
	"agents": true, "attach": true, "auth": true, "auto-mode": true, "config": true,
	"doctor": true, "gateway": true, "import": true, "install": true, "kill": true,
	"logs": true, "mcp": true, "migrate-installer": true, "plugin": true, "plugins": true,
	"project": true, "respawn": true, "rm": true, "setup-token": true, "stop": true,
	"ultrareview": true, "update": true, "upgrade": true,
}

// Session works out which session an interactive launch opens. A launch
// that names one (--resume <id>, --session-id <id>) returns it unchanged.
// A plain launch gets a fresh --session-id, so aiq knows the id before the
// CLI starts. Anything else (--continue, the resume picker, a fork, a
// subcommand) returns "" and the args unchanged.
func Session(args []string) (id string, out []string) {
	resume, fork := "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i = len(args)
		case a == "--session-id" || a == "--resume" || a == "-r":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", args // the picker
			}
			if a == "--session-id" {
				return args[i+1], args
			}
			resume = args[i+1]
			i++
		case strings.HasPrefix(a, "--session-id="):
			return strings.TrimPrefix(a, "--session-id="), args
		case strings.HasPrefix(a, "--resume="):
			resume = strings.TrimPrefix(a, "--resume=")
		case a == "--fork-session":
			fork = true
		case a == "--continue" || a == "-c" || a == "--from-pr" || strings.HasPrefix(a, "--from-pr=") ||
			a == "--teleport" || a == "--bg" || a == "-p" || a == "--print":
			return "", args
		}
	}
	if resume != "" {
		if fork {
			return "", args
		}
		return resume, args
	}
	if first := firstPositional(args); subcommands[first] {
		return "", args
	}
	id = newUUID()
	return id, append([]string{"--session-id", id}, args...)
}

// firstPositional is the first argument that is neither a flag nor, as far
// as a word can tell, a flag's value.
func firstPositional(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		}
		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && takesValue(a) {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// takesValue lists the claude flags whose value is a separate argument.
// A flag missing here only matters when its value is also a subcommand name.
func takesValue(flag string) bool {
	switch flag {
	case "--model", "--agent", "--agents", "--settings", "--setting-sources", "--add-dir",
		"--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools", "--tools",
		"--permission-mode", "--mcp-config", "--append-system-prompt", "--system-prompt",
		"--fallback-model", "--effort", "--plugin-dir", "--output-format", "--input-format",
		"--max-turns", "--name", "-n", "--worktree", "-w", "--debug-file", "--betas", "--ide":
		return true
	}
	return false
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
