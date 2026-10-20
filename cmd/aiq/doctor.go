package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/state"
)

// doctorFix is one thing to do about one or more failed checks. Checks that
// share a key share the fix: every problem with one account is sorted out
// in a single sitting, since each sign-in has to be as the same person.
type doctorFix struct {
	key     string
	title   string       // the line in the plan
	command string       // the aiq command(s) that do the same by hand
	steps   []string     // what the person will be asked to do
	run     func() error // nil when only the person can do it
	browser bool         // asks for a browser sign-in
	account bool         // per-account fixes come after the machine's
}

type doctorCheck struct {
	ok   bool
	text string
	fix  *doctorFix
}

type doctorReport struct {
	checks []doctorCheck
	fixes  []*doctorFix
	byKey  map[string]*doctorFix
}

func (r *doctorReport) check(ok bool, fix *doctorFix, format string, v ...any) {
	c := doctorCheck{ok: ok, text: fmt.Sprintf(format, v...)}
	if !ok && fix != nil {
		if seen, dup := r.byKey[fix.key]; dup {
			fix = seen
		} else {
			r.byKey[fix.key] = fix
			r.fixes = append(r.fixes, fix)
		}
		c.fix = fix
	}
	r.checks = append(r.checks, c)
}

func cmdDoctor(args []string) error {
	verbose := hasFlag(args, "-v") || hasFlag(args, "--verbose")
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()

	r := a.doctorChecks()
	r.print(verbose)
	if len(r.fixes) == 0 {
		return nil
	}
	r.printPlan()
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Println("\nRun aiq doctor in a terminal to go through them one at a time.")
		return nil
	}
	switch doctorAsk("\nGo through them now, one at a time? [Y/n] ") {
	case "", "y", "yes":
	default:
		return nil
	}
	a.doctorWizard(r.fixes)

	fmt.Println("\nChecking again...")
	after := a.doctorChecks()
	after.print(false)
	if len(after.fixes) > 0 {
		fmt.Println("Run aiq doctor again to pick up where this left off.")
	}
	return nil
}

