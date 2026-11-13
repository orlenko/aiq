package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/overlay"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/runner"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/tmux"
)

// runFlags are aiq's own flags, taken from before the "--" separator (or
// from the front of the args when no separator is present).
type runFlags struct {
	modelTier      *int
	effort         int
	flagsErr       error
	account        string
	next           bool
	mode           string
	inheritAuthEnv bool
	rest           []string

	// modelScope overrides the binding model-scoped window for this launch
	// ("fable" skips accounts whose Fable weekly cap is dry; "opus" ignores it).
	modelScope string
	// providers limits auto to these pools (--providers claude,codex).
	providers []string
	// wait keeps a worker polling for a free slot instead of refusing.
	wait    time.Duration
	waitErr error // a --wait value that did not parse (reported as bad-flags)

	// launcher names a registered launcher to exec in the CLI's place.
	// Empty means the CLI runs bare, which is what plain `claude`/`codex` do.
	launcher string

	// Long-session flags (set by `aiq long` and by the daemon's takeover).
	long          bool
	takeover      int64
	fallback      string
	resumeSession string
	// resumeTranscript is the transcript of resumeSession, for its lease.
	resumeTranscript string
	sessionName      string
	nudge            string

	// memoryDir points a Claude overlay launch's auto memory at the real
	// ~/.claude/projects folder (see claude.OverlayMemoryDir).
	memoryDir string
}

func parseRunFlags(args []string) runFlags {
	var f runFlags
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--model-tier" || a == "--effort" || strings.HasPrefix(a, "--model-tier=") || strings.HasPrefix(a, "--effort="):
			key, value, equals := strings.Cut(a, "=")
			if !equals && i+1 < len(args) {
				i++
				value = args[i]
			}
			n, err := strconv.Atoi(value)
			if key == "--model-tier" {
				if err != nil || n < 0 || n > 3 {
					f.flagsErr = fmt.Errorf("--model-tier must be 0–3")
				} else {
					f.modelTier = &n
				}
			} else {
				if err != nil || n < 1 || n > 6 {
					f.flagsErr = fmt.Errorf("--effort must be 1–6")
				} else {
					f.effort = n
				}
			}
		case a == "--":
			i++
			f.rest = append(f.rest, args[i:]...)
			return f
		case a == "--account" && i+1 < len(args):
			f.account = args[i+1]
			i++
		case strings.HasPrefix(a, "--account="):
			f.account = strings.TrimPrefix(a, "--account=")
		case a == "--next":
			f.next = true
		case a == "--mode" && i+1 < len(args):
			f.mode = args[i+1]
			i++
		case strings.HasPrefix(a, "--mode="):
			f.mode = strings.TrimPrefix(a, "--mode=")
		case a == "--inherit-auth-env":
			f.inheritAuthEnv = true
		case a == "--model-scope" && i+1 < len(args):
			f.modelScope = args[i+1]
			i++
		case strings.HasPrefix(a, "--model-scope="):
			f.modelScope = strings.TrimPrefix(a, "--model-scope=")
		case a == "--providers" || strings.HasPrefix(a, "--providers="):
			value, equals := strings.CutPrefix(a, "--providers=")
			if !equals {
				if i+1 == len(args) {
					f.flagsErr = fmt.Errorf("--providers requires a list such as claude,codex")
					break
				}
				i++
				value = args[i]
			}
			if list, err := config.ParseProviders(value); err != nil {
				f.flagsErr = fmt.Errorf("--providers: %v", err)
			} else {
				f.providers = list
			}
		case a == "--wait" && i+1 < len(args):
			f.wait, f.waitErr = parseWait(args[i+1])
			i++
		case strings.HasPrefix(a, "--wait="):
			f.wait, f.waitErr = parseWait(strings.TrimPrefix(a, "--wait="))
		case a == "--launcher" && i+1 < len(args):
			f.launcher = args[i+1]
			i++
		case strings.HasPrefix(a, "--launcher="):
			f.launcher = strings.TrimPrefix(a, "--launcher=")
		case a == "--long":
			f.long = true
		case a == "--takeover" && i+1 < len(args):
			f.takeover, _ = strconv.ParseInt(args[i+1], 10, 64)
			i++
		case a == "--fallback" && i+1 < len(args):
			f.fallback = args[i+1]
			i++
		case a == "--resume-session" && i+1 < len(args):
			f.resumeSession = args[i+1]
			if err := checkSessionID(f.resumeSession); err != nil {
				f.flagsErr = err
			}
			i++
		case a == "--resume-transcript" && i+1 < len(args):
			f.resumeTranscript = args[i+1]
			i++
		case a == "--session-name" && i+1 < len(args):
			f.sessionName = args[i+1]
			i++
		case a == "--nudge" && i+1 < len(args):
			f.nudge = args[i+1]
			i++
		default:
			f.rest = append(f.rest, args[i:]...)
			return f
		}
	}
	return f
}

