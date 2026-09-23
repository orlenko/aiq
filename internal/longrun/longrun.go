// Package longrun supervises long-running sessions: it decides when an
// account is about to run dry, asks the session to wrap up through its
// hooks, and moves the session to another account in the same tmux pane.
//
// Same provider: the CLI resumes the same transcript on the new account
// (the overlay shares projects/ and sessions/). Other provider: a fresh
// session starts from the handoff note the agent wrote while draining.
package longrun

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tier"
	"github.com/orlenko/aiq/internal/tmux"
)

// HandoffPath is where the draining agent writes its note for a workspace.
func HandoffPath(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return filepath.Join(paths.DataDir(), "handoff", hex.EncodeToString(sum[:8])+".md")
}

// SessionName is the tmux session for a workspace.
func SessionName(prefix, workspace string) string {
	base := filepath.Base(workspace)
	base = strings.Map(func(r rune) rune {
		if r == '.' || r == ':' {
			return '-'
		}
		return r
	}, base)
	sum := sha256.Sum256([]byte(workspace))
	return fmt.Sprintf("%s-%s-%s", prefix, base, hex.EncodeToString(sum[:2]))
}

// DrainInstruction is what the hook injects when the account is nearly out.
// The agent keeps working: stopping early strands the task on an account
// with quota left, and the move happens either way when it runs out.
func DrainInstruction(workspace string, remaining float64, until string) string {
	return fmt.Sprintf(`aiq: this account's quota is running low (%.0f%% left%s). Keep working on your task; do not stop, pause, or hold back work to save quota. aiq manages quota for this session and moves it to a fresh account, at the end of a turn or when this one runs out.
1. At the next natural break, write a handoff note to %s (create the directory if needed): objective, what is done, what is in progress, files and branches involved, unresolved problems, exact next steps, constraints and gotchas.
2. Keep the note current as you go, updating it whenever the plan or state changes. The move can come mid-turn, and whoever picks up (you on a resumed transcript, or a different model starting fresh) relies on the note plus the working tree.`,
		remaining, until, HandoffPath(workspace))
}

// IdleRotateDue says whether a quiet session on a nearly spent account
// should move now. It is the case the drain misses: an agent that stops on
// its own above drain_pct (a quota floor, nothing left it thinks it can
// afford) never uses the rest, so the account never runs out and nothing
// moves it. lastWrite is the newest write to its transcript or subagents;
// zero means unknown, and an unknown session is left alone.
func IdleRotateDue(l state.Lease, remaining float64, pct float64, idleFor time.Duration, lastWrite, now time.Time) bool {
	if pct <= 0 || remaining > pct || l.InTurn() || l.TurnEndedAt == 0 || l.SessionID == "" || lastWrite.IsZero() {
		return false
	}
	return now.Sub(time.Unix(l.TurnEndedAt, 0)) >= idleFor && now.Sub(lastWrite) >= idleFor
}

