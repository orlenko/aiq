// Package copilot implements the Copilot provider.
package copilot

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
)

// Provider launches Copilot.
type Provider struct {
	Command func(args []string, env []string) *exec.Cmd
}

// Env builds the child environment for a launch on the given home.
func (p *Provider) Env(home string, native bool, inheritAuthEnv bool) []string {
	env := os.Environ()
	if !inheritAuthEnv {
		env = proc.SanitizeEnv(env, "GITHUB_TOKEN", "COPILOT_API_KEY")
	}
	env = proc.SanitizeEnv(env, "COPILOT_HOME")
	if !native {
		env = append(env, "COPILOT_HOME="+home)
	} else if real := paths.RealCopilotHome(); real != filepath.Join(userHome(), ".copilot") {
		env = append(env, "COPILOT_HOME="+real)
	}
	return env
}

func userHome() string {
	h, _ := os.UserHomeDir()
	return h
}

// Login runs `copilot auth` or something? Copilot uses `copilot auth` or `copilot setup`? Let's just say `copilot` and instruct to auth. Wait, looking at `copilot --help`, there is no `login` command! Ah, GitHub Copilot usually handles login via device flow when first launched. Wait, let's just make it run `copilot init` or `copilot` and tell the user to follow prompts.
func (p *Provider) Login(home string, native bool) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Running `copilot` — authenticate in the browser if prompted.")
	code, err := proc.RunInteractive(p.Command(nil, p.Env(home, native, false)))
	if err != nil {
		return fmt.Errorf("copilot: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("copilot exited with code %d", code)
	}
	return nil
}

var passthrough = map[string]bool{
	"init": true, "auth": true, "setup": true, "completion": true,
	"--version": true, "-V": true, "--help": true, "-h": true,
}

// Passthrough reports whether args should bypass routing.
func Passthrough(args []string) bool {
	if len(args) == 0 {
		return false
	}
	return passthrough[args[0]]
}

// IsWorker classifies an invocation.
func IsWorker(args []string) bool {
	for _, a := range args {
		if a == "-p" || strings.HasPrefix(a, "-p=") {
			return true
		}
	}
	return !term.IsTerminal(int(os.Stdout.Fd()))
}

var modelRe = regexp.MustCompile(`(?m)^\s*"model"\s*:\s*"([^"]+)"`)

// DefaultModel reads `model = "..."` from the config.json.
func DefaultModel() string {
	data, err := os.ReadFile(filepath.Join(paths.RealCopilotHome(), "config.json"))
	if err != nil {
		return ""
	}
	if m := modelRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// UnattendedArgs turn off TUI popups.
var UnattendedArgs = []string{}

func RateLimitPromptOpen(screen string) bool {
	return false
}

// HookArgs renders the -c overrides that wire aiq's hooks into a long session.
func HookArgs(aiqBin string) []string {
	// Not implemented for copilot yet.
	return []string{}
}

// Session extracts session id.
func Session(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "--resume=") {
			return strings.TrimPrefix(a, "--resume=")
		}
		if strings.HasPrefix(a, "--session-id=") {
			return strings.TrimPrefix(a, "--session-id=")
		}
	}
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return args[i+1]
		}
		if a == "--session-id" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return args[i+1]
		}
	}
	return ""
}
