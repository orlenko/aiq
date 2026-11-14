package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

// aiq long <provider> [--account <name>] [--model-tier N] [--effort N] [--] [args...] starts or attaches.
// aiq long auto [--model-tier N] [--effort N]   the same across every pool
// aiq long auto resume [--account <name>] [<id>]  resume a session here, long
// aiq long resume [--account <name>] [<id>]       the same, as the session ran
// aiq long list                   long sessions and their drain state
// aiq long drain <lease-id|.>     ask a session to wrap up and move now
// aiq long attach [.|<id> [--started <unix>]] attach the workspace or an exact lease
// aiq long stop <lease-id|.>      end the session and its tmux session
func cmdLong(args []string) error {
	explicitBackend := len(args) > 0 && (args[0] == "--backend" || strings.HasPrefix(args[0], "--backend="))
	backend, args, err := longBackendArgs(args)
	if err != nil {
		return configErr("bad-flags", "%v", err)
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: aiq long %s|<launcher> [--account <name>] [--model-tier N] [--effort N] [--] [args...] | auto [--model-tier N] [--effort N] | [auto] resume [--account <name>] [<id>] | list | drain <id|.> | attach [.|<id> [--started <unix-seconds>]] | stop <id|.>", config.ProviderList("|"))
	}
	if args[0] == "attach" {
		o, err := parseLongAttachArgs(args[1:], explicitBackend)
		if err != nil {
			return configErr("bad-flags", "%v", err)
		}
		if !o.legacy {
			return runLongAttach(o, os.Stdout)
		}
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	a.longBackend = backend
	switch args[0] {
	case "claude", "codex", "agy":
		return a.longStart(args[0], "", args[1:])
	case "auto":
		if len(args) > 1 && args[1] == "resume" {
			return a.longAutoResume(args[2:])
		}
		return a.longStart("auto", "", args[1:])
	case "resume":
		return a.longResume(args[1:], false)
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
		if err := longLeaseStore(a.st, l).RequestDrainByUser(l.ID, time.Now()); err != nil {
			return err
		}
		a.st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: drain requested by user", l.ID), time.Now())
		fmt.Printf("lease %d (%s in %s): wrap-up requested; the daemon moves it at the next safe point\n", l.ID, l.AccountID, l.Pane)
		return nil
	case "attach":
		return a.attachLongWorkspace(workspaceID())
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: aiq long stop <lease-id|.>")
		}
		l, err := a.longFind(args[1])
		if err != nil {
			if args[1] == "." {
				if r, owned, e := herdr.Workspace(workspaceID()); owned {
					if e != nil {
						return e
					}
					return herdr.Stop(r.Target(), r.PID)
				}
			}
			return err
		}
		if herdr.IsRef(l.Pane) {
			if err := herdr.Stop(l.Pane, l.PID); err != nil {
				return err
			}
			a.st.ForLease(l, 0).ReleaseLease(l.ID)
			a.st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: stopped by user", l.ID), time.Now())
			return nil
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
		return state.Lease{}, fmt.Errorf("no long session for %s", oneLine(ws))
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
	account, flags, args, err := parseLongArgs(args)
	if err != nil {
		return configErr("bad-flags", "%v", err)
	}
	if provider == "auto" {
		args = append(flags, args...)
		// Fail here, not in a tmux pane that closes before it can be read.
		if account != "" {
			return configErr("bad-flags", "auto chooses the account; pick a provider to force one")
		}
		if _, _, err := parseAutoFlags(append([]string{"--long"}, args...)); err != nil {
			return configErr("bad-flags", "%v", err)
		}
	}
	if account != "" && provider != "auto" {
		// Fail here, not in a tmux pane that closes before it can be read.
		if account, err = a.longAccountName(provider, account); err != nil {
			return err
		}
	}
	ws := workspaceID()
	target, attached, err := a.longExisting(ws)
	defer target.unlock()
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
		cmdArgs = append(cmdArgs, flags...)
		cmdArgs = append(append(cmdArgs, "--"), args...)
	}
	what := provider
	if launcher != "" {
		what = launcher + " (" + provider + ")"
	}
	return a.longLaunch(target, ws, ws, what, launcher, cmdArgs)
}

// longAccountName resolves a forced --account for provider. The value is a
// bare name (claude3) or an id (claude/claude3); the name is what `aiq run`
// takes. A forced account only decides where the session starts: the
// supervisor still moves it on before the account runs dry.
func (a *app) longAccountName(provider, ref string) (string, error) {
	name := ref
	if p, n, ok := strings.Cut(ref, "/"); ok {
		if p != provider {
			return "", configErr("account-not-found", "account %s is a %s account; this session runs on %s", ref, p, provider)
		}
		name = n
	}
	if err := a.checkLongAccount(provider, name); err != nil {
		return "", err
	}
	return name, nil
}

// checkLongAccount rejects a forced account that does not exist or is
// disabled, naming the provider's accounts so a typo is easy to fix.
func (a *app) checkLongAccount(provider, name string) error {
	acc, err := a.st.GetAccount(state.AccountID(provider, name))
	if err == nil {
		if !acc.Enabled {
			return configErr("account-disabled", "account %s is disabled", acc.ID)
		}
		return nil
	}
	if !errors.Is(err, state.ErrNotFound) {
		return err
	}
	accs, _ := a.st.ListAccounts(provider)
	var names []string
	for _, x := range accs {
		n := x.Name
		if x.Identity != "" {
			n += " (" + x.Identity + ")"
		}
		names = append(names, n)
	}
	if len(names) == 0 {
		return configErr("account-not-found", "account %s not found; no %s accounts are registered", state.AccountID(provider, name), provider)
	}
	return configErr("account-not-found", "account %s not found; %s accounts: %s", state.AccountID(provider, name), provider, strings.Join(names, ", "))
}

// longTarget is where a workspace's long session starts: a new tmux
// session, or, with pane set, that pane of an existing one, in place.
type longTarget struct {
	native  bool
	core    herdr.Core
	release func()
	name    string // tmux session
	pane    string
	inside  bool // this command runs in a pane of the session
}

// longExisting returns where workspace ws's long session starts, attaching
// to one already running there instead. A stale tmux session with no lease
// is removed, or reused when the user has panes of their own in it.
func (a *app) longExistingTmux(ws string, release func()) (t longTarget, attached bool, err error) {
	t.release = release
	if !tmux.Available() {
		return t, false, fmt.Errorf("tmux is required for long sessions (not found on PATH)")
	}
	t.name = longrun.SessionName(a.cfg.Long.TmuxPrefix, ws)
	if !tmux.HasSession(t.name) {
		return t, false, nil
	}
	panes, err := tmux.SessionPanes(t.name)
	if err != nil {
		return t, false, err
	}
	self := os.Getenv("TMUX_PANE")
	t.inside = self != "" && slices.ContainsFunc(panes, func(p tmux.Pane) bool { return p.ID == self })
	leases, _ := a.longLeases()
	for _, l := range leases {
		if l.Workspace != ws {
			continue
		}
		// The CLI exited and the daemon has not pruned the lease yet: the
		// session is over, and attaching to it would only show a dead pane.
		if l.Hostname == pool.Hostname() && !proc.Alive(l.PID) {
			a.st.ReleaseLease(l.ID)
			continue
		}
		if t.inside && l.Pane != "" {
			longNote(os.Stderr, "aiq: long session already running for %s on %s (lease %d) in pane %s of this tmux session", ws, l.AccountID, l.ID, l.Pane)
			t.unlock()
			return t, true, tmux.SelectPane(l.Pane)
		}
		longNote(os.Stderr, "aiq: long session already running for %s on %s (lease %d); attaching", ws, l.AccountID, l.ID)
		t.unlock()
		return t, true, tmux.Attach(t.name)
	}
	if t.pane, err = placeLongSession(t.name, panes, self); err != nil || t.pane != "" {
		return t, false, err
	}
	tmux.Kill(t.name)
	return t, false, nil
}

// placeLongSession decides where a long session starts in tmux session
// name, which no live lease holds: in one of its panes, in place, or, when
// it returns "", in a new session that replaces this one, aiq's own
// leftover. It never kills a pane the user put there: killing the session
// would take that pane down too, and a takeover that failed leaves exactly
// this state (dead aiq pane, no lease) beside a script that is still
// running. Run from a pane of the session (self), it starts there or in a
// dead aiq pane beside it, so a long session can be started again from the
// pane split off beside one whose CLI exited.
func placeLongSession(name string, panes []tmux.Pane, self string) (string, error) {
	var foreign, deadAiq, liveAiq []string
	inside := false
	for _, p := range panes {
		switch {
		case self != "" && p.ID == self:
			inside = true
			if aiqLongPane(p) {
				liveAiq = append(liveAiq, p.ID)
			}
		case p.Dead:
			if aiqLongPane(p) {
				deadAiq = append(deadAiq, p.ID)
			}
		case aiqLongPane(p):
			liveAiq = append(liveAiq, p.ID)
		default:
			foreign = append(foreign, p.ID)
		}
	}
	switch {
	case len(liveAiq) > 0:
		return "", fmt.Errorf(
			"pane %s of tmux session %s runs an aiq long session that no lease holds yet; the daemon takes it back within a minute. Use that pane rather than start a second session",
			liveAiq[0], name)
	case len(deadAiq) > 0 && (inside || len(foreign) > 0):
		return deadAiq[0], nil
	case inside:
		return self, nil
	case len(foreign) > 0:
		return "", fmt.Errorf(
			"tmux session %s has no aiq lease but holds panes aiq did not start (%s); attach and close it yourself rather than have aiq kill them (tmux attach -t %s)",
			name, strings.Join(foreign, ", "), name)
	}
	return "", nil
}

// aiqLongPane reports whether aiq started the pane: a long session's pane,
// and every respawn of it, runs `aiq run <provider> --long ...`. A pane the
// user split off runs their shell and has no start command.
func aiqLongPane(p tmux.Pane) bool {
	return strings.Contains(p.StartCommand, " run ") && strings.Contains(p.StartCommand, " --long")
}

// longLaunch starts cmdArgs in dir, in t's pane or a new tmux session, and
// attaches.
func (a *app) longLaunch(t longTarget, ws, dir, what, launcher string, cmdArgs []string) error {
	if t.native {
		return a.longLaunchHerdr(t, ws, dir, what, cmdArgs)
	}
	if t.pane != "" {
		return a.longLaunchInPlace(t, ws, dir, what, launcher, cmdArgs)
	}
	pane, err := tmux.NewSession(t.name, dir, tmux.Quote(cmdArgs))
	if err != nil {
		return err
	}
	longNote(os.Stderr, "aiq: long %s session started in tmux %s (pane %s) for %s", what, t.name, pane, ws)
	if launcher != "" {
		fmt.Fprintf(os.Stderr, "aiq: takeover order: %s\n", strings.Join(a.launcherFallbackProviders(launcher), " → "))
	}
	t.unlock()
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return tmux.Attach(t.name)
	}
	fmt.Println(attachHint(t.name))
	return nil
}