// LastWrite is the newest mtime of a transcript file and the directory the
// CLI keeps beside it (Claude writes subagent and workflow transcripts
// there). Zero when the transcript is unknown or missing.
func LastWrite(transcript string) time.Time {
	if transcript == "" {
		return time.Time{}
	}
	fi, err := os.Stat(transcript)
	if err != nil {
		return time.Time{}
	}
	newest := fi.ModTime()
	dir := strings.TrimSuffix(transcript, filepath.Ext(transcript))
	seen := 0
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > 20000 {
			return filepath.SkipAll
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

// ArchiveNote moves a previous handoff note aside so a successor never reads
// a note from an earlier takeover as if it were current.
func ArchiveNote(workspace string, now time.Time) {
	p := HandoffPath(workspace)
	if _, err := os.Stat(p); err != nil {
		return
	}
	os.Rename(p, strings.TrimSuffix(p, ".md")+"."+now.Format("20060102-150405")+".md")
}

// TakeoverPrompt is the first message the successor receives. resumed says
// whether the successor sees the previous transcript.
func TakeoverPrompt(workspace string, resumed bool, from string) string {
	note := HandoffPath(workspace)
	hasNote := false
	if _, err := os.Stat(note); err == nil {
		hasNote = true
	}
	var b strings.Builder
	if resumed {
		b.WriteString("aiq moved this session to a fresh quota account; the conversation above is yours. ")
	} else {
		fmt.Fprintf(&b, "aiq is handing a long-running task over to you from a %s session that ran out of quota. ", from)
	}
	if hasNote {
		fmt.Fprintf(&b, "Read the handoff note at %s first. ", note)
	} else {
		b.WriteString("No handoff note was written (the previous session stopped abruptly). ")
	}
	b.WriteString("Inspect `git status` and the diff in this working tree; the previous session's work is already there. Continue the task from that point without undoing completed work, and do not ask for confirmation to continue.")
	return b.String()
}

// Supervisor runs inside the daemon.
type Supervisor struct {
	Pool   *pool.Pool
	Logf   func(format string, v ...any)
	AiqBin string

	// mutedLeases remembers the leases already reported as unsupervisable,
	// so the daemon says it once instead of on every tick.
	mutedLeases map[int64]bool
	// strandedLeases remembers idle leases already reported as having no
	// account to move to, for the same reason.
	strandedLeases map[int64]bool
}

// launcherFor names the launcher a successor on provider should start
// under. A lease with no launcher keeps none. A move to another provider
// takes the first launcher in the chain that starts that provider; when
// the chain has none, the session cannot move there at all.
func (s *Supervisor) launcherFor(l state.Lease, provider string) string {
	if l.Launcher == "" {
		return ""
	}
	if l.Provider == provider {
		return l.Launcher
	}
	own, ok := s.Pool.Cfg.Launcher(l.Launcher)
	if !ok {
		return ""
	}
	for _, name := range own.Fallback {
		if nl, ok := s.Pool.Cfg.Launcher(name); ok && nl.Provider == provider {
			return name
		}
	}
	return ""
}

// Tick evaluates every long lease once.
func (s *Supervisor) Tick() {
	leases, err := s.Pool.St.ListLeases()
	if err != nil {
		return
	}
	now := time.Now()
	for _, l := range leases {
		if l.Mode != state.ModeLong || l.Hostname != pool.Hostname() {
			continue
		}
		s.evaluate(l, now)
	}
}

// headroom returns the tightest binding window's remaining percentage and
// the wall time of its reset ("" when unknown), plus whether the account is
// blocked outright (exhausted, cooldown, disabled).
func (s *Supervisor) headroom(accountID, provider string, now time.Time) (remaining float64, until string, blocked bool) {
	cands, err := s.Pool.Candidates(provider)
	if err != nil {
		return 100, "", false
	}
	pol := s.Pool.Policy(provider, state.ModeInteractive, now)
	for _, r := range selector.Rank(pol, cands) {
		if r.ID == accountID && !r.Eligible {
			blocked = true
		}
	}
	remaining = 100
	for _, c := range cands {
		if c.ID != accountID {
			continue
		}
		for _, w := range c.Windows {
			if w.Kind != state.KindShort && w.Kind != state.KindWeekly {
				continue
			}
			if w.Scope != "" && (pol.ModelScope == "" || !strings.Contains(strings.ToLower(w.Scope), strings.ToLower(pol.ModelScope))) {
				continue
			}
			if w.UsedPct < 0 || (w.ResetsAt > 0 && w.ResetsAt <= now.Unix()) {
				continue
			}
			if rem := 100 - w.UsedPct; rem < remaining {
				remaining = rem
				if w.ResetsAt > 0 {
					until = ", resets " + time.Unix(w.ResetsAt, 0).Local().Format("15:04")
				}
			}
		}
	}
	return remaining, until, blocked
}

func (s *Supervisor) evaluate(l state.Lease, now time.Time) {
	st := s.Pool.St
	if l.Pane != "" {
		if exists, _ := tmux.PaneAlive(l.Pane); !exists {
			st.ReleaseLease(l.ID)
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: pane %s is gone, released", l.ID, l.Pane), now)
			return
		}
		if l.Provider == "codex" {
			s.dismissRateLimitPrompt(l, now)
		}
	}
	remaining, until, blocked := s.headroom(l.AccountID, l.Provider, now)
	drainPct := s.Pool.Cfg.Long.DrainPct
	grace := time.Duration(s.Pool.Cfg.Long.IdleGraceSeconds) * time.Second

	switch l.Drain {
	case state.DrainNone:
		if blocked {
			// Hard stop: the CLI cannot make another request. Move now.
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: account blocked, taking over", l.ID), now)
			s.takeover(l, now)
			return
		}
		if remaining <= drainPct {
			ArchiveNote(l.Workspace, now)
			st.SetLeaseDrain(l.ID, state.DrainRequested, now)
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: %.0f%% left%s, drain requested", l.ID, remaining, until), now)
			return
		}
		s.rotateIfIdle(l, remaining, now)
	case state.DrainRequested, state.DrainDraining:
		if blocked {
			s.takeover(l, now)
			return
		}
		// An idle session has nothing in flight; no need to wait for a
		// wrap-up turn that will never come.
		if l.Drain == state.DrainRequested && !l.InTurn() && now.Sub(time.Unix(l.DrainAt, 0)) > grace {
			// A launched session whose hooks never reported looks idle
			// whatever it is doing, because the turn timestamps come from
			// those hooks. Moving it then would cut a turn in half.
			if l.Launcher != "" && l.SessionID == "" {
				if s.mutedLeases == nil {
					s.mutedLeases = map[int64]bool{}
				}
				if !s.mutedLeases[l.ID] {
					s.mutedLeases[l.ID] = true
					st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf(
						"lease %d: launcher %q reports no turns (hooks cannot reach the state database); waiting for a wrap-up instead of moving it", l.ID, l.Launcher), now)
				}
				return
			}
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: idle, taking over", l.ID), now)
			s.takeover(l, now)
		}
	case state.DrainReady:
		s.takeover(l, now)
	case state.DrainWaiting:
		// The agent keeps working while no account can take over. When one
		// appears, move it at the end of a turn rather than in the middle,
		// unless the account has run out.
		if !blocked && l.InTurn() {
			return
		}
		s.takeover(l, now)
	}
}

// rotateIfIdle moves a quiet session off a nearly spent account when an
// account with more headroom than the rotate threshold is free.
func (s *Supervisor) rotateIfIdle(l state.Lease, remaining float64, now time.Time) {
	cfg := s.Pool.Cfg.Long
	idleFor := time.Duration(cfg.IdleRotateMinutes) * time.Minute
	if l.Pane == "" || !IdleRotateDue(l, remaining, cfg.IdleRotatePct, idleFor, LastWrite(l.Transcript), now) {
		return
	}
	acc, err := s.successorAbove(l, now, cfg.IdleRotatePct)
	if err != nil {
		if s.strandedLeases == nil {
			s.strandedLeases = map[int64]bool{}
		}
		if !s.strandedLeases[l.ID] {
			s.strandedLeases[l.ID] = true
			s.Pool.St.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf(
				"lease %d: idle at %.0f%% left, no account above %.0f%% to move it to", l.ID, remaining, cfg.IdleRotatePct), now)
		}
		return
	}
	s.Pool.St.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf(
		"lease %d: idle %s at %.0f%% left, moving it to a fresh account", l.ID, now.Sub(time.Unix(l.TurnEndedAt, 0)).Round(time.Minute), remaining), now)
	s.moveTo(l, acc, now)
}