// parseWait accepts a Go duration ("10m", "600s") or a bare number of seconds.
func parseWait(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		if secs < 0 {
			return 0, fmt.Errorf("--wait must not be negative: %q", s)
		}
		return time.Duration(secs * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("--wait wants a duration such as 600, 600s or 10m, got %q", s)
	}
	return d, nil
}

// workspaceID identifies the current project: git root if inside a
// repository, otherwise the normalized working directory.
func workspaceID() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "unknown"
	}
	return filepath.Clean(wd)
}

func envInt(name string) int {
	v, _ := strconv.Atoi(os.Getenv(name))
	return v
}

func cmdRun(provider string, args []string) error {
	// aiq run auto resume is aiq long auto resume, in this terminal.
	if provider == "auto" && len(args) > 0 && args[0] == "resume" {
		a, err := openApp()
		if err != nil {
			return err
		}
		defer a.close()
		return a.runAutoResume(args[1:])
	}
	f := parseRunFlags(args)
	auto := provider == "auto"
	var request autoRequest
	if auto {
		var err error
		f, request, err = parseAutoFlags(args)
		if err != nil {
			return configErr("bad-flags", "%v", err)
		}
	}
	if f.flagsErr != nil {
		return configErr("bad-flags", "%v", f.flagsErr)
	}
	if !auto && !knownProvider(provider) {
		return fmt.Errorf("unknown provider %s", provider)
	}
	if !auto && f.providers != nil {
		return configErr("bad-flags", "--providers applies to auto; %s already names the provider", provider)
	}
	if !auto {
		var err error
		f, err = modelFlags(provider, f)
		if err != nil {
			return configErr("bad-flags", "%v", err)
		}
	}
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()

	// A wrapper on PATH bounced us back into our own exec chain: continue
	// down PATH with the environment already prepared, no routing.
	if _, ok := inheritedChain(provider); ok {
		a.close()
		return a.execProvider(provider, f.rest, os.Environ(), "")
	}

	// CLI self-management never routes.
	passthrough := !auto && providerPassthrough(provider, f.rest)
	if passthrough || os.Getenv("AIQ_BYPASS") == "1" {
		if len(f.rest) > 0 && (f.rest[0] == "login" || f.rest[0] == "logout" || f.rest[0] == "auth") {
			fmt.Fprintf(os.Stderr, "aiq: `%s %s` acts on your real home, not on a pool account; pool logins are `aiq account login <provider>/<name>`\n", provider, f.rest[0])
		}
		a.close()
		return a.execProvider(provider, f.rest, os.Environ(), "")
	}
	if !auto {
		if _, err := a.binary(provider); err != nil {
			return configErr("no-binary", "%v", err)
		}
	}

	mode := f.mode
	if f.long {
		mode = state.ModeLong
	}
	if mode == "" {
		mode = os.Getenv("AIQ_MODE")
	}
	if mode == "" {
		isWorker := (auto && request.print) || (!auto && providerIsWorker(provider, f.rest))
		if isWorker {
			mode = state.ModeWorker
		} else {
			mode = state.ModeInteractive
		}
	}
	if mode != state.ModeWorker && mode != state.ModeInteractive && mode != state.ModeLong {
		return configErr("bad-flags", "--mode must be interactive or worker")
	}
	if auto && ((mode == state.ModeWorker) != request.print || (mode == state.ModeLong && request.hasPrompt)) {
		return configErr("bad-flags", "auto worker mode requires -p; auto interactive and long modes must omit -p")
	}
	if f.modelScope == "" && f.modelTier == nil {
		f.modelScope = os.Getenv("AIQ_MODEL_SCOPE")
	}
	if f.waitErr != nil {
		return configErr("bad-flags", "%v", f.waitErr)
	}
	if f.wait == 0 {
		if w := os.Getenv("AIQ_WAIT"); w != "" {
			d, err := parseWait(w)
			if err != nil {
				return configErr("bad-flags", "AIQ_WAIT: %v", err)
			}
			f.wait = d
		} else if a.cfg.Worker.WaitForSlotSeconds > 0 {
			f.wait = time.Duration(a.cfg.Worker.WaitForSlotSeconds) * time.Second
		}
	}
	if f.wait > 0 && mode != state.ModeWorker {
		f.wait = 0 // interactive launches never block on a slot
	}

	depth := envInt("AIQ_DEPTH")
	if os.Getenv("AIQ_ACCOUNT") != "" {
		depth++ // we are a child of a routed session
	}
	if max := a.cfg.Selection.MaxDepth; max > 0 && depth > max {
		return configErr("max-depth", "refusing nested AI invocation: depth %d exceeds max_depth %d", depth, max)
	}

	workspace := workspaceID()
	tried := map[string]bool{}
	attempts := 0
	deadline := time.Now().Add(f.wait)
	waiting := false
	baseFlags := f
	for {
		f := baseFlags
		attempts++
		var acc state.Account
		var notes []string
		var err error
		if auto {
			acc, notes, err = a.selectAutoAccount(mode, f, tried)
			if err == nil {
				provider = acc.Provider
				f.rest = request.args(provider)
				if mode == state.ModeLong {
					f.fallback = a.autoLongFallback(provider, f)
				}
				f, err = modelFlags(provider, f)
			}
		} else {
			acc, notes, err = a.selectAccount(provider, mode, f, workspace, tried)
		}
		// A worker asked to wait keeps polling for a slot until the deadline,
		// but only when the cause is the worker cap: an exhausted pool does
		// not free up by waiting.
		if err != nil && f.wait > 0 && refusalToken(err) == "at-worker-cap" {
			if time.Now().Before(deadline) {
				if !waiting {
					fmt.Fprintf(os.Stderr, "aiq: every %s account is at its worker cap; waiting up to %s for a slot\n", provider, f.wait.Round(time.Second))
					waiting = true
				}
				time.Sleep(5 * time.Second)
				attempts--
				continue
			}
			err = poolDry("wait-timeout", "no %s worker slot freed within %s", provider, f.wait.Round(time.Second))
		}
		// Workers run inside agent orchestrations; their stderr is only
		// worth a line when something went wrong.
		if mode == state.ModeInteractive || err != nil {
			for _, n := range notes {
				fmt.Fprintln(os.Stderr, "aiq:", n)
			}
		}
		if err != nil {
			return err
		}
		if waiting {
			fmt.Fprintf(os.Stderr, "aiq: slot free, using %s\n", acc.ID)
		}
		tried[acc.ID] = true
		// A login aiq cannot read, the CLI cannot read either, and the launch
		// would fail as logged out. A worker moves to the next account and
		// says so; anything else stops, so the user can fix it rather than
		// silently landing on a less preferred account.
		if why := a.pool.UnreadableLogin(acc, time.Now()); why != "" {
			if mode == state.ModeWorker && f.account == "" {
				fmt.Fprintf(os.Stderr, "aiq: skipped %s: %s\n", acc.ID, why)
				continue
			}
			return configErr("login-unreadable", "%s: %s; then retry, or choose another account with --account", acc.ID, why)
		}
		if err := a.prepareHome(acc); err != nil {
			return err
		}
		if mode == state.ModeLong {
			// Keep native naming flags in the recorded args as well as the
			// executed command. Copilot has no saved-title reader, so a later
			// handoff recovers its inherited name from this launch record.
			f.rest = namedSessionArgs(provider, f.sessionName, f.rest)
		}
		if mode == state.ModeLong && provider == "codex" && f.sessionName != "" && f.resumeSession == "" {
			p, err := a.codexProvider()
			if err != nil {
				return err
			}
			id, err := p.NewNamedThread(acc.Home, acc.Native, workspace, f.sessionName)
			if err != nil {
				fmt.Fprintln(os.Stderr, "aiq: could not preserve session name:", err)
			} else {
				f.resumeSession = id
			}
		}
		env := a.launchEnv(provider, acc, f.inheritAuthEnv, depth)
		if provider == "claude" && f.effort > 0 {
			// An explicit numeric flag takes precedence over inherited effort.
			env = proc.SanitizeEnv(env, "CLAUDE_CODE_EFFORT_LEVEL")
		}
		summary := a.usageSummary(provider, acc.ID)
		if summary != "" {
			summary = " — " + summary
		}
		if auto || mode != state.ModeWorker || attempts > 1 {
			fmt.Fprintf(os.Stderr, "aiq: using %s%s\n", acc.ID, summary)
		}
		if mode == state.ModeLong && f.takeover == 0 {
			if w := a.floorWarning(provider, acc.ID, time.Now()); w != "" {
				fmt.Fprintln(os.Stderr, w)
			}
		}

		now := time.Now()
		lease := state.Lease{
			AccountID: acc.ID, PID: os.Getpid(), Hostname: hostname(), Mode: mode,
			Cwd: cwd(), Depth: depth, ParentAccount: os.Getenv("AIQ_ACCOUNT"),
			RootID: int64(envInt("AIQ_ROOT")), Args: strings.Join(trimArgs(f.rest), " "), StartedAt: now.Unix(),
		}
		if mode == state.ModeLong {
			lease.Workspace, lease.Provider, lease.Pane = workspace, provider, terminal.Current()
			lease.Fallback, lease.TakeoverOf = f.fallback, f.takeover
			lease.Launcher = f.launcher
			// baseFlags, not f: a Codex named thread replaced f's id with a new, empty one.
			lease.SessionID, lease.Transcript = resumedSession(provider, baseFlags)
			rest := f.rest
			if rest == nil {
				rest = []string{}
			}
			if enc, err := json.Marshal(rest); err == nil {
				lease.Args = string(enc) // exact args: replayed on a same-provider takeover, translated on a cross-provider one
			}
		}
		native := mode == state.ModeLong && herdr.IsRef(lease.Pane)
		leaseID, leaseErr := a.st.AddLease(lease)
		if native && leaseErr != nil {
			return fmt.Errorf("register native long lease: %w", leaseErr)
		}
		lease.ID = leaseID
		if native {
			defer func() {
				if st, err := state.Open(paths.StateDB()); err == nil {
					defer st.Close()
					st.ForLease(lease, 0).ReleaseLease(leaseID)
				}
			}()
		}
		if lease.RootID == 0 {
			env = append(env, "AIQ_ROOT="+strconv.FormatInt(leaseID, 10))
		}
		// Record how this session starts, so `aiq resume` can reopen it the
		// same way (through the same launcher) after the lease is gone.
		// aiq writes this itself: a sandboxed CLI may not reach the database.
		userArgs := append([]string{}, f.rest...)
		sessionID := ""
		switch {
		case mode == state.ModeWorker:
		case mode == state.ModeLong && f.resumeSession != "":
			sessionID = f.resumeSession
		default:
			sessionID, f.rest = providerSession(provider, f.rest)
		}
		launchID, launchErr := a.st.AddLaunch(state.Launch{
			StartedAt: now, Hostname: hostname(), Provider: provider, AccountID: acc.ID,
			Launcher: f.launcher, Cwd: lease.Cwd, Mode: mode, SessionID: sessionID, LeaseID: leaseID, Args: userArgs,
		})
		if native && launchErr != nil {
			return fmt.Errorf("record native long launch: %w", launchErr)
		}
		a.st.LogEvent(provider, acc.ID, "launch", mode+" "+lease.Args, now)

		// A launcher asking for `credential = "file"` cuts the CLI off from
		// the system keychain, so the account's credential is written into
		// the overlay home the CLI is about to read.
		if l, ok := a.cfg.Launcher(f.launcher); ok && l.Credential == "file" && provider == "claude" {
			if wrote, err := claude.ExportCredential(acc.Home); err != nil {
				fmt.Fprintln(os.Stderr, "aiq:", err)
			} else if !wrote {
				fmt.Fprintf(os.Stderr, "aiq: no stored credential for %s; if %s asks you to log in, run: aiq account login %s\n", acc.ID, f.launcher, acc.ID)
			}
		}

		l, hasLauncher := memoryLauncher(a.cfg, f.launcher, mode)
		overlayMemory(&f, provider, acc.Native, mode, func() string {
			return launchMemoryDir(l, hasLauncher, claude.WorkingDir(), env)
		})

		if mode == state.ModeLong {
			env = append(env, "AIQ_LONG=1", "AIQ_LEASE="+strconv.FormatInt(leaseID, 10))
			if native {
				generation, _ := strconv.ParseUint(os.Getenv("AIQ_TERMINAL_GENERATION"), 10, 64)
				if err := herdr.Register(lease.Pane, generation, leaseID, launchID, f.fallback); err != nil {
					return err
				}
				env = append(env, "AIQ_TERMINAL_PID="+strconv.Itoa(os.Getpid()))
			}

			self, _ := os.Executable()
			prepareLong(provider, workspace)
			// The account is otherwise invisible from inside the session, and
			// a takeover changes it; the window name shows it in tmux's bar.
			if native {
				herdr.NameFor(lease.Pane, lease.PID, acc.ID)
			} else if pane := os.Getenv("TMUX_PANE"); pane != "" {
				tmux.NameWindow(pane, tmux.SafeName(acc.ID))
			}
			args, err := longArgs(provider, self, f)
			if err != nil {
				return err
			}
			a.close()
			return a.execProvider(provider, args, env, f.launcher)
		}
		if mode == state.ModeInteractive {
			// exec keeps our pid, so the lease follows the CLI process.
			a.close()
			return a.execProvider(provider, f.rest, env, f.launcher)
		}

		res, err := runner.RunWorker(providerCommand(provider, f.rest, env), a.limitPatterns())
		a.st.ReleaseLease(leaseID)
		if err != nil {
			return err
		}
		if res.Code == 0 {
			a.st.TouchSuccess(acc.ID, time.Now().Unix())
			a.close()
			os.Exit(0)
		}
		// A limit rejection is short and early. A long run whose tool output
		// happened to mention a rate limit is not one, and rerunning it could
		// repeat edits it already made.
		limitHit := res.LimitHit && (res.Elapsed < 5*time.Second || res.Total < 16*1024)
		retry := a.cfg.Worker.Retry && limitHit && res.Elapsed.Seconds() <= a.cfg.Worker.RetryMaxSeconds
		if limitHit {
			until := a.pool.ExhaustUntil(acc, "worker rejected: "+strings.TrimSpace(res.Match), time.Hour, time.Now())
			daemon.Notify(a.cfg.Daemon.Listen, acc.ID)
			fmt.Fprintf(os.Stderr, "aiq: %s hit its usage limit (marked exhausted until %s)\n", acc.ID, until.Local().Format("Mon 15:04"))
		}
		if !retry {
			a.st.LogEvent(provider, acc.ID, "exit", fmt.Sprintf("code=%d", res.Code), time.Now())
			a.close()
			os.Exit(res.Code)
		}
		fmt.Fprintln(os.Stderr, "aiq: rerouting to the next account")
	}
}