// longNote prints one line about a long session. The workspace and the
// tmux name come from a directory name, so the line goes out through
// oneLine: no control character, and no line break of its own.
func longNote(w io.Writer, format string, args ...any) {
	fmt.Fprintln(w, oneLine(fmt.Sprintf(format, args...)))
}

// attachHint says how to attach to tmux session name. The tmux command is
// shown only when the name needs no quoting in any shell and prints as
// itself, so a pasted copy reaches the session whatever shell it goes into;
// aiq long attach always does.
func attachHint(name string) string {
	if tmux.QuoteWord(name) == name && oneLine(name) == name {
		return "attach with: tmux attach -t " + name + "   (or: aiq long attach)"
	}
	return "attach with: aiq long attach"
}

// longLaunchInPlace starts cmdArgs in t's pane of an existing session. In
// the pane this command runs in, that replaces the shell, and this process
// with it, so everything is said first.
func (a *app) longLaunchInPlace(t longTarget, ws, dir, what, launcher string, cmdArgs []string) error {
	where := "pane " + t.pane
	if t.pane == os.Getenv("TMUX_PANE") {
		where = "this pane (" + t.pane + ")"
	}
	longNote(os.Stderr, "aiq: long %s session starting in tmux %s, %s, for %s", what, t.name, where, ws)
	if launcher != "" {
		fmt.Fprintf(os.Stderr, "aiq: takeover order: %s\n", strings.Join(a.launcherFallbackProviders(launcher), " → "))
	}
	// In self-respawn tmux kills this caller, closing the lock only after
	// accepting the serialized respawn. Otherwise release after the command.
	err := tmux.RespawnLong(t.pane, dir, tmux.Quote(cmdArgs))
	t.unlock()
	if err != nil || t.inside {
		return err
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return tmux.Attach(t.name)
	}
	fmt.Println(attachHint(t.name))
	return nil
}

