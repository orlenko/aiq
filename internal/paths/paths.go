// Package paths resolves aiq's on-disk locations.
//
// Data lives under $AIQ_DATA_DIR, else $XDG_DATA_HOME/aiq, else
// ~/.local/share/aiq. Config lives at $AIQ_CONFIG, else
// $XDG_CONFIG_HOME/aiq/config.toml, else ~/.config/aiq/config.toml.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// DataDir returns the root data directory (not necessarily created yet).
func DataDir() string {
	if d := os.Getenv("AIQ_DATA_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "aiq")
	}
	return filepath.Join(home(), ".local", "share", "aiq")
}

// ConfigFile returns the config file path.
func ConfigFile() string {
	if c := os.Getenv("AIQ_CONFIG"); c != "" {
		return c
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "aiq", "config.toml")
	}
	return filepath.Join(home(), ".config", "aiq", "config.toml")
}

func StateDB() string  { return filepath.Join(DataDir(), "state.db") }
func LocksDir() string { return filepath.Join(DataDir(), "locks") }
func LogDir() string   { return filepath.Join(DataDir(), "log") }
func ShimsDir() string { return filepath.Join(DataDir(), "shims") }

// ShimPath is where the shim for a command name lives.
func ShimPath(name string) string { return filepath.Join(ShimsDir(), name) }
func ClaudeHomesDir() string      { return filepath.Join(DataDir(), "claude") }
func CodexHomesDir() string       { return filepath.Join(DataDir(), "codex") }

// EnsureDirs creates every directory aiq needs, mode 0700.
func EnsureDirs() error {
	dirs := []string{
		DataDir(), LocksDir(), LogDir(), ShimsDir(), ClaudeHomesDir(), CodexHomesDir(),
		filepath.Dir(ConfigFile()),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// isManaged reports whether dir is one of aiq's own overlay homes.
func isManaged(dir string) bool {
	if dir == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	data := DataDir()
	return strings.HasPrefix(abs, data+string(filepath.Separator))
}

// RealClaudeHome returns the user's real Claude config directory. A
// CLAUDE_CONFIG_DIR pointing at one of aiq's overlay homes (inherited from a
// routed parent session) is ignored; any other override is respected.
func RealClaudeHome() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" && !isManaged(d) {
		return d
	}
	return filepath.Join(home(), ".claude")
}

// ClaudeGlobalJSON returns the real ~/.claude.json path, which Claude Code
// keeps beside (not inside) its config directory unless CLAUDE_CONFIG_DIR is
// set, in which case it lives inside that directory.
func ClaudeGlobalJSON() string {
	real := RealClaudeHome()
	if real != filepath.Join(home(), ".claude") {
		return filepath.Join(real, ".claude.json")
	}
	return filepath.Join(home(), ".claude.json")
}

// RealCodexHome returns the user's real Codex home, ignoring a CODEX_HOME
// that points at one of aiq's overlay homes.
func RealCodexHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" && !isManaged(d) {
		return d
	}
	return filepath.Join(home(), ".codex")
}