// refusalToken returns the stable token of an aiq-level refusal ("" for
// other errors).
func refusalToken(err error) string {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.token
	}
	return ""
}

// resumedSession is the session a long launch resumes, and its transcript, for
// the lease. Hooks report both only once a turn starts; until then the
// supervisor would take the session for a fresh one with nothing to lose. The id
// is the caller's --resume-session, captured before a Codex named thread replaces
// it, or a resume in the CLI's own arguments. A transcript that does not check
// out is left off, and the id kept.
func resumedSession(provider string, f runFlags) (id, transcript string) {
	if f.resumeSession == "" {
		return resumeArg(provider, f.rest), ""
	}
	if f.resumeTranscript == "" {
		return f.resumeSession, ""
	}
	p, err := longrun.ResumeTranscript(provider, f.resumeSession, f.resumeTranscript)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aiq: this session's transcript is recorded at its first turn, not now:", err)
		return f.resumeSession, ""
	}
	return f.resumeSession, p
}

// resumeArg is the session a CLI's own arguments resume by id, or "". A new
// session with an id of its choosing (Claude's --session-id) is not a resume.
func resumeArg(provider string, args []string) string {
	switch provider {
	case "codex":
		return codex.Session(args)
	case "agy":
		return agy.Session(args)
	case "claude":
		for _, a := range args {
			if a == "--" {
				break
			}
			if a == "--session-id" || strings.HasPrefix(a, "--session-id=") {
				return ""
			}
		}
		if id, out := claude.Session(args); len(out) == len(args) {
			return id
		}
	}
	return ""
}