// resumeUsageFor is the usage of a resume under run or long. Auto resumes
// with the permission bypass and tier 1, as auto starts; a plain long
// resume reopens the session as it ran, as aiq resume does.
func resumeUsageFor(verb string, auto bool) string {
	cmd, how := "aiq "+verb+" resume", "with the permission bypass only if it ran with one"
	if auto {
		cmd = "aiq " + verb + " auto resume"
		how = "with the permission bypass and the model tier (default 1) auto uses"
	}
	where, moves := "in this terminal", ""
	if verb == "long" {
		where, moves = "as a long session", "; the supervisor still moves the session on before it runs dry"
	}
	return "usage: " + cmd + ` [--account <name>] [--model-tier 0..3] [--effort 1..6] [--all] [--launcher <name> | --bare] [<session-id>] [-- extra CLI args]
  no id: browse this directory's sessions and pick one with r
  resumes the pick ` + where + ` on a routed account of its provider,
  ` + how + `
  --account starts on that account of the session's provider instead of the
  routed pick` + moves
}

// splitResumeArgs splits a run or long resume command line: aiq's own
// --account, the model flags `aiq run` translates, and the rest, which is
// the session selection `aiq resume` parses.
func splitResumeArgs(args []string) (account string, tierArgs, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case arg == "--account":
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return "", nil, nil, fmt.Errorf("--account requires an account name")
			}
			i++
			account = args[i]
		case strings.HasPrefix(arg, "--account="):
			if account = strings.TrimPrefix(arg, "--account="); account == "" || strings.HasPrefix(account, "-") {
				return "", nil, nil, fmt.Errorf("--account requires an account name")
			}
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
	return account, tierArgs, rest, nil
}