// dismissRateLimitPrompt answers Codex's switch-to-a-cheaper-model menu with
// "Keep current model" when it is open in the lease's pane. Sessions launched
// with codex.UnattendedArgs never show it; this covers older ones.
func (s *Supervisor) dismissRateLimitPrompt(l state.Lease, now time.Time) {
	screen, err := tmux.Capture(l.Pane)
	if err != nil || !codex.RateLimitPromptOpen(screen) {
		return
	}
	if err := tmux.SendKeys(l.Pane, "2"); err != nil {
		s.Logf("lease %d: dismiss rate-limit prompt: %v", l.ID, err)
		return
	}
	s.Pool.St.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: kept the current model at Codex's rate-limit prompt", l.ID), now)
}

// successor picks the account a long lease moves to.
func (s *Supervisor) successor(l state.Lease, now time.Time) (state.Account, error) {
	// Search every fallback provider above the normal drain floor first.
	// Only when none qualifies may a nearly empty account be used as a last
	// resort.
	drainPct := s.Pool.Cfg.Long.DrainPct
	if acc, err := s.successorAbove(l, now, drainPct); err == nil {
		return acc, nil
	}
	// Do not let the floor strand the lease when the current account has even
	// less quota or is blocked outright. In those cases any account with more
	// usable headroom is progress. Keeping the comparison strict also prevents
	// two equally low accounts from handing the lease back and forth.
	remaining, _, blocked := s.headroom(l.AccountID, l.Provider, now)
	if blocked {
		return s.successorAbove(l, now, 0)
	}
	if remaining < drainPct {
		return s.successorAbove(l, now, remaining)
	}
	return state.Account{}, fmt.Errorf("no account with headroom")
}