// doctorChecks runs every check and works out the fix for each failure.
func (a *app) doctorChecks() *doctorReport {
	r := &doctorReport{byKey: map[string]*doctorFix{}}

	for _, provider := range pool.Providers {
		bin, err := a.binary(provider)
		r.check(err == nil, &doctorFix{
			key:   "binary:" + provider,
			title: fmt.Sprintf("Install the %s CLI", provider),
			steps: []string{fmt.Sprintf("Install %s so it is on PATH, or set providers.%s.binary in %s to its path.", provider, provider, paths.ConfigFile())},
		}, "%s binary: %s", provider, orErr(bin, err))
	}

	shimsFix := &doctorFix{
		key:     "shims",
		title:   "Write the PATH shims",
		command: "aiq shim install",
		steps:   []string{"aiq writes a shim for each CLI and launcher into " + paths.ShimsDir() + "."},
		run:     func() error { return cmdShim([]string{"install"}) },
	}

	// Launchers: the command has to exist, and its shim has to be there for
	// the name to work in a shell.
	names := make([]string, 0, len(a.cfg.Launchers))
	for n := range a.cfg.Launchers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		l := a.cfg.Launchers[n]
		path, err := resolveLauncher(n, l)
		r.check(err == nil, &doctorFix{
			key:   "launcher:" + n,
			title: fmt.Sprintf("Point launcher %s at a command that exists", n),
			steps: []string{fmt.Sprintf("Fix or remove launchers.%s in %s.", n, paths.ConfigFile())},
		}, "launcher %s → %s", n, orErr(path, err))
		if _, err := os.Stat(paths.ShimPath(n)); err != nil {
			r.check(false, shimsFix, "launcher %s has no shim; run: aiq shim install", n)
		}
	}

	shimDir := paths.ShimsDir()
	onPath := false
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == shimDir {
			onPath = true
			break
		}
	}
	pathLine := fmt.Sprintf("export PATH=%q:$PATH", shimDir)
	rc := shellRC()
	pathFix := &doctorFix{
		key:   "path",
		title: "Put the shim dir first on PATH",
		steps: []string{
			fmt.Sprintf("Add this line at the end of %s:\n\n       %s\n", rc, pathLine),
			"Open a new terminal so the change takes effect.",
		},
	}
	_, shimErr := os.Stat(filepath.Join(shimDir, "claude"))
	r.check(shimErr == nil, shimsFix, "shims installed in %s", shimDir)
	r.check(onPath, pathFix, "shim dir on PATH (%s)", pathLine)
	if which, err := exec.LookPath("claude"); err == nil {
		r.check(strings.HasPrefix(which, shimDir), pathFix, "`claude` on PATH resolves to %s", which)
	}

	_, installed, _ := claude.InstalledStatusline(paths.RealClaudeHome())
	r.check(installed, &doctorFix{
		key:     "statusline:claude",
		title:   "Install the Claude statusline multiplexer",
		command: "aiq statusline install",
		steps:   []string{"aiq points statusLine in " + claude.SettingsPath(paths.RealClaudeHome()) + " at itself and keeps your own statusline rendering."},
		run:     a.claudeStatuslineInstall,
	}, "Claude statusline multiplexer installed in %s", claude.SettingsPath(paths.RealClaudeHome()))

	accounts, _ := a.st.ListAccounts("")
	r.check(len(accounts) > 0, &doctorFix{
		key:   "accounts",
		title: "Add an account",
		steps: []string{fmt.Sprintf("In another terminal, run: aiq account add <%s> <name>", config.ProviderList("|"))},
	}, "%d accounts registered", len(accounts))
	if agyAccounts, _ := a.st.ListAccounts("agy"); len(agyAccounts) > 0 {
		_, installed, _ := agy.InstalledStatusline(paths.RealAgyHome())
		r.check(installed, &doctorFix{
			key:     "statusline:agy",
			title:   "Install the agy statusline multiplexer",
			command: "aiq statusline install agy",
			steps:   []string{"aiq points statusLine in " + agy.SettingsPath(paths.RealAgyHome()) + " at itself; agy quota is polled through it."},
			run:     func() error { return a.cmdAgyStatuslineAdmin("install") },
		}, "agy statusline multiplexer installed in %s (quota is polled through it)%s", agy.SettingsPath(paths.RealAgyHome()), hint(!installed, "aiq statusline install agy"))
	}

	daemonFix := &doctorFix{
		key:     "daemon",
		title:   "Install the daemon service",
		command: "aiq daemon install",
		steps:   []string{"aiq installs the background service that keeps quota telemetry fresh, and starts it."},
		run:     a.doctorDaemonInstall,
	}
	_, svc := daemon.Installed()
	if svc {
		daemonFix.title = "Restart the daemon service"
		daemonFix.steps = []string{"aiq reinstalls the background service, which restarts it."}
	}
	daemonUp := daemon.Alive(a.cfg.Daemon.Listen)
	now := time.Now()
	for _, acc := range accounts {
		// A daemon that is down leaves every account's telemetry stale;
		// starting it polls them all, so that is the fix, not a poll each.
		var staleFix *doctorFix
		if !daemonUp {
			staleFix = daemonFix
		}
		a.checkAccount(r, acc, now, staleFix)
	}

	r.check(svc, daemonFix, "daemon service installed")
	r.check(daemonUp, daemonFix, "daemon answering on http://%s/", a.cfg.Daemon.Listen)

	// The machine first: a daemon that is down makes every account's
	// telemetry look stale, and logins are the slow part.
	sort.SliceStable(r.fixes, func(i, j int) bool { return !r.fixes[i].account && r.fixes[j].account })
	return r
}

// accountNeeds is what it takes to bring one account back.
type accountNeeds struct {
	sync, login, grant, poll bool
}

