package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/overlay"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/runner"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

// runFlags are aiq's own flags, taken from before the "--" separator (or
// from the front of the args when no separator is present).
type runFlags struct {
	account        string
	next           bool
	mode           string
	inheritAuthEnv bool
	rest           []string

	// modelScope overrides the binding model-scoped window for this launch
	// ("fable" skips accounts whose Fable weekly cap is dry; "opus" ignores it).
	modelScope string
	// wait keeps a worker polling for a free slot instead of refusing.
	wait    time.Duration
	waitErr error // a --wait value that did not parse (reported as bad-flags)

	// Long-session flags (set by `aiq long` and by the daemon's takeover).
	long          bool
	takeover      int64
	fallback      string
	resumeSession string
	nudge         string
}

func parseRunFlags(args []string) runFlags {
	var f runFlags
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
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
		case a == "--wait" && i+1 < len(args):
			f.wait, f.waitErr = parseWait(args[i+1])
			i++
		case strings.HasPrefix(a, "--wait="):
			f.wait, f.waitErr = parseWait(strings.TrimPrefix(a, "--wait="))
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
	f := parseRunFlags(args)
	if provider != "claude" && provider != "codex" {
		return fmt.Errorf("unknown provider %s", provider)
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
		return a.execProvider(provider, f.rest, os.Environ())
	}

	// CLI self-management never routes.
	passthrough := (provider == "claude" && claude.Passthrough(f.rest)) || (provider == "codex" && codex.Passthrough(f.rest))
	if passthrough || os.Getenv("AIQ_BYPASS") == "1" {
		if len(f.rest) > 0 && (f.rest[0] == "login" || f.rest[0] == "logout" || f.rest[0] == "auth") {
			fmt.Fprintf(os.Stderr, "aiq: `%s %s` acts on your real home, not on a pool account; pool logins are `aiq account login <provider>/<name>`\n", provider, f.rest[0])
		}
		a.close()
		return a.execProvider(provider, f.rest, os.Environ())
	}
	if _, err := a.binary(provider); err != nil {
		return configErr("no-binary", "%v", err)
	}

	mode := f.mode
	if f.long {
		mode = state.ModeLong
	}
	if mode == "" {
		mode = os.Getenv("AIQ_MODE")
	}
	if mode == "" {
		isWorker := (provider == "claude" && claude.IsWorker(f.rest)) || (provider == "codex" && codex.IsWorker(f.rest))
		if isWorker {
			mode = state.ModeWorker
		} else {
			mode = state.ModeInteractive
		}
	}
	if mode != state.ModeWorker && mode != state.ModeInteractive && mode != state.ModeLong {
		return configErr("bad-flags", "--mode must be interactive or worker")
	}
	if f.modelScope == "" {
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
	for {
		attempts++
		acc, notes, err := a.selectAccount(provider, mode, f, workspace, tried)
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
		if err := a.prepareHome(acc); err != nil {
			return err
		}
		env := a.launchEnv(provider, acc, f.inheritAuthEnv, depth)
		summary := a.usageSummary(provider, acc.ID)
		if summary != "" {
			summary = " — " + summary
		}
		if mode != state.ModeWorker || attempts > 1 {
			fmt.Fprintf(os.Stderr, "aiq: using %s%s\n", acc.ID, summary)
		}

		now := time.Now()
		lease := state.Lease{
			AccountID: acc.ID, PID: os.Getpid(), Hostname: hostname(), Mode: mode,
			Cwd: cwd(), Depth: depth, ParentAccount: os.Getenv("AIQ_ACCOUNT"),
			RootID: int64(envInt("AIQ_ROOT")), Args: strings.Join(trimArgs(f.rest), " "), StartedAt: now.Unix(),
		}
		if mode == state.ModeLong {
			lease.Workspace, lease.Provider, lease.Pane = workspace, provider, os.Getenv("TMUX_PANE")
			lease.Fallback, lease.TakeoverOf = f.fallback, f.takeover
			rest := f.rest
			if rest == nil {
				rest = []string{}
			}
			if enc, err := json.Marshal(rest); err == nil {
				lease.Args = string(enc) // exact args, replayed on a same-provider takeover
			}
		}
		leaseID, _ := a.st.AddLease(lease)
		if lease.RootID == 0 {
			env = append(env, "AIQ_ROOT="+strconv.FormatInt(leaseID, 10))
		}
		a.st.LogEvent(provider, acc.ID, "launch", mode+" "+lease.Args, now)

		if mode == state.ModeLong {
			env = append(env, "AIQ_LONG=1", "AIQ_LEASE="+strconv.FormatInt(leaseID, 10))
			self, _ := os.Executable()
			a.close()
			return a.execProvider(provider, longArgs(provider, self, f), env)
		}
		if mode == state.ModeInteractive {
			// exec keeps our pid, so the lease follows the CLI process.
			a.close()
			return a.execProvider(provider, f.rest, env)
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

// trimArgs shortens an argument list for the lease/event log: long
// arguments are cut, and the whole line is capped so an IDE's injected hook
// flags do not swamp the event table.
// longArgs assembles the CLI argument list for a supervised session: aiq's
// hooks, the user's own args, an optional resume, and an optional first
// prompt (the takeover nudge).
func longArgs(provider, aiqBin string, f runFlags) []string {
	var out []string
	switch provider {
	case "claude":
		out = append(out, "--settings", claude.HookSettings(aiqBin))
		out = append(out, f.rest...)
		if f.resumeSession != "" {
			out = append(out, "--resume", f.resumeSession)
		}
		if f.nudge != "" {
			out = append(out, f.nudge)
		}
	case "codex":
		out = append(out, codex.HookArgs(aiqBin)...)
		out = append(out, f.rest...)
		if f.resumeSession != "" {
			out = append(out, "resume", f.resumeSession)
		}
		if f.nudge != "" {
			out = append(out, f.nudge)
		}
	}
	return out
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
	if f.modelScope != "" {
		pol.ModelScope = f.modelScope
		if strings.EqualFold(f.modelScope, "none") {
			pol.ModelScope = ""
		}
	}
	pol.AffinityID = affinity
	if f.account != "" && len(tried) == 0 {
		pol.ForceID = state.AccountID(provider, f.account)
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
	var spec overlay.Spec
	switch acc.Provider {
	case "claude":
		spec = claude.OverlaySpec(acc.Home)
	case "codex":
		spec = codex.OverlaySpec(acc.Home)
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
	var env []string
	switch provider {
	case "claude":
		p, _ := a.claudeProvider()
		env = p.Env(acc.Home, acc.Native, inheritAuthEnv)
	case "codex":
		p, _ := a.codexProvider()
		env = p.Env(acc.Home, acc.Native, inheritAuthEnv)
	}
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

// usageSummary renders "5h 31%, weekly 48%, fable 52%" from stored windows:
// the plan-wide windows plus any binding model-scoped cap.
func (a *app) usageSummary(provider, accountID string) string {
	ws, err := a.st.ListWindows(accountID)
	if err != nil {
		return ""
	}
	scope := strings.ToLower(a.pool.ModelScope(provider))
	var parts []string
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
	}
	return strings.Join(parts, ", ")
}