// trimArgs shortens an argument list for the lease/event log: long
// arguments are cut, and the whole line is capped so an IDE's injected hook
// flags do not swamp the event table.
// longArgs assembles the CLI argument list for a supervised session: aiq's
// hooks (Antigravity's are global, installed by ensureLongHooks), the
// user's own args, an optional resume, and an optional first prompt (the
// takeover nudge).
func longArgs(provider, aiqBin string, f runFlags) ([]string, error) {
	var out []string
	switch provider {
	case "claude":
		out = append(out, "--settings", claude.LaunchSettings(aiqBin, f.memoryDir))
	case "codex":
		out = append(out, codex.HookArgs(aiqBin)...)
		out = append(out, codex.UnattendedArgs...)
	}
	out = append(out, f.rest...)
	if f.resumeSession != "" {
		verb, err := resumeVerb(provider, f.resumeSession)
		if err != nil {
			return nil, err
		}
		out = append(out, verb...)
	}
	if f.nudge != "" {
		out = append(out, firstPromptArgs(provider, f.nudge)...)
	}
	return out, nil
}

func namedSessionArgs(provider, name string, args []string) []string {
	if name != "" && (provider == "claude" || provider == "copilot") {
		return append([]string{"--name", name}, args...)
	}
	return args
}

func trimArgs(args []string) []string {
	out := make([]string, 0, len(args))
	total := 0
	for _, a := range args {
		if len(a) > 80 {
			a = a[:77] + "..."
		}
		if total+len(a) > 300 {
			out = append(out, "…")
			break
		}
		total += len(a) + 1
		out = append(out, a)
	}
	return out
}