// successorAbove picks the first eligible account in fallback order with
// more than minRemaining left.
func (s *Supervisor) successorAbove(l state.Lease, now time.Time, minRemaining float64) (state.Account, error) {
	order := strings.Split(l.Fallback, ",")
	if l.Fallback == "" {
		order = append([]string{l.Provider}, s.Pool.Cfg.Long.Fallback...)
	}
	seen := map[string]bool{}
	for _, provider := range order {
		provider = strings.TrimSpace(provider)
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		cands, err := s.Pool.Candidates(provider)
		if err != nil {
			continue
		}
		pol := s.Pool.Policy(provider, state.ModeInteractive, now)
		pol.SkipID = l.AccountID
		for _, r := range selector.Rank(pol, cands) {
			if !r.Eligible {
				continue
			}
			acc, err := s.Pool.St.GetAccount(r.ID)
			if err != nil {
				continue
			}
			// The successor must have real headroom, not just be eligible.
			rem, _, _ := s.headroom(acc.ID, provider, now)
			if rem <= minRemaining {
				continue
			}
			return acc, nil
		}
	}
	return state.Account{}, fmt.Errorf("no account with headroom")
}

// takeover respawns the pane on the successor account.
func (s *Supervisor) takeover(l state.Lease, now time.Time) {
	st := s.Pool.St
	if l.Pane == "" {
		st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: no tmux pane recorded; cannot take over", l.ID), now)
		st.SetLeaseDrain(l.ID, state.DrainWaiting, now)
		return
	}
	acc, err := s.successor(l, now)
	if err != nil {
		if l.Drain != state.DrainWaiting {
			st.SetLeaseDrain(l.ID, state.DrainWaiting, now)
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: %v; waiting for a window to reset", l.ID, err), now)
		}
		return
	}
	s.moveTo(l, acc, now)
}