// resumeRequest is a parsed run or long resume command line.
type resumeRequest struct {
	verb     string
	auto     bool // auto's bypass and default tier
	account  string
	tierArgs []string
	f        runFlags
	o        resumeOpts
}

// parseResume checks a resume command line before anything starts, so a
// bad flag fails here and not in a tmux pane that closes before it can be
// read.
func (a *app) parseResume(verb string, auto bool, args []string) (resumeRequest, error) {
	r := resumeRequest{auto: auto, verb: verb}
	usage := resumeUsageFor(verb, auto)
	// Bypass runs on the real home and ignores routing and --account, and a
	// long session never gets its lease, so an auto or long resume refuses
	// it; plain aiq resume is the one that honours it.
	if (auto || verb == "long") && os.Getenv("AIQ_BYPASS") == "1" {
		return r, configErr("bad-flags", "aiq %s requires routing; unset AIQ_BYPASS or resume with: aiq resume", verb)
	}
	var rest []string
	var err error
	if r.account, r.tierArgs, rest, err = splitResumeArgs(args); err != nil {
		return r, configErr("bad-flags", "%v\n%s", err, usage)
	}
	// An id names its provider, so a typo fails before the browser opens.
	if p, _, ok := strings.Cut(r.account, "/"); ok {
		if _, err := a.longAccountName(p, r.account); err != nil {
			return r, err
		}
	}
	if r.o, err = parseResumeArgs(rest); err != nil {
		return r, configErr("bad-flags", "%v", strings.Replace(err.Error(), resumeUsage, usage, 1))
	}
	if r.o.print {
		return r, configErr("bad-flags", "--print lists without resuming; use: aiq resume --print")
	}
	if r.f = parseRunFlags(r.tierArgs); r.f.flagsErr != nil {
		return r, configErr("bad-flags", "%v", r.f.flagsErr)
	}
	if auto && r.f.modelTier == nil {
		tier := 1
		r.f.modelTier = &tier
		r.tierArgs = append(r.tierArgs, "--model-tier", "1")
	}
	if r.o.launcher != "" {
		if _, ok := a.cfg.Launcher(r.o.launcher); !ok {
			return r, configErr("no-launcher", "no launcher %q; see: aiq launcher list", r.o.launcher)
		}
	}
	return r, nil
}