// overlayMemory points an overlay Claude launch's auto memory at the folder
// memoryDir names (claude.OverlayMemoryDir; empty leaves Claude Code's
// default). It is asked only for an overlay Claude launch. A long
// session carries it in longArgs' --settings with aiq's hooks, as Claude Code
// keeps only one --settings; any other launch gets it in its own arguments.
func overlayMemory(f *runFlags, provider string, native bool, mode string, memoryDir func() string) {
	// A flag that moves the session's project makes aiq's folder a guess.
	if provider != "claude" || native || claude.MovesProject(f.rest) {
		return
	}
	f.memoryDir = memoryDir()
	if mode != state.ModeLong {
		var file bool
		// Workers keep stderr for failures, so only an interactive launch
		// gets the note.
		if f.rest, file = claude.WithMemoryDir(f.rest, f.memoryDir); file && mode == state.ModeInteractive {
			fmt.Fprintf(os.Stderr, "aiq: --settings names a file, so aiq left the memory folder to it; unless it sets autoMemoryDirectory to %s, Claude Code asks before every memory write on this account\n", f.memoryDir)
		}
	}
}

// memoryLauncher is the launcher, if any, that execProvider starts this
// mode's CLI through, so the memory folder is judged on what that CLI gets.
func memoryLauncher(cfg *config.Config, name, mode string) (config.Launcher, bool) {
	if mode == state.ModeWorker {
		return config.Launcher{}, false
	}
	return cfg.Launcher(name)
}

