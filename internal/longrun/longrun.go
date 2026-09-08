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
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
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
func DrainInstruction(workspace string, remaining float64, until string) string {
	return fmt.Sprintf(`aiq: this account's quota is almost exhausted (%.0f%% left%s). Wrap up for a handoff now:
1. Finish only the unit of work you are in the middle of; do not start new work.
2. Stop background processes, servers and monitors you started; let running subagents finish or stop them.
3. Write a handoff note to %s (create the directory if needed): objective, what is done, what is in progress, files and branches involved, unresolved problems, exact next steps, constraints and gotchas. Another agent, possibly a different model, resumes from this note plus the working tree.
4. Then end your turn without further changes. aiq restarts the session on a fresh account and tells it to read the note.`,
		remaining, until, HandoffPath(workspace))
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
		}
	case state.DrainRequested, state.DrainDraining:
		if blocked {
			s.takeover(l, now)
			return
		}
		// An idle session has nothing in flight; no need to wait for a
		// wrap-up turn that will never come.
		if l.Drain == state.DrainRequested && !l.InTurn() && now.Sub(time.Unix(l.DrainAt, 0)) > grace {
			st.LogEvent(l.Provider, l.AccountID, "long", fmt.Sprintf("lease %d: idle, taking over", l.ID), now)
			s.takeover(l, now)
		}
	case state.DrainReady:
		s.takeover(l, now)
	case state.DrainWaiting:
		s.takeover(l, now)
	}
}

// successor picks the account a long lease moves to.
func (s *Supervisor) successor(l state.Lease, now time.Time) (state.Account, error) {
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
			if rem <= s.Pool.Cfg.Long.DrainPct {
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
	same := acc.Provider == l.Provider
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
		if resume {
			args = append(args, "--resume-session", l.SessionID)
		}
		args = append(args, "--nudge", TakeoverPrompt(l.Workspace, resume, l.Provider), "--")
		if l.Args != "" {
			if same {
				args = append(args, splitArgs(l.Args)...)
			} else {
				args = append(args, TranslateArgs(l.Provider, acc.Provider, splitArgs(l.Args))...)
			}
		}
		return tmux.Respawn(l.Pane, l.Workspace, tmux.Quote(args))
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
	if resume {
		time.Sleep(8 * time.Second)
		if _, running := tmux.PaneAlive(l.Pane); !running {
			st.LogEvent(acc.Provider, acc.ID, "long", fmt.Sprintf("resume of %s exited at once; starting fresh in pane %s", l.SessionID, l.Pane), time.Now())
			if err := launch(false); err != nil {
				st.LogEvent(acc.Provider, acc.ID, "long", fmt.Sprintf("fresh respawn failed: %v", err), time.Now())
			}
		}
	}
}

func fallbackOrder(l state.Lease, def []string) []string {
	if l.Fallback != "" {
		return strings.Split(l.Fallback, ",")
	}
	return append([]string{l.Provider}, def...)
}

// TranslateArgs carries across a cross-provider takeover the arguments that
// mean the same thing on both CLIs. Today that is the permission bypass:
// Claude's --dangerously-skip-permissions (or --permission-mode
// bypassPermissions) and Codex's --dangerously-bypass-approvals-and-sandbox
// (or --yolo). A model, a resume target, extra directories and a prompt are
// provider-specific and dropped. Same provider returns args unchanged.
func TranslateArgs(from, to string, args []string) []string {
	if from == to {
		return args
	}
	bypass := false
	for i, a := range args {
		switch a {
		case "--dangerously-skip-permissions", "--dangerously-bypass-approvals-and-sandbox", "--yolo",
			"--permission-mode=bypassPermissions":
			bypass = true
		case "--permission-mode":
			if i+1 < len(args) && args[i+1] == "bypassPermissions" {
				bypass = true
			}
		}
	}
	if !bypass {
		return nil
	}
	switch to {
	case "claude":
		return []string{"--dangerously-skip-permissions"}
	case "codex":
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	}
	return nil
}

// splitArgs decodes the JSON array a long lease records its args as.
func splitArgs(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return strings.Fields(s)
	}
	return out
}
