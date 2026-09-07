// Package codex implements the Codex provider. Each account is a CODEX_HOME
// overlay: every entry of the real ~/.codex is symlinked in (config,
// sessions, skills, sqlite state), and only auth.json belongs to the account.
package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/fsutil"
	"github.com/orlenko/aiq/internal/overlay"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
)

// Provider launches Codex. Command builds a child process for the real CLI
// (the caller decides how the binary is found).
type Provider struct {
	Command func(args []string, env []string) *exec.Cmd
}

// OverlaySpec describes an account home layered over the real ~/.codex.
func OverlaySpec(home string) overlay.Spec {
	return overlay.Spec{
		Real:         paths.RealCodexHome(),
		Overlay:      home,
		Exclude:      []string{"auth.json"},
		SkipPrefixes: []string{".DS_Store", ".aiq-"},
		LockDir:      paths.LocksDir(),
	}
}

// AuthPath returns the credential file inside a home.
func AuthPath(home string) string { return filepath.Join(home, "auth.json") }

// HasCredential reports whether the home holds an auth.json.
func HasCredential(home string) bool {
	_, err := os.Stat(AuthPath(home))
	return err == nil
}

// ImportAuth copies a credential file into the account home.
func ImportAuth(src, home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := fsutil.CopyFileAtomic(src, AuthPath(home)); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return nil
}

// Env builds the child environment for a launch on the given home.
func (p *Provider) Env(home string, native bool, inheritAuthEnv bool) []string {
	env := os.Environ()
	if !inheritAuthEnv {
		env = proc.SanitizeEnv(env, "OPENAI_API_KEY", "CODEX_API_KEY")
	}
	env = proc.SanitizeEnv(env, "CODEX_HOME")
	if !native {
		env = append(env, "CODEX_HOME="+home)
	} else if real := paths.RealCodexHome(); real != filepath.Join(userHome(), ".codex") {
		env = append(env, "CODEX_HOME="+real)
	}
	return env
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// Login runs `codex login` interactively inside the account home.
func (p *Provider) Login(home string, native bool) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Running `codex login` — authenticate in the browser as the account you are enrolling.")
	// The credential must land in auth.json, not an OS keyring: the overlay
	// home is what tells accounts apart.
	code, err := proc.RunInteractive(p.Command([]string{"-c", `cli_auth_credentials_store="file"`, "login"}, p.Env(home, native, false)))
	if err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("codex login exited with code %d", code)
	}
	if !native && !HasCredential(home) {
		return fmt.Errorf("login finished but %s was not created (is cli_auth_credentials_store set to keyring in config.toml?)", AuthPath(home))
	}
	return nil
}

var passthrough = map[string]bool{
	"login": true, "logout": true, "completion": true,
	"--version": true, "-V": true, "--help": true, "-h": true,
}

// Passthrough reports whether args should bypass routing.
func Passthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return passthrough[args[0]]
}

// IsWorker classifies an invocation: `codex exec` or a non-terminal stdout
// means a disposable worker.
func IsWorker(args []string) bool {
	for _, a := range args {
		if a == "exec" || a == "e" {
			return true
		}
	}
	return !term.IsTerminal(int(os.Stdout.Fd()))
}

var modelRe = regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"`)

// DefaultModel reads `model = "..."` from the real config.toml.
func DefaultModel() string {
	data, err := os.ReadFile(filepath.Join(paths.RealCodexHome(), "config.toml"))
	if err != nil {
		return ""
	}
	if m := modelRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// HookArgs renders the -c overrides that wire aiq's hooks into a long
// session, plus the flags that let them run without a trust prompt.
func HookArgs(aiqBin string) []string {
	hook := func(event, name string) string {
		return fmt.Sprintf(`hooks.%s=[{hooks=[{type="command",command=%q,timeout=30}]}]`, event, aiqBin+" codex-hook "+name)
	}
	return []string{
		"--enable", "hooks", "--dangerously-bypass-hook-trust",
		"-c", hook("SessionStart", "sessionstart"),
		"-c", hook("UserPromptSubmit", "userpromptsubmit"),
		"-c", hook("Stop", "stop"),
	}
}