func (a *app) checkAccount(r *doctorReport, acc state.Account, now time.Time, staleFix *doctorFix) {
	var need accountNeeds
	fix := &doctorFix{key: "account:" + acc.ID, account: true}
	check := func(ok bool, flag *bool, format string, v ...any) {
		if !ok {
			*flag = true
		}
		r.check(ok, fix, format, v...)
	}

	hasCred := pool.HasCredential(acc)
	check(hasCred, &need.login, "%s: CLI login present (%s)%s", acc.ID, shortHome(acc.Home), hint(!hasCred, "aiq account login "+acc.ID))
	// A credential file can outlive its login, and Claude's quota poll
	// uses aiq's own grant, so the CLI's credential is read on its own.
	if acc.Provider == "claude" && hasCred {
		if err := a.pool.ProbeClaudeLogin(acc, now); err != nil {
			// No answer is not a logout: a locked keychain hides a good
			// login, and a fresh OAuth login would not unlock it.
			r.check(false, &doctorFix{
				key:   "credential-store",
				title: "Let aiq read the Claude logins",
				steps: []string{"aiq could not read a stored Claude login (see the FAIL lines above). On macOS, unlock the login keychain (security unlock-keychain) and allow access if it asks."},
			}, "%s: CLI login: %v", acc.ID, err)
		} else {
			u, _, _ := a.st.GetUsage(acc.ID)
			switch why := pool.CLILoginProblem(acc, u); {
			case why != "":
				check(false, &need.login, "%s: %s", acc.ID, why)
			case u.CLIIdentity == "":
				check(true, nil, "%s: CLI logged in (not yet verified as whom: its access token has expired; the next session refreshes it)", acc.ID)
			default:
				check(true, nil, "%s: CLI logged in as %s", acc.ID, u.CLIIdentity)
			}
		}
	}
	if acc.Provider == "claude" {
		has := claude.HasGrant(acc.Home)
		var bad error
		if has {
			_, bad = claude.LoadGrant(claude.GrantPath(acc.Home))
		}
		if bad != nil {
			check(false, &need.grant, "%s: poll grant unreadable: %v — run: aiq account authorize %s", acc.ID, bad, acc.ID)
		} else {
			check(has, &need.grant, "%s: poll grant present%s", acc.ID, hint(!has, "aiq account authorize "+acc.ID))
		}
	}
	u, ok, _ := a.st.GetUsage(acc.ID)
	ws, _ := a.st.ListWindows(acc.ID)
	switch {
	case !acc.Enabled:
		check(true, nil, "%s: disabled, so not polled (aiq account enable %s)", acc.ID, acc.ID)
	case acc.Provider == "copilot":
		// The pool has no Copilot poller yet, so no telemetry is expected.
		check(true, nil, "%s: quota not polled (no Copilot poller yet)", acc.ID)
	case ok && u.PollError != "":
		msg := strings.ToLower(u.PollError)
		flag := &need.poll
		switch {
		case strings.Contains(msg, "account authorize"):
			flag = &need.grant
		case strings.Contains(msg, "account login"), strings.Contains(msg, codex.ErrAuthRequired.Error()):
			flag = &need.login
		}
		check(false, flag, "%s: last poll failed: %s", acc.ID, u.PollError)
	case len(ws) == 0 && staleFix != nil:
		r.check(false, staleFix, "%s: no telemetry yet (the daemon is not running)", acc.ID)
	case len(ws) == 0:
		check(false, &need.poll, "%s: no telemetry yet (aiq account poll)", acc.ID)
	default:
		age := now.Sub(time.Unix(ws[0].ObservedAt, 0))
		stale := age > 2*time.Duration(a.cfg.Poll.IntervalSeconds)*time.Second+time.Minute
		if stale && staleFix != nil {
			r.check(false, staleFix, "%s: telemetry %s old (the daemon is not running)", acc.ID, age.Round(time.Second))
			break
		}
		check(!stale, &need.poll, "%s: telemetry %s old", acc.ID, age.Round(time.Second))
	}
	if !acc.Native {
		_, err := os.Lstat(filepath.Join(acc.Home, "settings.json"))
		if acc.Provider == "codex" {
			_, err = os.Lstat(filepath.Join(acc.Home, "config.toml"))
		}
		check(err == nil, &need.sync, "%s: overlay synced", acc.ID)
		if acc.Provider == "claude" && claude.CredentialDrifted(acc.Home) {
			check(false, &need.login, "%s: the credential file a launcher refreshed is newer than the keychain copy; run: aiq account login %s", acc.ID, acc.ID)
		}
	}
	a.planAccountFix(fix, acc, need)
}