// resumePick is the session a resume reopens, and how.
type resumePick struct {
	changedDir bool
	workspace  string
	s          transcript.Session
	dir        string
	launcher   string
	why        string
	forced     string   // account name; "" routes
	cliArgs    []string // the bypass and the extra CLI args, without the resume verb
}

// pickResume finds the session to resume in this directory, by id or in
// the browser. done reports that nothing is left to start: the browser was
// closed, or the session already runs in aiq long and was attached to.
func (a *app) pickResume(r resumeRequest) (p resumePick, done bool, err error) {
	if p.dir, err = os.Getwd(); err != nil {
		return p, false, err
	}
	sessions, unreadable, err := transcript.List(defaultRoots(), p.dir)
	if err != nil {
		reportUnreadable(os.Stderr, unreadable)
		return p, false, err
	}
	if r.o.id == "" {
		reportUnreadable(os.Stderr, unreadable)
	}
	if r.o.id != "" {
		selected, e := resolveResumeSelection(a.st, defaultRoots(), sessions, unreadable, r.o.id)
		if e != nil {
			var load *convoLoadError
			path := ""
			if errors.As(e, &load) {
				path = load.path
			}
			reportResumeUnreadable(os.Stderr, unreadable, path)
			return p, false, e
		}
		reportResumeUnreadable(os.Stderr, unreadable, selected.s.Path)
		p.s = selected.s
		if selected.global || selected.forced != "" {
			if p.forced, err = a.resumeCopyAccount(p.s.Provider, selected.forced, r.account); err != nil {
				return p, false, err
			}
		}
		if selected.global {
			p.dir, p.changedDir, err = enterResumeDirectory(resumeDirectoryRuntime(), p.s.Cwd, p.s.ID)
			if err != nil {
				return p, false, err
			}
			sessions = append(sessions, p.s)
			resumeGlobalEcho(os.Stderr, p.s, p.dir, p.forced)
		}
	}
	live := liveSessions(a.st, paths.RealClaudeHome(), a.cfg.Long.TmuxPrefix)
	origins := loadOrigins(a.st, hostname(), os.Getenv("PWD"), p.dir, sessions)
	o := r.o
	switch {
	case o.id != "":
		// Exact global lookup and directory admission precede live decisions.
	case !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())):
		return p, false, configErr("bad-flags", "no terminal to browse sessions on; give a session id (aiq resume --print lists them)")
	default:
		b := newBrowser(p.dir, sessions, live, o.all)
		b.origins = origins
		pick, err := b.run()
		if err != nil || pick == nil {
			return p, true, err
		}
		if b.bare {
			o.bare, o.launcher = true, ""
		}
		p.s = *pick
	}
	s := p.s
	if err := checkSessionID(s.ID); err != nil {
		return p, false, err
	}

	org := originOf(origins, s)
	if p.launcher, p.why, err = resumeLauncher(a.cfg, s, org, o); err != nil {
		return p, false, err
	}
	// The pick fixes the provider, so a forced account can be checked now.
	if r.account != "" && p.forced == "" {
		if p.forced, err = a.longAccountName(s.Provider, r.account); err != nil {
			return p, false, err
		}
	}
	switch {
	// Auto always bypasses permissions. Through a launcher, which may add
	// the flag itself, the bypass is added only if it was passed before.
	case r.auto && (p.launcher == "" || resumeBypass(s, org, p.launcher)):
		p.cliArgs = append(p.cliArgs, autoRequest{}.args(s.Provider)...)
	case !r.auto && resumeBypass(s, org, p.launcher):
		p.cliArgs = append(p.cliArgs, longrun.BypassFlag[s.Provider])
	}
	p.cliArgs = append(p.cliArgs, o.extra...)
	f := r.f
	f.rest = p.cliArgs
	if _, err := modelFlags(s.Provider, f); err != nil {
		return p, false, configErr("bad-flags", "%v", err)
	}
	if p.changedDir && (r.verb == "long" || p.launcher != "") {
		p.workspace = workspaceID()
		if err := checkResumeWorkspace(resumeDirectoryRuntime(), p.dir, p.workspace, s.ID, true); err != nil {
			return p, false, err
		}
	}
	note := live[s.ID]
	if note.native != "" {
		err := herdr.Attach(note.native)
		if !errors.Is(err, herdr.ErrEnded) {
			return p, true, err
		}
	}
	if note.tmux != "" && tmux.HasSession(note.tmux) {
		resumeNote(os.Stderr, "aiq: session %s is %s; attaching instead of starting a second copy\n", s.ID, note.note)
		return p, true, tmux.Attach(note.tmux)
	}
	if note.pid > 0 {
		resumeNote(os.Stderr, "aiq: note: session %s is also %s\n", s.ID, note.note)
	}
	return p, false, nil
}