// moveTo respawns the lease's pane on acc.
func (s *Supervisor) moveTo(l state.Lease, acc state.Account, now time.Time) {
	st := s.Pool.St
	same := acc.Provider == l.Provider
	launcher := s.launcherFor(l, acc.Provider)
	if l.Launcher != "" && launcher == "" {
		if l.Drain != state.DrainWaiting {
			st.SetLeaseDrain(l.ID, state.DrainWaiting, now)
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf(
				"lease %d: no launcher for %s in the %q chain; waiting rather than starting unlaunched", l.ID, acc.Provider, l.Launcher), now)
		}
		return
	}
	old, _ := st.GetAccount(l.AccountID)
	if same && acc.Provider == "claude" {
		if err := claude.CopyProjectTrust(old.Home, old.Native, acc.Home, acc.Native, l.Workspace); err != nil {
			s.Logf("trust copy %s → %s: %v", old.ID, acc.ID, err)
		}
	}
	// A transcript with no completed turn is not resumable (the CLI has
	// nothing persisted yet); start fresh in that case.
	resume := same && l.SessionID != "" && l.TurnStartedAt > 0
	launch := func(resume bool) error {
		args := []string{s.AiqBin, "run", acc.Provider, "--long", "--account", acc.Name,
			"--takeover", fmt.Sprintf("%d", l.ID),
			"--fallback", strings.Join(fallbackOrder(l, s.Pool.Cfg.Long.Fallback), ",")}
		if launcher != "" {
			args = append(args, "--launcher", launcher)
		}
		if resume {
			args = append(args, "--resume-session", l.SessionID)
		}
		args = append(args, "--nudge", TakeoverPrompt(l.Workspace, resume, l.Provider), "--")
		if l.Args != "" {
			if same {
				args = append(args, replayArgs(l.Provider, splitArgs(l.Args))...)
			} else {
				args = append(args, TranslateArgs(l.Provider, acc.Provider, splitArgs(l.Args))...)
			}
		}
		cmd := tmux.Quote(args)
		s.Logf("lease %d: respawn %s in %s: %s", l.ID, l.Pane, l.Workspace, cmd)
		return tmux.Respawn(l.Pane, l.Workspace, cmd)
	}
	// died reports whether the pane's command has exited, and describes what
	// is on screen. A tmux query that fails is not a death: respawning then
	// would kill a session that is merely slow to start.
	died := func() (bool, string) {
		exists, running, err := tmux.PaneState(l.Pane)
		if err != nil {
			s.Logf("lease %d: pane %s query failed, leaving it alone: %v", l.ID, l.Pane, err)
			return false, ""
		}
		if !exists || running {
			return false, ""
		}
		return true, tmux.PaneDiag(l.Pane) + " | screen: " + tmux.Tail(l.Pane, 6)
	}
	if err := launch(resume); err != nil {
		st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: respawn failed: %v", l.ID, err), now)
		st.SetLeaseDrain(l.ID, state.DrainWaiting, now)
		return
	}
	st.ReleaseLease(l.ID)
	st.LogEvent(acc.Provider, acc.ID, "long", fmt.Sprintf("took over lease %d from %s in pane %s (resume=%v)", l.ID, l.AccountID, l.Pane, resume), now)
	// A resume that the CLI rejects exits within seconds; fall back to a
	// fresh session so the pane never sits dead.
	time.Sleep(8 * time.Second)
	dead, why := died()
	if dead && resume {
		st.LogEvent(acc.Provider, acc.ID, "long", fmt.Sprintf("resume of %s exited at once; starting fresh in pane %s [%s]", l.SessionID, l.Pane, why), time.Now())
		if err := launch(false); err != nil {
			st.LogEvent(acc.Provider, acc.ID, "long", fmt.Sprintf("fresh respawn failed: %v", err), time.Now())
			s.readopt(l, now)
			return
		}
		time.Sleep(8 * time.Second)
		dead, why = died()
	}
	if dead {
		st.LogEvent(acc.Provider, acc.ID, "long", fmt.Sprintf(
			"start in pane %s exited at once (aiq long attach to inspect) [%s]", l.Pane, why), time.Now())
		s.readopt(l, now)
	}
}

// readopt takes a session back under supervision after every attempt to hand
// it to a successor died on the spot. The lease was released the moment the
// pane was respawned, on the assumption the successor would register its own;
// a successor that never got that far leaves nothing behind, so the daemon
// forgets the session entirely and the user finds a dead pane that nothing is
// going to retry. Re-adding the lease against the daemon's own pid (the old
// process is gone, and PruneLeases drops leases whose pid is not alive) keeps
// it in the supervisor's hands, waiting, so the next tick tries again once
// whatever broke the launch has passed.
func (s *Supervisor) readopt(l state.Lease, now time.Time) {
	retry := l
	retry.PID = os.Getpid()
	retry.Drain = state.DrainWaiting
	retry.DrainAt = now.Unix()
	retry.TakeoverOf = l.ID
	id, err := s.Pool.St.AddLease(retry)
	if err != nil {
		s.Logf("lease %d: could not re-adopt after a failed takeover: %v", l.ID, err)
		return
	}
	s.Pool.St.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf(
		"lease %d: every start in pane %s died at once; re-adopted as lease %d and waiting to retry", l.ID, l.Pane, id), now)
}

func fallbackOrder(l state.Lease, def []string) []string {
	if l.Fallback != "" {
		return strings.Split(l.Fallback, ",")
	}
	return append([]string{l.Provider}, def...)
}