// planAccountFix fills in the one fix for everything wrong with an account:
// the overlay, the CLI login, the poll grant, then a poll to prove it works.
func (a *app) planAccountFix(f *doctorFix, acc state.Account, need accountNeeds) {
	if acc.Provider != "claude" {
		need.grant = false
	}
	who := acc.Identity
	if who == "" {
		who = "the account that belongs in " + acc.ID
	}
	var what, cmds []string
	switch {
	case need.login:
		what = append(what, "log the CLI back in")
		cmds = append(cmds, "aiq account login "+shellQuote(acc.ID))
		if acc.Provider == "claude" {
			f.steps = append(f.steps, fmt.Sprintf("`claude auth login` opens a browser: sign in as %s.", who))
		} else {
			f.steps = append(f.steps, fmt.Sprintf("The %s CLI's login opens a browser if it needs one: sign in as %s.", acc.Provider, who))
		}
		f.browser = true
	case need.sync:
		// Only a login or a launch syncs an overlay from the command line.
		what = append(what, "re-sync the overlay home")
		cmds = append(cmds, fmt.Sprintf("aiq run %s --account %s", acc.Provider, shellQuote(acc.Name)))
		f.steps = append(f.steps, "aiq re-links the shared settings into "+shortHome(acc.Home)+" (by hand, any launch on the account does it).")
	}
	if need.grant {
		what = append(what, "renew the quota poll grant")
		cmds = append(cmds, "aiq account authorize "+shellQuote(acc.ID))
		again := ""
		if need.login {
			again = " again"
		}
		f.steps = append(f.steps, fmt.Sprintf("A browser page opens to authorize quota polling: sign in as %s%s, then paste the code it shows here.", who, again))
		f.browser = true
	}
	if len(what) == 0 {
		what = append(what, "poll quota again")
	}
	// The pool never polls a disabled account, so there is nothing to
	// confirm with.
	if acc.Enabled {
		cmds = append(cmds, "aiq account poll "+shellQuote(acc.ID))
		f.steps = append(f.steps, "aiq polls the account to confirm it works.")
	}
	f.title = acc.ID + ": " + strings.Join(what, ", then ")
	f.command = strings.Join(cmds, " && ")
	// A retry picks up at the action that failed: redoing a sign-in that
	// worked is one more chance to sign in as the wrong account.
	var synced, loggedIn, granted, grantUnproven bool
	f.run = func() error {
		if need.sync && !need.login && !synced {
			if err := a.prepareHome(acc); err != nil {
				return err
			}
			synced = true
		}
		if need.login && !loggedIn {
			if err := a.accountLogin(acc.ID); err != nil {
				return err
			}
			loggedIn = true
		}
		if need.grant && !granted {
			err := a.accountAuthorize(acc.ID)
			// A grant saved before its first poll failed is kept, so a
			// retry goes straight to the poll.
			grantUnproven = errors.Is(err, errGrantPollFailed)
			granted = err == nil || grantUnproven
			if err != nil {
				return err
			}
		}
		if !acc.Enabled {
			// The pool skips a disabled account, so a saved grant that has
			// not polled yet is proven here.
			if grantUnproven {
				u, err := claude.Poll(claude.GrantPath(acc.Home), time.Now())
				if err != nil {
					return fmt.Errorf("the saved grant does not poll: %w", err)
				}
				grantUnproven = false
				if cur, err := a.st.GetAccount(acc.ID); err == nil {
					a.adoptGrantIdentity(cur, u)
				}
			}
			return nil
		}
		return a.pollAndReport(acc.ID)
	}
}

// doctorDaemonInstall (re)installs the service and waits for it to answer,
// so the check that follows sees the daemon it started.
func (a *app) doctorDaemonInstall() error {
	if err := cmdDaemon([]string{"install"}); err != nil {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if daemon.Alive(a.cfg.Daemon.Listen) {
			return nil
		}
	}
	return fmt.Errorf("the service is installed but nothing answers on %s yet; see %s", a.cfg.Daemon.Listen, daemon.LogPath())
}

