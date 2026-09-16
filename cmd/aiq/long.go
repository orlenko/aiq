package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

// aiq long <provider> [--account <name>] [--] [args...] starts or attaches.
// aiq long auto [--model-tier N] [--effort N]   the same across both pools
// aiq long auto resume [<id>]     pick a session in this directory, resume it long
// aiq long list                   long sessions and their drain state
// aiq long drain <lease-id|.>     ask a session to wrap up and move now
// aiq long attach [.]             attach to this workspace's session
// aiq long stop <lease-id|.>      end the session and its tmux session
func cmdLong(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq long claude|codex|<launcher> [--account <name>] [--] [args...] | auto [--model-tier N] [--effort N] | auto resume [<id>] | list | drain <id|.> | attach [.] | stop <id|.>")
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	switch args[0] {
	case "claude", "codex":
		return a.longStart(args[0], "", args[1:])
	case "auto":
		if len(args) > 1 && args[1] == "resume" {
			return a.longAutoResume(args[2:])
		}
		return a.longStart("auto", "", args[1:])
	case "list":
		return a.longList()
	case "drain":
		if len(args) < 2 {
			return fmt.Errorf("usage: aiq long drain <lease-id|.>")
		}
		l, err := a.longFind(args[1])
		if err != nil {
			return err
		}
		longrun.ArchiveNote(l.Workspace, time.Now())
		if err := a.st.SetLeaseDrain(l.ID, state.DrainRequested, time.Now()); err != nil {
			return err
		}
		a.st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: drain requested by user", l.ID), time.Now())
		fmt.Printf("lease %d (%s in %s): wrap-up requested; the daemon moves it at the next safe point\n", l.ID, l.AccountID, l.Pane)
		return nil
	case "attach":
		ws := workspaceID()
		name := longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
		if !tmux.HasSession(name) {
			return fmt.Errorf("no long session for %s (tmux session %s)", ws, name)
		}
		return tmux.Attach(name)
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: aiq long stop <lease-id|.>")
		}
		l, err := a.longFind(args[1])
		if err != nil {
			return err
		}
		name := longrun.SessionName(a.cfg.Long.TmuxPrefix, l.Workspace)
		a.st.ReleaseLease(l.ID)
		a.st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: stopped by user", l.ID), time.Now())
		if tmux.HasSession(name) {
			return tmux.Kill(name)
		}
		return nil
	}
	// A registered launcher supervises the same way, started under its own
	// name: `aiq long <launcher> [args...]`.
	if l, ok := a.cfg.Launcher(args[0]); ok {
		return a.longStart(l.Provider, args[0], args[1:])
	}
	return fmt.Errorf("unknown long subcommand %q (not a provider or a registered launcher)", args[0])
}

func (a *app) longLeases() ([]state.Lease, error) {
	leases, err := a.st.ListLeases()
	if err != nil {
		return nil, err
	}
	var out []state.Lease
	for _, l := range leases {
		if l.Mode == state.ModeLong {
			out = append(out, l)
		}
	}
	return out, nil
}

// longFind resolves "." (this workspace) or a lease id.
func (a *app) longFind(ref string) (state.Lease, error) {
	leases, err := a.longLeases()
	if err != nil {
		return state.Lease{}, err
	}
	if ref == "." {
		ws := workspaceID()
		for _, l := range leases {
			if l.Workspace == ws {
				return l, nil
			}
		}
		return state.Lease{}, fmt.Errorf("no long session for %s", ws)
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		return state.Lease{}, fmt.Errorf("lease id or '.' expected, got %q", ref)
	}
	for _, l := range leases {
		if l.ID == id {
			return l, nil
		}
	}
	return state.Lease{}, fmt.Errorf("no long lease %d", id)
}

// launcherFallbackProviders is the provider order a launched long session
// may move through: its own provider first, then the provider of each
// launcher in the chain. A provider with no launcher in the chain is left
// out, so a session never silently loses the launcher it asked for.
func (a *app) launcherFallbackProviders(name string) []string {
	l, ok := a.cfg.Launcher(name)
	if !ok {
		return nil
	}
	out := []string{l.Provider}
	seen := map[string]bool{l.Provider: true}
	for _, next := range l.Fallback {
		nl, ok := a.cfg.Launcher(next)
		if !ok || seen[nl.Provider] {
			continue
		}
		seen[nl.Provider] = true
		out = append(out, nl.Provider)
	}
	return out
}

// longFallback is the takeover order of a long session on provider: its own
// provider first, whose successor resumes the transcript, then long.fallback.
func (a *app) longFallback(provider string) string {
	return strings.Join(append([]string{provider}, a.cfg.Long.Fallback...), ",")
}