// runAutoResume picks a session in this directory and reopens it in this
// terminal, as longAutoResume reopens it in tmux.
func (a *app) runAutoResume(args []string) error {
	provider, run, err := a.runAutoResumeArgs(args)
	if err != nil || provider == "" {
		return err
	}
	a.close()
	return cmdRun(provider, run)
}

// runAutoResumeArgs is the `aiq run` command line runAutoResume hands on,
// with no provider when nothing is left to start.
func (a *app) runAutoResumeArgs(args []string) (string, []string, error) {
	r, err := a.parseResume("run", true, args)
	if err != nil {
		return "", nil, err
	}
	p, done, err := a.pickResume(r)
	if done || err != nil {
		return "", nil, err
	}
	run := []string{"--mode", state.ModeInteractive}
	if p.launcher != "" {
		run = append(run, "--launcher", p.launcher)
	}
	if p.forced != "" {
		run = append(run, "--account", p.forced)
	}
	run = append(run, r.tierArgs...)
	run = append(append(run, "--"), p.cliArgs...)
	verb, err := resumeVerb(p.s.Provider, p.s.ID)
	if err != nil {
		return "", nil, err
	}
	run = append(run, verb...)
	fmt.Fprintf(os.Stderr, "aiq: resuming %s session %s\n", p.s.Provider, p.s.ID)
	if p.why != "" {
		fmt.Fprintf(os.Stderr, "aiq: %s\n", p.why)
	}
	return p.s.Provider, run, nil
}

// longAutoResume picks a session in this directory and reopens it as a
// supervised long session. The transcript fixes the provider; the tier and
// bypass follow auto, and so does the account unless --account names one.
func (a *app) longAutoResume(args []string) error { return a.longResume(args, true) }

// longResume reopens a session as a supervised long session: with auto's
// tier and bypass when auto is set, otherwise as it ran, as aiq resume
// reopens it in this terminal.
func (a *app) longResume(args []string, auto bool) error {
	r, err := a.parseResume("long", auto, args)
	if err != nil {
		return err
	}
	var ws string
	var target longTarget
	if r.o.id == "" {
		ws = workspaceID()
		var attached bool
		target, attached, err = a.longExisting(ws)
		defer target.unlock()
		if attached || err != nil {
			return err
		}
	}
	p, done, err := a.pickResume(r)
	if done || err != nil {
		return err
	}
	if r.o.id != "" {
		ws = p.workspace
		if ws == "" {
			ws = workspaceID()
		}
		var attached bool
		target, attached, err = a.longExisting(ws)
		defer target.unlock()
		if attached || err != nil {
			return err
		}
	}
	s, launcher := p.s, p.launcher

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
	if p.forced != "" {
		cmdArgs = append(cmdArgs, "--account", p.forced)
	}
	cmdArgs = append(cmdArgs, r.tierArgs...)
	cmdArgs = append(cmdArgs, "--resume-session", s.ID)
	if s.Path != "" {
		cmdArgs = append(cmdArgs, "--resume-transcript", s.Path)
	}
	cmdArgs = append(cmdArgs, "--")
	cmdArgs = append(cmdArgs, p.cliArgs...)

	fmt.Fprintf(os.Stderr, "aiq: resuming %s session %s as a long session\n", s.Provider, s.ID)
	if p.why != "" {
		fmt.Fprintf(os.Stderr, "aiq: %s\n", p.why)
	}
	what := s.Provider
	if launcher != "" {
		what = launcher + " (" + s.Provider + ")"
	}
	// The CLI finds a transcript by the directory it ran in, which may be
	// below the workspace root.
	return a.longLaunch(target, ws, p.dir, what, launcher, cmdArgs)
}

