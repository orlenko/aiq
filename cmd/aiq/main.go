// Command aiq routes every claude and codex invocation to the pooled
// subscription account that is about to waste the most quota, and runs the
// daemon that keeps the pool's telemetry fresh.
package main

import (
	"errors"
	"fmt"
	"os"

	"os/exec"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/state"
)

const version = "0.3.0"

const usage = `aiq %s — quota-aware router for pooled Claude Code and Codex accounts

Launch (what the PATH shims call):
  aiq run claude [--account <name>] [--next] [--mode interactive|worker]
                 [--model-scope <name>] [--wait <duration>] -- [claude args...]
  aiq run codex  [same flags] -- [codex args...]
    exit 75: no eligible account (pool dry, at cap, wait expired); 78: nothing to route to;
    any other code is the child's own
  aiq claude [args...]            same as: aiq run claude -- args
  aiq codex  [args...]

Pool:
  aiq status [--json] [--refresh] [--explain]
  aiq top                          live console view
  aiq account list
  aiq account add <provider> <name>        new overlay home + browser login (+ poll grant for Claude)
  aiq account login <provider>/<name>      (re)authenticate the CLI for an account
  aiq account authorize claude/<name>      (re)mint the quota poll grant
  aiq account poll [<provider>/<name>...]  poll now
  aiq account label <provider>/<name> <text>
  aiq account order [<provider>/<name>...]  display order (per provider column)
  aiq account enable|disable|remove <provider>/<name>
  aiq account use <provider>/<name>        pin this workspace
  aiq account next <provider>              rotate this workspace
  aiq account import [id[=name]...]        adopt accounts from aiquota, if you have it
  aiq mark <provider>/<name> exhausted [--until 14:42|+2h|RFC3339]
  aiq mark <provider>/<name> ready
  aiq reset codex/<name>                   consume an earned Codex reset credit

Long-running sessions (supervised, moved between accounts before they run dry):
  aiq long claude|codex [args...]   start in a tmux session named after the workspace, or attach
  aiq long list | attach | drain <lease|.> | stop <lease|.>

Machine:
  aiq shim install|uninstall|path
  aiq statusline install|uninstall|status   Claude status-line multiplexer (live quota feed)
  aiq daemon run|install|uninstall|status
  aiq doctor
`

// Exit codes for aiq-level outcomes, so a launcher can tell a pool refusal
// from a child failure without parsing stderr. Child exit codes pass through.
const (
	ExitPoolDry = 75 // no eligible account right now (exhausted, at cap, waited out)
	ExitConfig  = 78 // nothing to route to: no accounts, no binary, bad flags
)

// exitError carries an aiq-level exit code and a stable refusal token that
// is printed as `aiq: refused: <token>` before the human-readable message.
// Tokens: at-worker-cap, pool-exhausted, wait-timeout, no-accounts,
// bad-flags, max-depth, no-binary, account-not-found.
type exitError struct {
	code  int
	token string
	msg   string
}

func (e *exitError) Error() string { return e.msg }

func poolDry(token, format string, v ...any) error {
	return &exitError{ExitPoolDry, token, fmt.Sprintf(format, v...)}
}

func configErr(token, format string, v ...any) error {
	return &exitError{ExitConfig, token, fmt.Sprintf(format, v...)}
}

// app bundles everything a subcommand needs.
type app struct {
	cfg  *config.Config
	st   *state.Store
	pool *pool.Pool
}

func openApp() (*app, error) {
	if err := paths.EnsureDirs(); err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	st, err := state.Open(paths.StateDB())
	if err != nil {
		return nil, err
	}
	p := &pool.Pool{Cfg: cfg, St: st}
	p.CodexCommand = func(args, env []string) *exec.Cmd { return providerCommand("codex", args, env) }
	return &app{cfg: cfg, st: st, pool: p}, nil
}

func (a *app) close() {
	if a.st != nil {
		a.st.Close()
	}
}

// binary reports which executable a launch would exec first (for doctor).
func (a *app) binary(provider string) (string, error) {
	return a.resolveNext(provider, chain{provider: provider})
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

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, version)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "__launch":
		err = cmdLaunch(args)
	case "run":
		if len(args) == 0 {
			err = fmt.Errorf("usage: aiq run <provider> [flags] -- [args]")
		} else {
			err = cmdRun(args[0], args[1:])
		}
	case "claude", "codex":
		err = cmdRun(cmd, append([]string{"--"}, args...))
	case "status":
		err = cmdStatus(args)
	case "top":
		err = cmdTop(args)
	case "account":
		err = cmdAccount(args)
	case "mark":
		err = cmdMark(args)
	case "reset":
		err = cmdReset(args)
	case "shim":
		err = cmdShim(args)
	case "statusline":
		err = cmdStatusline(args)
	case "daemon":
		err = cmdDaemon(args)
	case "doctor":
		err = cmdDoctor(args)
	case "long":
		err = cmdLong(args)
	case "claude-hook":
		cmdHook("claude", args)
		return
	case "codex-hook":
		cmdHook("codex", args)
		return
	case "claude-statusline":
		// Never break the user's status line: this always exits 0.
		cmdClaudeStatusline()
		return
	case "version", "--version", "-v":
		fmt.Println("aiq", version)
	case "help", "--help", "-h":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, "aiq: unknown command %q\n\n", cmd)
		fmt.Printf(usage, version)
		os.Exit(2)
	}
	if err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			fmt.Fprintf(os.Stderr, "aiq: refused: %s\n", ee.token)
			fmt.Fprintln(os.Stderr, "aiq:", err)
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "aiq:", err)
		os.Exit(1)
	}
}
