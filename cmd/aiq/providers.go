package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/overlay"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/provider/copilot"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// The provider packages share no interface: each exports the functions its
// CLI calls for. This file is where the command layer tells them apart, so
// that a new CLI is added here and in its package rather than in every
// command.

func knownProvider(p string) bool { return config.KnownProvider(p) }

// providerList is the "claude, codex or agy" of an error message.
func providerList() string { return joinOr(config.Providers) }

// joinOr renders a list as "a, b or c".
func joinOr(items []string) string {
	out := ""
	for i, item := range items {
		switch {
		case i == 0:
		case i == len(items)-1:
			out += " or "
		default:
			out += ", "
		}
		out += item
	}
	return out
}

// cliName is the product name of a provider, for the menu and help.
func cliName(provider string) string {
	switch provider {
	case "codex":
		return "Codex"
	case "agy":
		return "Antigravity"
	case "copilot":
		return "Copilot"
	}
	return "Claude Code"
}

func (a *app) claudeProvider() (*claude.Provider, error) {
	if _, err := a.binary("claude"); err != nil {
		return nil, err
	}
	return &claude.Provider{Command: func(args, env []string) *exec.Cmd { return providerCommand("claude", args, env) }}, nil
}

func (a *app) codexProvider() (*codex.Provider, error) {
	if _, err := a.binary("codex"); err != nil {
		return nil, err
	}
	return &codex.Provider{Command: func(args, env []string) *exec.Cmd { return providerCommand("codex", args, env) }}, nil
}

func (a *app) agyProvider() (*agy.Provider, error) {
	if _, err := a.binary("agy"); err != nil {
		return nil, err
	}
	return &agy.Provider{Command: func(args, env []string) *exec.Cmd { return providerCommand("agy", args, env) }}, nil
}

func (a *app) copilotProvider() (*copilot.Provider, error) {
	if _, err := a.binary("copilot"); err != nil {
		return nil, err
	}
	return &copilot.Provider{Command: func(args, env []string) *exec.Cmd { return providerCommand("copilot", args, env) }}, nil
}

// providerPassthrough reports whether the CLI's own management subcommands
// were named, which run on the real home unrouted.
func providerPassthrough(provider string, args []string) bool {
	switch provider {
	case "claude":
		return claude.Passthrough(args)
	case "codex":
		return codex.Passthrough(args)
	case "agy":
		return agy.Passthrough(args)
	case "copilot":
		return copilot.Passthrough(args)
	}
	return false
}

// providerIsWorker classifies a launch as a disposable worker.
func providerIsWorker(provider string, args []string) bool {
	switch provider {
	case "claude":
		return claude.IsWorker(args)
	case "codex":
		return codex.IsWorker(args)
	case "agy":
		return agy.IsWorker(args)
	case "copilot":
		return copilot.IsWorker(args)
	}
	return false
}

// providerSession works out the session an interactive launch opens, and
// may rewrite the args so the CLI uses an id aiq chose (Claude only).
func providerSession(provider string, args []string) (string, []string) {
	switch provider {
	case "claude":
		return claude.Session(args)
	case "codex":
		return codex.Session(args), args
	case "agy":
		return agy.Session(args), args
	case "copilot":
		return copilot.Session(args), args
	}
	return "", args
}

// providerOverlaySpec describes the overlay home of a non-native account.
func providerOverlaySpec(acc state.Account) (overlay.Spec, bool) {
	switch acc.Provider {
	case "claude":
		return claude.OverlaySpec(acc.Home), true
	case "codex":
		return codex.OverlaySpec(acc.Home), true
	}
	return overlay.Spec{}, false
}

// providerEnv builds the CLI's environment for a launch on an account.
func (a *app) providerEnv(provider string, acc state.Account, inheritAuthEnv bool) []string {
	switch provider {
	case "claude":
		p, _ := a.claudeProvider()
		return p.Env(acc.Home, acc.Native, inheritAuthEnv)
	case "codex":
		p, _ := a.codexProvider()
		return p.Env(acc.Home, acc.Native, inheritAuthEnv)
	case "agy":
		p, _ := a.agyProvider()
		return p.Env(acc.Home, acc.Native, inheritAuthEnv)
	case "copilot":
		p, _ := a.copilotProvider()
		return p.Env(acc.Home, acc.Native, inheritAuthEnv)
	}
	return os.Environ()
}

// bypassFlag is the permission bypass as aiq adds it for a session it
// starts (auto, the menu, a resume).
func bypassFlag(provider string) string {
	if provider == "codex" || provider == "copilot" {
		return "--yolo"
	}
	return longrun.BypassFlag[provider]
}

// workerArgs are the CLI's non-interactive verb and its prompt.
func workerArgs(provider, prompt string) []string {
	switch provider {
	case "codex":
		return []string{"exec", "--", prompt}
	case "agy", "copilot":
		return []string{"-p", prompt}
	}
	return []string{"-p", "--", prompt}
}

// resumeVerb reopens a session by id.
func resumeVerb(provider, id string) []string {
	switch provider {
	case "codex":
		return []string{"resume", id}
	case "agy":
		return []string{"--conversation", id}
	case "copilot":
		return []string{"--resume", id}
	}
	return []string{"--resume", id}
}

// firstPromptArgs hands an interactive session its first prompt.
func firstPromptArgs(provider, prompt string) []string {
	if provider == "agy" {
		return []string{"--prompt-interactive", prompt}
	}
	if provider == "copilot" {
		return []string{"-i", prompt}
	}
	return []string{prompt}
}

// realHome is the user's own home directory for a provider.
func realHome(provider string) string {
	switch provider {
	case "claude":
		return paths.RealClaudeHome()
	case "codex":
		return paths.RealCodexHome()
	case "agy":
		return paths.RealAgyHome()
	case "copilot":
		return paths.RealCopilotHome()
	}
	return ""
}

// homesDir is the parent of aiq's overlay homes for a provider.
func homesDir(provider string) string {
	switch provider {
	case "claude":
		return paths.ClaudeHomesDir()
	case "codex":
		return paths.CodexHomesDir()
	case "agy":
		return paths.AgyHomesDir()
	case "copilot":
		return paths.CopilotHomesDir()
	}
	return ""
}

// defaultRoots is where the transcripts of every provider are looked for.
func defaultRoots() transcript.Roots {
	return transcript.DefaultRoots(
		paths.RealClaudeHome(), paths.RealCodexHome(), agy.AppDataDir(paths.RealAgyHome()), paths.RealCopilotHome(),
		paths.ClaudeHomesDir(), paths.CodexHomesDir(), paths.CopilotHomesDir())
}

// prepareLong readies a CLI for a supervised session where it cannot be
// done with a flag: Antigravity reads its hooks from a file, and asks
// whether to trust a workspace before it starts, which nobody would answer
// in an unattended pane.
func prepareLong(provider, workspace string) {
	if provider != "agy" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	geminiDir := paths.RealAgyHome()
	if changed, err := agy.InstallHooks(geminiDir, self); err != nil {
		fmt.Fprintln(os.Stderr, "aiq: cannot install agy hooks:", err)
	} else if changed {
		fmt.Fprintf(os.Stderr, "aiq: long-session hooks installed in %s (they act only under aiq long)\n", agy.HooksPath(geminiDir))
	}
	if changed, err := agy.TrustWorkspace(geminiDir, workspace); err != nil {
		fmt.Fprintln(os.Stderr, "aiq: cannot trust the workspace for agy:", err)
	} else if changed {
		fmt.Fprintf(os.Stderr, "aiq: %s added to agy's trusted workspaces\n", workspace)
	}
}