func (r *doctorReport) print(verbose bool) {
	passed := 0
	for _, c := range r.checks {
		switch {
		case !c.ok:
			fmt.Printf("FAIL %s\n", c.text)
		case verbose:
			fmt.Printf("ok   %s\n", c.text)
			passed++
		default:
			passed++
		}
	}
	more := ""
	if !verbose {
		more = " (aiq doctor -v lists every check)"
	}
	if passed == len(r.checks) {
		fmt.Printf("all %d checks passed%s\n", passed, more)
		return
	}
	fmt.Printf("\n%d problem(s); the other %d checks passed%s\n", len(r.checks)-passed, passed, more)
}

func (r *doctorReport) printPlan() {
	if len(r.fixes) == 1 {
		fmt.Println("\nOne thing to fix:")
	} else {
		fmt.Printf("\n%d things to fix, in this order:\n", len(r.fixes))
	}
	for i, f := range r.fixes {
		fmt.Printf("  %d. %s\n", i+1, f.title)
		if f.command != "" {
			fmt.Printf("       %s\n", f.command)
		}
	}
}

// doctorWizard walks through the fixes one at a time, asking before each.
func (a *app) doctorWizard(fixes []*doctorFix) {
	signIns := 0
	for _, f := range fixes {
		if f.browser {
			signIns++
		}
	}
	tipped := false
	for i, f := range fixes {
		fmt.Printf("\n── Step %d of %d: %s\n", i+1, len(fixes), f.title)
		if f.browser && signIns > 1 && !tipped {
			fmt.Println("   Each of these sign-ins is a different account. Sign out of the site between")
			fmt.Println("   them, or copy each link into a private window, so the browser does not hand")
			fmt.Println("   back the account it is already signed in as.")
			tipped = true
		}
		for j, s := range f.steps {
			if len(f.steps) == 1 {
				fmt.Printf("   %s\n", s)
			} else {
				fmt.Printf("   %d. %s\n", j+1, s)
			}
		}
		if f.run == nil {
			if doctorAsk("   Press Enter when done, s to skip, q to stop: ") == "q" {
				return
			}
			if !a.wizardReload() {
				return
			}
			continue
		}
		prompt := "   Press Enter to start, s to skip, q to stop: "
		for {
			switch doctorAsk(prompt) {
			case "q":
				return
			case "s":
			default:
				fmt.Println()
				err := f.run()
				if !a.wizardReload() {
					return
				}
				if err != nil {
					fmt.Printf("   ✗ %v\n", err)
					prompt = "   Press Enter to try again, s to skip, q to stop: "
					continue
				}
				fmt.Println("   ✓ done")
			}
			break
		}
	}
}

// reloadConfig picks up what a fix saved, so a later fix does not write
// back a stale copy.
func (a *app) reloadConfig() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a.cfg, a.pool.Cfg = cfg, cfg
	return nil
}

// wizardReload reloads the config after a step and holds the wizard until
// it loads: going on with the old copy could save it over the person's
// edit. It reports false when the person stops.
func (a *app) wizardReload() bool {
	for {
		err := a.reloadConfig()
		if err == nil {
			return true
		}
		fmt.Printf("   ✗ %s does not load: %v\n", paths.ConfigFile(), err)
		if doctorAsk("   Fix it, then press Enter to try again, or q to stop: ") == "q" {
			return false
		}
	}
}

// doctorAsk reads one answer. A fresh reader each time leaves nothing
// buffered for the login and authorize prompts that read stdin themselves;
// a terminal hands over one line per read. End of input means stop.
func doctorAsk(prompt string) string {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Println()
		return "q"
	}
	return strings.ToLower(strings.TrimSpace(line))
}

// shellQuote quotes a word for a POSIX shell when it needs it, so a
// printed command pastes as one argument.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./_-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellRC names the startup file of the login shell, for PATH advice.
func shellRC() string {
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		return "~/.zshrc"
	case "bash":
		// macOS terminals start login shells, which read the profile.
		if runtime.GOOS == "darwin" {
			return "~/.bash_profile"
		}
		return "~/.bashrc"
	}
	return "your shell's startup file"
}

func hint(show bool, cmd string) string {
	if !show {
		return ""
	}
	return " — run: " + cmd
}

func orErr(s string, err error) string {
	if err != nil {
		return err.Error()
	}
	return s
}