func (a *app) longStart(provider, launcher string, args []string) error {
	account, args, err := parseLongArgs(args)
	if err != nil {
		return err
	}
	if provider == "auto" {
		// Fail here, not in a tmux pane that closes before it can be read.
		if account != "" {
			return configErr("bad-flags", "auto chooses the account; pick a provider to force one")
		}
		if _, _, err := parseAutoFlags(append([]string{"--long"}, args...)); err != nil {
			return configErr("bad-flags", "%v", err)
		}
	}
	ws := workspaceID()
	name, attached, err := a.longExisting(ws)
	if attached || err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmdArgs := []string{self, "run", provider, "--long"}
	switch {
	case provider == "auto":
		// The provider, and with it the takeover order, is chosen at launch.
		cmdArgs = append(cmdArgs, args...)
	default:
		fallback := a.longFallback(provider)
		if launcher != "" {
			fallback = strings.Join(a.launcherFallbackProviders(launcher), ",")
		}
		cmdArgs = append(cmdArgs, "--fallback", fallback)
		if launcher != "" {
			cmdArgs = append(cmdArgs, "--launcher", launcher)
		}
		if account != "" {
			cmdArgs = append(cmdArgs, "--account", account)
		}
		cmdArgs = append(append(cmdArgs, "--"), args...)
	}
	what := provider
	if launcher != "" {
		what = launcher + " (" + provider + ")"
	}
	return a.longLaunch(name, ws, ws, what, launcher, cmdArgs)
}

// longExisting returns the tmux session name for workspace ws, attaching
// to a long session already running there. A stale tmux session with no
// lease is removed.
func (a *app) longExisting(ws string) (name string, attached bool, err error) {
	if !tmux.Available() {
		return "", false, fmt.Errorf("tmux is required for long sessions (not found on PATH)")
	}
	name = longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
	if tmux.HasSession(name) {
		leases, _ := a.longLeases()
		for _, l := range leases {
			if l.Workspace == ws {
				fmt.Fprintf(os.Stderr, "aiq: long session already running for %s on %s (lease %d); attaching\n", ws, l.AccountID, l.ID)
				return name, true, tmux.Attach(name)
			}
		}
		tmux.Kill(name)
	}
	return name, false, nil
}

// longLaunch starts cmdArgs in tmux session name, in dir, and attaches.
func (a *app) longLaunch(name, ws, dir, what, launcher string, cmdArgs []string) error {
	pane, err := tmux.NewSession(name, dir, tmux.Quote(cmdArgs))
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "aiq: long %s session started in tmux %s (pane %s) for %s\n", what, name, pane, ws)
	if launcher != "" {
		fmt.Fprintf(os.Stderr, "aiq: takeover order: %s\n", strings.Join(a.launcherFallbackProviders(launcher), " → "))
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return tmux.Attach(name)
	}
	fmt.Printf("attach with: tmux attach -t %s   (or: aiq long attach)\n", name)
	return nil
}

const longAutoResumeUsage = `usage: aiq long auto resume [--model-tier 0..3] [--effort 1..6] [--all] [--launcher <name> | --bare] [<session-id>] [-- extra CLI args]
  no id: browse this directory's sessions and pick one with r
  resumes the pick as a long session on a routed account of its provider,
  with the permission bypass and the model tier (default 1) auto uses`