// launchMemoryDir is the memory folder for an overlay Claude launch in dir,
// judged on the environment the CLI will get, a launcher's Env included. It
// is empty when the launcher's Args carry a --settings: aiq's would come
// after it and replace it.
func launchMemoryDir(l config.Launcher, hasLauncher bool, dir string, env []string) string {
	if hasLauncher {
		// A wrapper launcher often passes the CLI's own flags after "--",
		// so its args are read whole, as the --settings scan below does.
		if claude.MovesProject(slices.DeleteFunc(slices.Clone(l.Args), func(a string) bool { return a == "--" })) {
			return ""
		}
		for _, arg := range l.Args {
			if arg == "--settings" || strings.HasPrefix(arg, "--settings=") {
				return ""
			}
		}
		env = launcherEnv(env, l)
	}
	return claude.OverlayMemoryDirIn(dir, env)
}

// hostname is the lease's host identity; it must match what the daemon and
// PruneLeases use (a stable machine id), or leases are never reaped.
func hostname() string { return pool.Hostname() }

func cwd() string {
	d, _ := os.Getwd()
	return d
}

func (a *app) limitPatterns() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range a.cfg.LimitPatterns() {
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		} else {
			fmt.Fprintf(os.Stderr, "aiq: bad limit pattern %q: %v\n", p, err)
		}
	}
	return out
}

