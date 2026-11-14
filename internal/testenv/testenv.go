// Package testenv runs a package's tests with tmux, the provider homes and aiq's
// data pointed at a temporary directory, so no test reaches the runner's own tmux
// server, logins or aiq data by accident. Every package with tests calls Main from
// its TestMain.
package testenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// marker names the root of an environment Main set up. A test binary that a test
// runs again inherits it and keeps the environment its parent test gave it. It
// does not start with AIQ_, which Main clears.
const marker = "AIQTESTENV_ROOT"

// homes are set to directories of their own under the root. None lies inside
// another, as aiq falls back to the real home for one under its data directory.
var homes = []string{
	"HOME", "AIQ_DATA_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "QUOTA_HOME",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
}

// cleared are unset: the runner's tmux session, its desktop bus, and the
// credentials a provider CLI would use in place of a login. Every GIT_ variable
// is unset too, as a git hook exports the repository it runs in.
var cleared = []string{
	"TMUX", "TMUX_PANE", "DBUS_SESSION_BUS_ADDRESS",
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	"OPENAI_API_KEY", "CODEX_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
	"GITHUB_TOKEN", "GH_TOKEN", "COPILOT_GITHUB_TOKEN", "COPILOT_API_KEY",
}

// saved is the environment Main started with, for the go command.
var saved []string

// Main sets up the environment, runs the tests and exits with their code.
func Main(m *testing.M) {
	if inherited(os.Getenv(marker)) {
		os.Exit(m.Run())
	}
	os.Exit(run(m))
}

func inherited(root string) bool {
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "aiq-testenv-") {
		return false
	}
	fi, err := os.Stat(root)
	return err == nil && fi.IsDir()
}

func run(m *testing.M) int {
	saved = os.Environ()
	root, err := os.MkdirTemp("", "aiq-testenv-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		return 1
	}
	defer remove(root)
	tmuxDir, err := os.MkdirTemp(tmuxBase(), "aiqt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		return 1
	}
	defer remove(tmuxDir)
	if err := isolate(root, tmuxDir); err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		return 1
	}
	code := m.Run()
	killTmux(tmuxDir)
	return code
}

func isolate(root, tmuxDir string) error {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "AIQ_") && !testSwitch(k) || strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "HC_") || strings.HasPrefix(k, "HERDR_") {
			os.Unsetenv(k)
		}
	}
	for _, k := range cleared {
		os.Unsetenv(k)
	}
	for _, k := range homes {
		dir := filepath.Join(root, strings.ToLower(k))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		os.Setenv(k, dir)
	}
	os.Setenv("TMUX_TMPDIR", tmuxDir)
	os.Setenv(marker, root)
	if runtime.GOOS == "darwin" {
		return stubs(root)
	}
	return nil
}

// testSwitch is an AIQ_ variable only tests read, which a runner may set by hand.
func testSwitch(k string) bool {
	return strings.HasPrefix(k, "AIQ_TEST_") || strings.Contains(k, "_TEST_") || strings.HasSuffix(k, "_HELPER")
}

// stubs puts failing security and launchctl commands first on PATH, as the
// login keychain and launchd are the runner's own whatever HOME says.
func stubs(root string) error {
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		return err
	}
	for name, code := range map[string]int{"security": 44, "launchctl": 1} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", code)), 0o700); err != nil {
			return err
		}
	}
	os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return nil
}

// tmuxBase is where the tmux directory goes: the temporary directory when a
// socket under it fits in a Unix socket path, else /tmp.
func tmuxBase() string {
	dir := os.TempDir()
	real := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		real = r
	}
	if len(real)+len("/aiqt0123456789")+len(fmt.Sprintf("/tmux-%d/default", os.Getuid())) <= 100 {
		return dir
	}
	return "/tmp"
}

// killTmux stops a tmux server a test left on the temporary default socket,
// giving it 5 s.
func killTmux(dir string) {
	sock := filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
	if _, err := os.Stat(sock); err != nil {
		return
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		exec.CommandContext(ctx, "tmux", "-S", sock, "kill-server").Run()
	}
}

func remove(dir string) {
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
	}
}

// GoCommand is the go command with the environment Main started with, less aiq's
// variables, tmux's, the provider homes and the credentials: the runner's go
// settings and module cache, and nothing a test should not reach. In a test
// binary that a test runs again, that is the parent test's environment.
func GoCommand(args ...string) *exec.Cmd {
	env := saved
	if env == nil {
		env = os.Environ()
	}
	drop := map[string]bool{marker: true, "TMUX_TMPDIR": true, "CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true, "COPILOT_HOME": true, "QUOTA_HOME": true}
	for _, k := range cleared {
		drop[k] = true
	}
	var keep []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] && !strings.HasPrefix(k, "AIQ_") {
			keep = append(keep, kv)
		}
	}
	cmd := exec.Command("go", args...)
	cmd.Env = keep
	return cmd
}