// longAutoResume picks a session in this directory and reopens it as a
// supervised long session. The transcript fixes the provider; the account,
// tier and bypass follow auto.
func (a *app) longAutoResume(args []string) error {
	var tierArgs, rest []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case arg == "--model-tier" || arg == "--effort":
			tierArgs = append(tierArgs, arg)
			if i+1 < len(args) {
				i++
				tierArgs = append(tierArgs, args[i])
			}
		case strings.HasPrefix(arg, "--model-tier=") || strings.HasPrefix(arg, "--effort="):
			tierArgs = append(tierArgs, arg)
		default:
			rest = append(rest, arg)
		}
	}
	o, err := parseResumeArgs(rest)
	if err != nil {
		return configErr("bad-flags", "%v", strings.Replace(err.Error(), resumeUsage, longAutoResumeUsage, 1))
	}
	if o.print {
		return configErr("bad-flags", "--print lists without resuming; use: aiq resume --print")
	}
	f := parseRunFlags(tierArgs)
	if f.flagsErr != nil {
		return configErr("bad-flags", "%v", f.flagsErr)
	}
	if f.modelTier == nil {
		tier := 1
		f.modelTier = &tier
		tierArgs = append(tierArgs, "--model-tier", "1")
	}
	if o.launcher != "" {
		if _, ok := a.cfg.Launcher(o.launcher); !ok {
			return configErr("no-launcher", "no launcher %q; see: aiq launcher list", o.launcher)
		}
	}

	ws := workspaceID()
	name, attached, err := a.longExisting(ws)
	if attached || err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	roots := transcript.DefaultRoots(paths.RealClaudeHome(), paths.RealCodexHome(), paths.ClaudeHomesDir(), paths.CodexHomesDir())
	sessions, err := transcript.List(roots, dir)
	if err != nil {
		return err
	}
	live := a.liveSessions()
	origins := a.loadOrigins(dir, sessions)
	var s transcript.Session
	switch {
	case o.id != "":
		if s, err = findSession(sessions, o.id); err != nil {
			return err
		}
	case !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())):
		return configErr("bad-flags", "no terminal to browse sessions on; give a session id (aiq resume --print lists them)")
	default:
		b := newBrowser(dir, sessions, live, o.all)
		b.origins = origins
		pick, err := b.run()
		if err != nil || pick == nil {
			return err
		}
		if b.bare {
			o.bare, o.launcher = true, ""
		}
		s = *pick
	}

	note := live[s.ID]
	if note.tmux != "" && tmux.HasSession(note.tmux) {
		fmt.Fprintf(os.Stderr, "aiq: session %s is %s; attaching instead of starting a second copy\n", s.ID, note.note)
		return tmux.Attach(note.tmux)
	}
	if note.pid > 0 {
		fmt.Fprintf(os.Stderr, "aiq: note: session %s is also %s\n", s.ID, note.note)
	}
	org := originOf(origins, s)
	launcher, why, err := resumeLauncher(a.cfg, s, org, o)
	if err != nil {
		return err
	}
	// Auto always bypasses permissions. Through a launcher, which may add
	// the flag itself, the bypass is added only if it was passed before.
	var cliArgs []string
	if launcher == "" || resumeBypass(s, org, launcher) {
		cliArgs = append(cliArgs, autoRequest{}.args(s.Provider)...)
	}
	cliArgs = append(cliArgs, o.extra...)
	f.rest = cliArgs
	if _, err := modelFlags(s.Provider, f); err != nil {
		return configErr("bad-flags", "%v", err)
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}
	fallback := a.longFallback(s.Provider)
	if launcher != "" {
		fallback = strings.Join(a.launcherFallbackProviders(launcher), ",")
	}
	cmdArgs := []string{self, "run", s.Provider, "--long", "--fallback", fallback}
	if launcher != "" {
		cmdArgs = append(cmdArgs, "--launcher", launcher)
	}
	cmdArgs = append(cmdArgs, tierArgs...)
	cmdArgs = append(cmdArgs, "--resume-session", s.ID, "--")
	cmdArgs = append(cmdArgs, cliArgs...)

	fmt.Fprintf(os.Stderr, "aiq: resuming %s session %s as a long session\n", s.Provider, s.ID)
	if why != "" {
		fmt.Fprintf(os.Stderr, "aiq: %s\n", why)
	}
	what := s.Provider
	if launcher != "" {
		what = launcher + " (" + s.Provider + ")"
	}
	// The CLI finds a transcript by the directory it ran in, which may be
	// below the workspace root.
	return a.longLaunch(name, ws, dir, what, launcher, cmdArgs)
}

// Like run, long consumes its own leading options and stops at -- or the
// first provider argument. Everything after that boundary belongs to the CLI.
func parseLongArgs(args []string) (account string, rest []string, err error) {
	for len(args) > 0 {
		switch {
		case args[0] == "--":
			return account, args[1:], nil
		case args[0] == "--account":
			if len(args) < 2 || args[1] == "" || strings.HasPrefix(args[1], "-") {
				return "", nil, fmt.Errorf("--account requires an account name")
			}
			account, args = args[1], args[2:]
		case strings.HasPrefix(args[0], "--account="):
			account = strings.TrimPrefix(args[0], "--account=")
			if account == "" || strings.HasPrefix(account, "-") {
				return "", nil, fmt.Errorf("--account requires an account name")
			}
			args = args[1:]
		default:
			return account, args, nil
		}
	}
	return account, args, nil
}

func (a *app) longList() error {
	leases, err := a.longLeases()
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		fmt.Println("no long sessions — start one with: aiq long claude|codex [args]")
		return nil
	}
	now := time.Now()
	fmt.Printf("%-5s %-20s %-12s %-9s %-10s %-6s %-8s %s\n", "LEASE", "ACCOUNT", "LAUNCHER", "PANE", "DRAIN", "TURN", "SINCE", "WORKSPACE")
	for _, l := range leases {
		drain := l.Drain
		if drain == "" {
			drain = "-"
		}
		turn := "idle"
		if l.InTurn() {
			turn = "busy"
		}
		launcher := l.Launcher
		if launcher == "" {
			launcher = "-"
		}
		fmt.Printf("%-5d %-20s %-12s %-9s %-10s %-6s %-8s %s\n", l.ID, l.AccountID, launcher, l.Pane, drain, turn, fmtAgo(l.StartedAt, now), l.Workspace)
	}
	return nil
}