// selectAccount applies the policy for one launch attempt.
func (a *app) selectAccount(provider, mode string, f runFlags, workspace string, tried map[string]bool) (state.Account, []string, error) {
	affinity, err := a.st.GetAffinity(provider, workspace)
	if err != nil {
		return state.Account{}, nil, err
	}
	cands, err := a.pool.Candidates(provider)
	if err != nil {
		return state.Account{}, nil, err
	}
	if len(cands) == 0 {
		return state.Account{}, nil, configErr("no-accounts", "no %s accounts registered — run: aiq account add %s <name>", provider, provider)
	}
	var filtered []selector.Candidate
	for _, c := range cands {
		if tried[c.ID] {
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return state.Account{}, nil, poolDry("pool-exhausted", "every %s account was tried and rejected", provider)
	}
	policyMode := mode
	if policyMode == state.ModeLong {
		policyMode = state.ModeInteractive
	}
	pol := a.pool.Policy(provider, policyMode, time.Now())
	if mode == state.ModeLong {
		pol.FloorPct = a.cfg.Long.DrainPct
	}
	switch {
	case f.modelTier != nil:
		// The tier names the model, so its scope replaces the one the
		// provider's default model implies, even when it has none.
		pol.ModelScope = f.modelScope
	case strings.EqualFold(f.modelScope, "none"):
		pol.ModelScope = ""
	case f.modelScope != "":
		pol.ModelScope = f.modelScope
	}
	pol.AffinityID = affinity
	if f.account != "" && len(tried) == 0 {
		// --account takes a bare name, and an id (claude/claude3) too, so a
		// value copied from `aiq account list` or the menu works as typed.
		pol.ForceID = state.AccountID(provider, f.account)
		if p, name, ok := strings.Cut(f.account, "/"); ok {
			if p != provider {
				return state.Account{}, nil, configErr("account-not-found", "account %s is a %s account, not %s", f.account, p, provider)
			}
			pol.ForceID = state.AccountID(provider, name)
		}
	}
	if f.next {
		pol.SkipID = affinity
	}
	res, err := selector.Select(pol, filtered)
	if err != nil {
		if f.account != "" {
			return state.Account{}, res.Notes, configErr("account-not-found", "%v", err)
		}
		// Only the cap is worth waiting for; everything else is exhaustion.
		token := "pool-exhausted"
		for _, r := range res.Ranked {
			if strings.HasPrefix(r.Reason, "at worker cap") {
				token = "at-worker-cap"
				break
			}
		}
		return state.Account{}, res.Notes, poolDry(token, "%v", err)
	}
	acc, err := a.st.GetAccount(res.ID)
	if err != nil {
		return state.Account{}, res.Notes, err
	}
	now := time.Now()
	a.st.TouchSelected(acc.ID, now.Unix())
	if mode != state.ModeWorker {
		a.st.SetAffinity(provider, workspace, acc.ID, now)
	}
	return acc, res.Notes, nil
}

// prepareHome syncs the overlay for non-native accounts.
func (a *app) prepareHome(acc state.Account) error {
	if acc.Native {
		return nil
	}
	spec, ok := providerOverlaySpec(acc)
	if !ok {
		return fmt.Errorf("%s: %s accounts run on the real home only in this release", acc.ID, acc.Provider)
	}
	rep, err := overlay.Sync(spec)
	if err != nil {
		return fmt.Errorf("sync %s: %w", acc.Home, err)
	}
	if acc.Provider == "claude" {
		if err := claude.SeedPrivate(acc.Home); err != nil {
			fmt.Fprintln(os.Stderr, "aiq: overlay: seed .claude.json:", err)
		}
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "aiq: overlay:", w)
	}
	var changes []string
	if len(rep.Adopted) > 0 {
		changes = append(changes, "adopted "+strings.Join(rep.Adopted, ", "))
	}
	if len(rep.Merged) > 0 {
		changes = append(changes, "merged "+strings.Join(rep.Merged, ", "))
	}
	if len(rep.Replaced) > 0 {
		changes = append(changes, "kept real copy of "+strings.Join(rep.Replaced, ", ")+" (overlay copy saved as .aiq-bak)")
	}
	if len(changes) > 0 {
		a.st.LogEvent(acc.Provider, acc.ID, "overlay", strings.Join(changes, "; "), time.Now())
	}
	overlay.Touch(acc.Home)
	return nil
}