// Like run, long consumes its own leading options and stops at -- or the
// first provider argument. Everything after that boundary belongs to the CLI.
// --model-tier and --effort come back in flags, for `aiq run` to translate.
func parseLongArgs(args []string) (account string, flags, rest []string, err error) {
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "--":
			return account, flags, args[1:], nil
		case arg == "--account":
			if len(args) < 2 || args[1] == "" || strings.HasPrefix(args[1], "-") {
				return "", nil, nil, fmt.Errorf("--account requires an account name")
			}
			account, args = args[1], args[2:]
		case strings.HasPrefix(arg, "--account="):
			account = strings.TrimPrefix(arg, "--account=")
			if account == "" || strings.HasPrefix(account, "-") {
				return "", nil, nil, fmt.Errorf("--account requires an account name")
			}
			args = args[1:]
		case arg == "--model-tier" || arg == "--effort" || strings.HasPrefix(arg, "--model-tier=") || strings.HasPrefix(arg, "--effort="):
			key, value, equals := strings.Cut(arg, "=")
			args = args[1:]
			if !equals {
				if len(args) == 0 {
					return "", nil, nil, fmt.Errorf("%s requires a value", key)
				}
				value, args = args[0], args[1:]
			}
			if f := parseRunFlags([]string{key, value}); f.flagsErr != nil {
				return "", nil, nil, f.flagsErr
			}
			flags = append(flags, key, value)
		default:
			return account, flags, args, nil
		}
	}
	return account, flags, args, nil
}

func (a *app) longList() error {
	leases, err := a.longLeases()
	if err != nil {
		return err
	}
	native, records, err := listNativeOwnership(leases)
	if err != nil {
		return err
	}
	represented := map[string]bool{}
	if len(leases) == 0 && len(records) == 0 {
		fmt.Printf("no long sessions — start one with: aiq long %s [args]\n", config.ProviderList("|"))
		return nil
	}
	whys, _ := a.st.LeaseWhys()
	shown := false
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
		pane := l.Pane
		if herdr.IsRef(l.Pane) {
			view := native[listingFamily(l.Pane)]
			r := view.record
			pane = "herdr"
			if r.Core.Terminal != "" {
				pane += "/" + r.Core.Terminal
			}
			if view.err != nil {
				turn = "unknown"
			} else if r.Target() != l.Pane {
				turn = "stale"
			} else if r.State != "running" {
				turn = r.State
			}
			if r.Target() == l.Pane {
				represented[r.Target()] = true
			}
		}
		fmt.Printf("%-5d %-20s %-12s %-9s %-10s %-6s %-8s %s\n", l.ID, l.AccountID, launcher, pane, drain, turn, fmtAgo(l.StartedAt, now), oneLine(l.Workspace))
		why, ok := whys[state.LeaseRef{ID: l.ID, StartedAt: l.StartedAt}]
		if !ok {
			continue
		}
		shown = true
		for i, line := range strings.Split(state.CleanLine(why), "\n") {
			indent := "      "
			if i > 0 {
				indent += "  "
			}
			fmt.Println(indent + line)
		}
	}
	for _, saved := range records {
		view := native[saved.Ref]
		r := view.record
		if represented[r.Target()] {
			continue
		}
		status := r.State
		if view.err != nil {
			status = "unknown (" + r.State + ")"
		}
		fmt.Printf("%-5s %-20s %-12s %-9s %-10s %-6s %-8s %s\n", "-", "-", "-", "herdr/"+r.Core.Terminal, "-", status, "-", oneLine(r.Workspace))
	}

	// The daemon writes these lines; without it they stand as it left them.
	if held, err := daemon.LockHeld(); shown && err == nil && !held {
		fmt.Println("(the daemon is not running; these may be out of date)")
	}
	return nil
}