// replayArgs is the same-provider replay of a lease's launch arguments. A
// session selection in the original command (Codex `resume`/`fork` with its
// id, --last, --all or prompt; Claude --resume/-r, --continue/-c,
// --session-id; Antigravity --conversation, --continue/-c) was consumed by
// the session that ran: the successor gets its own --resume-session or
// starts fresh, so those tokens are dropped.
func replayArgs(provider string, args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch provider {
		case "codex":
			if a == "resume" || a == "fork" {
				for i+1 < len(args) && (args[i+1] == "--last" || args[i+1] == "--all" || !strings.HasPrefix(args[i+1], "-")) {
					i++
				}
				continue
			}
		case "claude", "copilot":
			switch {
			case a == "--continue" || a == "-c":
				continue
			case a == "--resume" || a == "-r" || a == "--session-id":
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
				continue
			case strings.HasPrefix(a, "--resume=") || strings.HasPrefix(a, "--session-id="):
				continue
			}
		case "agy":
			switch {
			case a == "--continue" || a == "-c":
				continue
			case a == "--conversation":
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
				}
				continue
			case strings.HasPrefix(a, "--conversation="):
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

// BypassFlag is each CLI's permission bypass as a takeover spells it.
var BypassFlag = map[string]string{
	"claude":  "--dangerously-skip-permissions",
	"codex":   "--dangerously-bypass-approvals-and-sandbox",
	"agy":     "--dangerously-skip-permissions",
	"copilot": "--yolo",
}

// ModelArgs spells a model ("" for the CLI's default) and an effort level
// ("" for its default) for a provider. Antigravity carries the level in the
// model name, so the two are composed there.
func ModelArgs(provider, model, level string) []string {
	if provider == "agy" {
		return agy.ModelArgs(model, level)
	}
	var out []string
	if model != "" {
		out = append(out, "--model", model)
	}
	switch {
	case level == "":
	case provider == "codex":
		out = append(out, "-c", "model_reasoning_effort="+level)
	default:
		out = append(out, "--effort", level)
	}
	return out
}

// modelTier is tier.Of, with Antigravity's effort suffixes ignored.
func modelTier(provider, model string) int {
	if provider == "agy" {
		for i, m := range tier.Models[provider] {
			if agy.SameModel(m, model) {
				return i
			}
		}
		return -1
	}
	return tier.Of(provider, model)
}

// TranslateArgs carries across a cross-provider takeover the arguments that
// mean the same thing on every CLI: the permission bypass (Claude's and
// Antigravity's --dangerously-skip-permissions or Claude's --permission-mode
// bypassPermissions, Codex's --dangerously-bypass-approvals-and-sandbox or
// --yolo), a model named by an aiq tier (opus ↔ gpt-5.6-sol ↔
// gemini-3.1-pro-high), and an effort level (Claude and Antigravity
// --effort, Codex -c model_reasoning_effort). Any other model, a resume
// target, extra directories and a prompt are provider-specific and dropped.
// Same provider returns args unchanged.
func TranslateArgs(from, to string, args []string) []string {
	if from == to {
		return args
	}
	bypass, model, effort := false, -1, 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--":
			i = len(args)
		case a == "--dangerously-skip-permissions", a == "--dangerously-bypass-approvals-and-sandbox", a == "--yolo",
			a == "--permission-mode=bypassPermissions":
			bypass = true
		case a == "--permission-mode":
			bypass = bypass || next() == "bypassPermissions"
		case a == "--model" || (from == "codex" && a == "-m"):
			model = modelTier(from, next())
		case strings.HasPrefix(a, "--model="):
			model = modelTier(from, strings.TrimPrefix(a, "--model="))
		case from != "codex" && a == "--effort":
			effort = tier.EffortOf(from, next())
		case from != "codex" && strings.HasPrefix(a, "--effort="):
			effort = tier.EffortOf(from, strings.TrimPrefix(a, "--effort="))
		case from == "codex" && (a == "-c" || a == "--config" || strings.HasPrefix(a, "-c") || strings.HasPrefix(a, "--config=")):
			value := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(a, "--config="), "-c"), "=")
			if a == "-c" || a == "--config" {
				value = next()
			}
			if key, level, ok := strings.Cut(value, "="); ok && strings.TrimSpace(key) == "model_reasoning_effort" {
				effort = tier.EffortOf(from, strings.Trim(strings.TrimSpace(level), `"'`))
			}
		}
	}
	var out []string
	if bypass {
		if flag := BypassFlag[to]; flag != "" {
			out = append(out, flag)
		}
	}
	if !tier.Known(to) {
		return out
	}
	name, level := "", ""
	if model >= 0 {
		name = tier.Models[to][model]
	}
	if effort > 0 {
		level = tier.Efforts[to][effort-1]
	}
	return append(out, ModelArgs(to, name, level)...)
}

// splitArgs decodes the JSON array a long lease records its args as.
func splitArgs(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return strings.Fields(s)
	}
	return out
}