func (a *app) launchEnv(provider string, acc state.Account, inheritAuthEnv bool, depth int) []string {
	env := a.providerEnv(provider, acc, inheritAuthEnv)
	// Scrub the routing metadata of a parent session before setting ours.
	filtered := env[:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "AIQ_PROVIDER", "AIQ_ACCOUNT", "AIQ_DEPTH", "AIQ_MODE", "AIQ_PARENT_PROVIDER", "AIQ_PARENT_ACCOUNT", chainVar:
			continue
		}
		filtered = append(filtered, kv)
	}
	env = filtered
	if parent := os.Getenv("AIQ_ACCOUNT"); parent != "" {
		env = append(env, "AIQ_PARENT_PROVIDER="+os.Getenv("AIQ_PROVIDER"), "AIQ_PARENT_ACCOUNT="+parent)
	}
	return append(env,
		"AIQ_PROVIDER="+provider,
		"AIQ_ACCOUNT="+acc.Name,
		"AIQ_DEPTH="+strconv.Itoa(depth),
	)
}

// floorWarning is what `aiq long` says when it starts on an account at or
// below the drain floor, judged as the supervisor judges it: the session
// will move once another account is free, and one on extra usage once an
// account has plan quota above the floor.
func (a *app) floorWarning(provider, accountID string, now time.Time) string {
	cands, err := a.pool.Candidates(provider)
	if err != nil {
		return ""
	}
	pol := a.pool.Policy(provider, state.ModeInteractive, now)
	pol.FloorPct = a.cfg.Long.DrainPct
	extra := false
	for _, r := range selector.Rank(pol, cands) {
		if r.ID == accountID {
			extra = r.Rung == selector.RungExtra
		}
	}
	for _, c := range cands {
		if c.ID != accountID {
			continue
		}
		// Headroom is 100 when no window is known, which is never at the floor.
		h, _ := selector.Headroom(c.Windows, pol.ModelScope, now)
		switch {
		case h > pol.FloorPct:
		case extra:
			return fmt.Sprintf("aiq: %s has %.0f%% plan quota left and runs on extra usage: aiq moves this session to an account with more than %.0f%% plan quota once one is free, after a handoff note if it has started work", accountID, h, pol.FloorPct)
		default:
			return fmt.Sprintf("aiq: %s has %.0f%% left, at or below the %.0f%% drain floor: aiq moves this session to an account with more once one is free, after a handoff note if it has started work", accountID, h, pol.FloorPct)
		}
	}
	return ""
}

// usageSummary renders "5h 31%, weekly 48%, fable 52%" from stored windows:
// the plan-wide windows plus any binding model-scoped cap.
func (a *app) usageSummary(provider, accountID string) string {
	ws, err := a.st.ListWindows(accountID)
	if err != nil {
		return ""
	}
	scope := strings.ToLower(a.pool.ModelScope(provider))
	var parts []string
	spent := false
	for _, w := range ws {
		if w.UsedPct < 0 || (w.Kind != state.KindShort && w.Kind != state.KindWeekly) {
			continue
		}
		if w.Scope != "" && (scope == "" || !strings.Contains(strings.ToLower(w.Scope), scope)) {
			continue
		}
		label := strings.ToLower(w.Label)
		if label == "session" {
			label = "5h"
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", label, w.UsedPct))
		spent = spent || w.UsedPct >= 100 || w.Severity == "critical"
	}
	if u, ok, _ := a.st.GetUsage(accountID); ok && spent && a.cfg.Selection.ExtraUsage {
		if ok, _ := u.Extra.Spendable(time.Now()); ok {
			parts = append(parts, "on extra usage, "+u.Extra.Money(u.Extra.BalanceMinor)+" prepaid left")
		}
	}
	return strings.Join(parts, ", ")
}
