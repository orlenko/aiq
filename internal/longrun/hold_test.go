package longrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

// holdPool is a Codex account at 4% left, with an optional Codex account and
// an optional Claude account well above the drain floor. Moves are recorded,
// not made, and every pane is alive.
type holdPool struct {
	s     *Supervisor
	st    *state.Store
	path  string // the store's file
	moves []string
}

// noTmux puts a tmux that always fails first on PATH, so a test that reaches
// the real move cannot touch a tmux server.
func noTmux(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
}

func newHoldPool(t *testing.T, codexTarget, claudeTarget bool) *holdPool {
	t.Helper()
	t.Setenv("AIQ_DATA_DIR", t.TempDir())
	// The accounts are native: a real move would use this home's logins.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	noTmux(t)
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	add := func(id string, used float64) {
		provider, name, _ := strings.Cut(id, "/")
		if err := st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true}); err != nil {
			t.Fatal(err)
		}
		setUsed(t, st, id, used)
	}
	add("codex/acct-a", 96)
	if codexTarget {
		add("codex/acct-c", 10)
	}
	if claudeTarget {
		add("claude/acct-b", 10)
	}
	cfg := config.Default()
	cfg.Providers.Claude.ModelScope = ""
	h := &holdPool{st: st, path: path}
	h.s = &Supervisor{Pool: &pool.Pool{Cfg: cfg, St: st}, Logf: func(string, ...any) {}}
	h.s.paneAlive = func(string) (bool, bool) { return true, true }
	h.s.move = func(l state.Lease, acc state.Account, _ time.Time) { h.moves = append(h.moves, acc.ID) }
	return h
}

// lease adds an idle Codex long lease asked to drain a minute ago.
func (h *holdPool) lease(t *testing.T, edit func(*state.Lease)) state.Lease {
	t.Helper()
	l := state.Lease{AccountID: "codex/acct-a", Mode: state.ModeLong, Hostname: pool.Hostname(), Provider: "codex",
		Workspace: "/home/me/proj", Pane: "%1", Fallback: "codex,claude", Drain: state.DrainRequested,
		DrainAt: time.Now().Add(-time.Minute).Unix(), StartedAt: time.Now().Add(-2 * time.Minute).Unix(), PID: os.Getpid()}
	if edit != nil {
		edit(&l)
	}
	id, err := h.st.AddLease(l)
	if err != nil {
		t.Fatal(err)
	}
	// Turn marks come from the hooks, not from the lease's first write.
	if l.TurnStartedAt > 0 {
		h.st.MarkTurn(id, true, time.Unix(l.TurnStartedAt, 0))
	}
	if l.TurnEndedAt > 0 {
		h.st.MarkTurn(id, false, time.Unix(l.TurnEndedAt, 0))
	}
	got, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (h *holdPool) events(t *testing.T, contains string) int {
	t.Helper()
	evs, err := h.st.ListEvents(500)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if strings.Contains(e.Detail, contains) {
			n++
		}
	}
	return n
}

func (h *holdPool) tick(t *testing.T, id int64) state.Lease {
	t.Helper()
	l, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	h.s.evaluate(l, time.Now())
	got, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// codexRollout writes a Codex transcript for session id with one turn, or
// none.
func codexRollout(t *testing.T, id string, turns bool) string {
	t.Helper()
	lines := `{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + id + `","cwd":"/home/me/proj"}}` + "\n"
	if turns {
		lines += `{"timestamp":"2026-01-01T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"fix the test"}}` + "\n" +
			`{"timestamp":"2026-01-01T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"fixed"}}` + "\n"
	}
	p := filepath.Join(t.TempDir(), "rollout-2026-01-01T00-00-00-"+id+".jsonl")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDecideIdle(t *testing.T) {
	codex := state.Lease{Provider: "codex"}
	manual := state.Lease{Provider: "codex", DrainManual: true}
	copilot := state.Lease{Provider: "copilot"}
	for _, c := range []struct {
		name                       string
		l                          state.Lease
		found, hasContext, carries bool
		want                       idleAction
	}{
		{"fresh successor would lose the conversation", codex, true, true, false, idleHold},
		{"same transcript resumes", codex, true, true, true, idleMove},
		{"nothing to lose", codex, true, false, false, idleMove},
		{"no successor", codex, false, true, false, idleStranded},
		{"the person asked for the move", manual, true, true, false, idleMove},
		{"no hooks to deliver a wrap-up", copilot, true, true, false, idleMove},
	} {
		if got := decideIdle(c.l, c.found, c.hasContext, c.carries); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A resumed session sitting at its prompt on a nearly spent account must not
// be replaced by a fresh session on another provider: it has not been asked
// for a note, and its next prompt would ask it.
func TestIdleResumedSessionIsHeldForItsWrapUp(t *testing.T) {
	h := newHoldPool(t, false, true)
	tr := codexRollout(t, "0000-aaaa", true)
	l := h.lease(t, func(l *state.Lease) { l.SessionID, l.Transcript = "0000-aaaa", tr })
	for i := 0; i < 3; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainRequested {
			t.Fatalf("tick %d: drain = %q, want it still asked", i, got.Drain)
		}
	}
	if len(h.moves) != 0 {
		t.Fatalf("moved to %v, want held", h.moves)
	}
	if n := h.events(t, "not prompted since it started; held so it can write a handoff note first"); n != 1 {
		t.Fatalf("%d hold events, want 1", n)
	}

	// Its next turn brings the wrap-up (the hooks' part), then it moves.
	now := time.Now()
	h.st.MarkTurn(l.ID, true, now)
	h.st.SetLeaseDrain(l.ID, state.DrainDraining, now)
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moved mid-turn to %v", h.moves)
	}
	h.st.MarkTurn(l.ID, false, now)
	h.st.SetLeaseDrain(l.ID, state.DrainReady, now)
	h.tick(t, l.ID)
	if len(h.moves) != 1 || h.moves[0] != "claude/acct-b" {
		t.Fatalf("moves = %v, want one to claude/acct-b after the wrap-up", h.moves)
	}
}

// A session that has had turns is held the same way, and says so differently.
func TestIdleSessionWithTurnsIsHeld(t *testing.T) {
	h := newHoldPool(t, false, true)
	l := h.lease(t, func(l *state.Lease) {
		l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-bbbb", time.Now().Add(-time.Hour).Unix(), time.Now().Add(-50*time.Minute).Unix()
	})
	h.tick(t, l.ID)
	if len(h.moves) != 0 || h.events(t, "idle at 4% left; held so it can write a handoff note first") != 1 {
		t.Fatalf("moves = %v; want held with one event", h.moves)
	}
}

func TestIdleMovesWhenNothingIsLost(t *testing.T) {
	t.Run("same transcript resumes", func(t *testing.T) {
		h := newHoldPool(t, true, true)
		tr := codexRollout(t, "0000-cccc", true)
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.Transcript = "0000-cccc", tr })
		h.tick(t, l.ID)
		if len(h.moves) != 1 || h.moves[0] != "codex/acct-c" {
			t.Fatalf("moves = %v, want codex/acct-c", h.moves)
		}
	})
	t.Run("fresh session with nothing in it", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		l := h.lease(t, nil)
		h.tick(t, l.ID)
		if len(h.moves) != 1 {
			t.Fatalf("moves = %v, want one", h.moves)
		}
	})
	t.Run("transcript with no turn", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		tr := codexRollout(t, "0000-dddd", false)
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.Transcript = "0000-dddd", tr })
		h.tick(t, l.ID)
		if len(h.moves) != 1 {
			t.Fatalf("moves = %v, want one", h.moves)
		}
	})
	t.Run("drain the person asked for", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		l := h.lease(t, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-eeee", 1, 2 })
		if err := h.st.RequestDrainByUser(l.ID, time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		h.tick(t, l.ID)
		if len(h.moves) != 1 {
			t.Fatalf("moves = %v, want one", h.moves)
		}
	})
}

// A resume aiq knows only by id (given in the CLI's own arguments) or whose
// transcript cannot be read counts as having a conversation.
func TestIdleUnknownTranscriptIsHeld(t *testing.T) {
	for name, edit := range map[string]func(*state.Lease){
		"id only": func(l *state.Lease) { l.SessionID = "0000-ffff" },
		"unreadable": func(l *state.Lease) {
			l.SessionID, l.Transcript = "0000-ffff", t.TempDir() // a directory does not parse
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHoldPool(t, false, true)
			l := h.lease(t, edit)
			h.tick(t, l.ID)
			if len(h.moves) != 0 {
				t.Fatalf("moves = %v, want held", h.moves)
			}
		})
	}
}

// With no account to take it, an idle session stays asked: in DrainWaiting
// its next prompt would bring no wrap-up.
func TestIdleWithNoSuccessorStaysAsked(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.TurnStartedAt, l.TurnEndedAt = 1, 2 })
	for i := 0; i < 2; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainRequested || got.DrainAt != l.DrainAt {
			t.Fatalf("tick %d: drain = %q at %d, want requested at %d", i, got.Drain, got.DrainAt, l.DrainAt)
		}
	}
	if n := h.events(t, "no account to move to; it keeps going"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

// Recording the session id at launch must not let a launched session whose
// hooks never reported be moved: it may be in the middle of a turn.
func TestSilentLauncherIsNotMoved(t *testing.T) {
	h := newHoldPool(t, true, false)
	tr := codexRollout(t, "0000-1111", true)
	l := h.lease(t, func(l *state.Lease) { l.Launcher, l.SessionID, l.Transcript = "safe-codex", "0000-1111", tr })
	h.tick(t, l.ID)
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moves = %v, want none", h.moves)
	}
	if n := h.events(t, "no turn seen yet from launcher"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

// A lease that changed after it was read (a prompt typed meanwhile) is left
// for the next tick.
func TestIdleRereadsTheLease(t *testing.T) {
	h := newHoldPool(t, true, false)
	l := h.lease(t, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-2222", 1, 2 })
	h.st.MarkTurn(l.ID, true, time.Now())
	h.s.evaluate(l, time.Now()) // the stale copy says idle
	if len(h.moves) != 0 {
		t.Fatalf("moves = %v, want none", h.moves)
	}
}

// A blocked account with no successor leaves the session as it is: nothing
// asked it to wrap up, and its note stays. Once the block lifts with little
// left, the drain floor asks, and an idle session is then held.
func TestBlockedWithNoSuccessorLeavesTheSessionAlone(t *testing.T) {
	h := newHoldPool(t, false, false)
	note := HandoffPath("/home/me/proj")
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte("the plan\n"), 0o600)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt, l.TurnStartedAt, l.TurnEndedAt = state.DrainNone, 0, 1, 2 })
	h.st.MarkExhausted("codex/acct-a", "test", time.Now().Add(time.Hour), time.Now())
	for i := 0; i < 3; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainNone {
			t.Fatalf("tick %d: drain = %q, want none", i, got.Drain)
		}
	}
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("the note was moved: %v", err)
	}
	if h.events(t, "its account is out and no other account can take it") != 1 || h.events(t, "taking over") != 0 {
		t.Fatal("want one blocked event that claims no move")
	}
	// The block lifts at the floor: the low-quota path asks, and archives the note then.
	if err := h.st.MarkReady("codex/acct-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := h.tick(t, l.ID); got.Drain != state.DrainRequested {
		t.Fatalf("drain = %q, want requested", got.Drain)
	}
	if _, err := os.Stat(note); err == nil {
		t.Fatal("the note was not archived when the drain was asked for")
	}
	// A Claude account appears; the idle session is held, not moved fresh.
	if err := h.st.AddAccount(state.Account{ID: "claude/acct-b", Provider: "claude", Name: "acct-b", Enabled: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	setUsed(t, h.st, "claude/acct-b", 10)
	got, _ := h.st.GetLease(l.ID)
	h.st.SetLeaseDrain(l.ID, state.DrainRequested, time.Unix(got.DrainAt, 0).Add(-time.Minute))
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moves = %v, want held", h.moves)
	}
}

// A disabled account with quota left and nowhere to go is not told it is
// running low.
func TestDisabledAccountIsNotDrained(t *testing.T) {
	h := newHoldPool(t, false, false)
	setUsed(t, h.st, "codex/acct-a", 40)
	if err := h.st.SetEnabled("codex/acct-a", false); err != nil {
		t.Fatal(err)
	}
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	for i := 0; i < 3; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainNone {
			t.Fatalf("tick %d: drain = %q, want none", i, got.Drain)
		}
	}
	if h.events(t, "its account is disabled and no other account can take it") != 1 || h.events(t, "taking over") != 0 {
		t.Fatal("want one blocked event that claims no move")
	}
}

// Each block is said once, and a block after the account came back is said
// again: the morning log must show every night the session could not move.
func TestEachBlockIsReported(t *testing.T) {
	h := newHoldPool(t, false, false)
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	for _, enabled := range []bool{false, false, true, false, false} {
		if err := h.st.SetEnabled("codex/acct-a", enabled); err != nil {
			t.Fatal(err)
		}
		h.tick(t, l.ID)
	}
	if n := h.events(t, "its account is disabled and no other account can take it"); n != 2 {
		t.Fatalf("blocked events = %d, want 2", n)
	}
	// With somewhere to go, the block moves it, and says so.
	if err := h.st.AddAccount(state.Account{ID: "codex/acct-c", Provider: "codex", Name: "acct-c", Enabled: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	setUsed(t, h.st, "codex/acct-c", 10)
	h.tick(t, l.ID)
	h.tick(t, l.ID) // the recorded move leaves the lease as it was
	if len(h.moves) != 2 || h.events(t, "account blocked, taking over") != 1 {
		t.Fatalf("moves = %v; want a move each tick and one event", h.moves)
	}
}

// A block whose move fails is said in full each time, failure included.
func TestEachBlockReportsItsFailedMove(t *testing.T) {
	for name, c := range map[string]struct{ pane, launcher, want string }{
		"pane":     {"%1", "", "respawn failed"},
		"no pane":  {"", "", "no tmux pane recorded"},
		"launcher": {"%1", "cbox", "no launcher for claude"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHoldPool(t, c.launcher == "", c.launcher != "")
			h.s.move = nil // the real move; tmux fails
			h.s.Pool.Cfg.Launchers = map[string]config.Launcher{"cbox": {Provider: "codex"}}
			setUsed(t, h.st, "codex/acct-a", 40)
			l := h.lease(t, func(l *state.Lease) {
				l.Drain, l.DrainAt, l.Pane, l.Launcher = state.DrainNone, 0, c.pane, c.launcher
			})
			for _, enabled := range []bool{false, false, false, true, false, false} {
				if err := h.st.SetEnabled("codex/acct-a", enabled); err != nil {
					t.Fatal(err)
				}
				if got := h.tick(t, l.ID); got.Drain != state.DrainNone {
					t.Fatalf("drain = %q, want none", got.Drain)
				}
			}
			if n := h.events(t, c.want); n != 2 {
				t.Fatalf("%q events = %d, want one for each block", c.want, n)
			}
		})
	}
}

// An idle rotation whose respawn keeps failing says so once: only the end of
// a block lets a failure be said again.
func TestFailedRespawnOfARotationIsReportedOnce(t *testing.T) {
	h := newHoldPool(t, true, false)
	h.s.move = nil // the real move; tmux fails
	setUsed(t, h.st, "codex/acct-a", 88)
	tr := codexRollout(t, "0000-6666", true)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(tr, old, old)
	l := h.lease(t, func(l *state.Lease) {
		l.Drain, l.DrainAt, l.SessionID, l.Transcript = state.DrainNone, 0, "0000-6666", tr
		l.TurnStartedAt, l.TurnEndedAt = old.Add(-time.Minute).Unix(), old.Unix()
	})
	for i := 0; i < 4; i++ {
		h.tick(t, l.ID)
	}
	if n := h.events(t, "respawn failed"); n != 1 {
		t.Fatalf("respawn failed events = %d, want 1", n)
	}
	// A block that follows fails the same way, and says so.
	if err := h.st.SetEnabled("codex/acct-a", false); err != nil {
		t.Fatal(err)
	}
	h.tick(t, l.ID)
	h.tick(t, l.ID)
	if n := h.events(t, "respawn failed"); n != 2 {
		t.Fatalf("respawn failed events = %d, want 2", n)
	}
}

// An idle rotation whose move cannot start (no launcher for the other
// provider) leaves a healthy session and its note alone, and says so once.
func TestFailedRotationLeavesTheSessionAlone(t *testing.T) {
	h := newHoldPool(t, false, true)
	h.s.move = nil
	h.s.Pool.Cfg.Launchers = map[string]config.Launcher{"cbox": {Provider: "codex"}}
	setUsed(t, h.st, "codex/acct-a", 88)
	note := HandoffPath("/home/me/proj")
	os.MkdirAll(filepath.Dir(note), 0o700)
	os.WriteFile(note, []byte("the plan\n"), 0o600)
	tr := codexRollout(t, "0000-7777", true)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(tr, old, old)
	l := h.lease(t, func(l *state.Lease) {
		l.Drain, l.DrainAt, l.Launcher, l.SessionID, l.Transcript = state.DrainNone, 0, "cbox", "0000-7777", tr
		l.TurnStartedAt, l.TurnEndedAt = old.Add(-time.Minute).Unix(), old.Unix()
	})
	for i := 0; i < 6; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainNone {
			t.Fatalf("tick %d: drain = %q, want none", i, got.Drain)
		}
	}
	if _, err := os.Stat(note); err != nil {
		t.Fatalf("the note was moved: %v", err)
	}
	if h.events(t, "moving it to a fresh account") != 1 || h.events(t, "no launcher for claude") != 1 {
		t.Fatal("want one rotation and one no-launcher event")
	}
}

// A long session started outside tmux has no pane. Moving it would respawn
// whatever pane tmux reads an empty target as.
func TestNoPaneIsNeverMoved(t *testing.T) {
	resumable := func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-8888", 1, 2 }
	for name, c := range map[string]struct {
		codexTarget bool
		edit        func(*state.Lease)
	}{
		"fresh":     {true, func(l *state.Lease) {}},
		"resumable": {true, resumable},
		"held":      {false, resumable}, // only Claude could take it: the hold
	} {
		t.Run(name, func(t *testing.T) {
			h := newHoldPool(t, c.codexTarget, true)
			l := h.lease(t, func(l *state.Lease) { c.edit(l); l.Pane = "" })
			for i := 0; i < 2; i++ {
				if got := h.tick(t, l.ID); got.Drain != state.DrainRequested {
					t.Fatalf("drain = %q, want requested", got.Drain)
				}
			}
			if len(h.moves) != 0 || h.events(t, "no tmux pane recorded") != 1 {
				t.Fatalf("moves = %v; want none and one event", h.moves)
			}
			if h.events(t, "taking over") != 0 || h.events(t, "held so it can") != 0 {
				t.Fatal("a session with no pane was said to move or to be held for one")
			}
		})
	}
	t.Run("manual", func(t *testing.T) {
		h := newHoldPool(t, false, true)
		l := h.lease(t, func(l *state.Lease) { l.Pane = "" })
		h.st.RequestDrainByUser(l.ID, time.Now().Add(-time.Minute))
		h.tick(t, l.ID)
		h.tick(t, l.ID)
		if len(h.moves) != 0 || h.events(t, "no tmux pane recorded") != 1 {
			t.Fatalf("moves = %v; want none and one event", h.moves)
		}
	})
}

// A move that keeps failing says so once, and an unwrapped session stays asked.
func TestFailingMoveIsReportedOnce(t *testing.T) {
	h := newHoldPool(t, true, false)
	h.s.move = nil // the real move; tmux fails
	l := h.lease(t, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-9999", 1, 2 })
	for i := 0; i < 4; i++ {
		if got := h.tick(t, l.ID); got.Drain != state.DrainRequested {
			t.Fatalf("tick %d: drain = %q, want requested", i, got.Drain)
		}
	}
	if h.events(t, "idle, taking over") != 1 || h.events(t, "respawn failed") != 1 {
		t.Fatal("want one move and one failure event")
	}
	// After a wrap-up, the same failure waits.
	h.st.SetLeaseDrain(l.ID, state.DrainReady, time.Now())
	if got := h.tick(t, l.ID); got.Drain != state.DrainWaiting {
		t.Fatalf("drain = %q, want waiting", got.Drain)
	}
}

// A manual drain whose successor has no launcher in the chain says so once.
func TestManualDrainWithNoLauncherIsReportedOnce(t *testing.T) {
	h := newHoldPool(t, false, true)
	h.s.move = nil
	h.s.Pool.Cfg.Launchers = map[string]config.Launcher{"cbox": {Provider: "codex"}}
	l := h.lease(t, func(l *state.Lease) { l.Launcher, l.TurnStartedAt, l.TurnEndedAt = "cbox", 1, 2 })
	h.st.RequestDrainByUser(l.ID, time.Now().Add(-time.Minute))
	for i := 0; i < 4; i++ {
		h.tick(t, l.ID)
	}
	if h.events(t, "idle, taking over") != 1 || h.events(t, "no launcher for claude") != 1 {
		t.Fatal("want one move and one no-launcher event")
	}
}

// A turn the person starts after the wrap-up ends before the move, unless the
// account has run out.
func TestReadyWaitsForTheTurnToEnd(t *testing.T) {
	h := newHoldPool(t, false, true)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.TurnStartedAt, l.TurnEndedAt = state.DrainReady, 1, 2 })
	h.st.MarkTurn(l.ID, true, time.Now())
	h.tick(t, l.ID)
	if len(h.moves) != 0 {
		t.Fatalf("moved mid-turn to %v", h.moves)
	}
	h.st.MarkTurn(l.ID, false, time.Now())
	h.tick(t, l.ID)
	if len(h.moves) != 1 {
		t.Fatalf("moves = %v, want one after the turn", h.moves)
	}

	h = newHoldPool(t, false, true)
	l = h.lease(t, func(l *state.Lease) { l.Drain, l.TurnStartedAt, l.TurnEndedAt = state.DrainReady, 1, 2 })
	h.st.MarkTurn(l.ID, true, time.Now())
	h.st.MarkExhausted("codex/acct-a", "test", time.Now().Add(time.Hour), time.Now())
	h.tick(t, l.ID)
	if len(h.moves) != 1 {
		t.Fatalf("moves = %v, want one on a blocked account", h.moves)
	}
}

func TestSuccessorArgsNameOnlyTheSessionsOwnTranscript(t *testing.T) {
	s := &Supervisor{Pool: &pool.Pool{Cfg: config.Default()}, AiqBin: "aiq"}
	tr := codexRollout(t, "0000-aaaa", true)
	real, _ := filepath.EvalSymlinks(tr)
	other := codexRollout(t, "0000-bbbb", true)
	l := state.Lease{ID: 3, Provider: "codex", SessionID: "0000-aaaa", Transcript: tr, Args: `["--model","gpt-6.1-sol"]`}
	codexAcc := state.Account{Provider: "codex", Name: "acct-c"}
	claudeAcc := state.Account{Provider: "claude", Name: "acct-b"}
	has := func(args []string, flag string) string {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1]
			}
			if a == "--" {
				break
			}
		}
		return ""
	}
	args := s.successorArgs(l, codexAcc, "", "", true, "go on")
	if has(args, "--resume-session") != "0000-aaaa" || has(args, "--resume-transcript") != real {
		t.Fatalf("resume: %q", args)
	}
	l2 := l
	l2.Transcript = other
	if args := s.successorArgs(l2, codexAcc, "", "", true, "go on"); has(args, "--resume-transcript") != "" {
		t.Fatalf("another session's transcript passed: %q", args)
	}
	for _, args := range [][]string{s.successorArgs(l, codexAcc, "", "", false, "go on"), s.successorArgs(l, claudeAcc, "", "", false, "go on")} {
		if has(args, "--resume-session") != "" || has(args, "--resume-transcript") != "" {
			t.Fatalf("a fresh start names the session: %q", args)
		}
	}
}

func TestWaitForSuccessorAfterTheWrapUp(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	if got := h.tick(t, l.ID); got.Drain != state.DrainWaiting {
		t.Fatalf("drain = %q, want waiting", got.Drain)
	}
	h.tick(t, l.ID)
	if n := h.events(t, "no account to move to; it keeps going"); n != 1 {
		t.Fatalf("%d events, want 1", n)
	}
}

// What was said about a lease is said again for a new lease that reuses its id.
func TestSayOnceKnowsAReusedID(t *testing.T) {
	s := &Supervisor{}
	l := state.Lease{ID: 7, StartedAt: 100, DrainAt: 200}
	if !s.sayOnce(l, "x") || s.sayOnce(l, "x") {
		t.Fatal("not once")
	}
	if !s.sayOnce(l, "y") {
		t.Fatal("another message was silenced")
	}
	reused := l
	reused.StartedAt = 300
	if !s.sayOnce(reused, "x") {
		t.Fatal("a reused id was silenced")
	}
	s.forget([]state.Lease{reused})
	if !s.sayOnce(l, "x") {
		t.Fatal("a gone lease's record was kept")
	}
}

func TestTranscriptTurnsIsRememberedUntilTheFileChanges(t *testing.T) {
	s := &Supervisor{}
	tr := codexRollout(t, "0000-3333", false)
	l := state.Lease{Provider: "codex", SessionID: "0000-3333", Transcript: tr}
	if got := s.transcriptTurns(l); got != turnsNone {
		t.Fatalf("got %d, want none", got)
	}
	if _, ok := s.turns[tr]; !ok {
		t.Fatal("not remembered")
	}
	full, _ := os.ReadFile(codexRollout(t, "0000-3333", true))
	if err := os.WriteFile(tr, full, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.transcriptTurns(l); got != turnsSome {
		t.Fatalf("got %d after the file grew, want some", got)
	}
	s.forget(nil)
	if len(s.turns) != 0 {
		t.Fatal("a transcript no lease names was kept")
	}
}

func TestCarriesOver(t *testing.T) {
	s := &Supervisor{}
	codexAcc := state.Account{Provider: "codex"}
	claudeAcc := state.Account{Provider: "claude"}
	withTurns := state.Lease{Provider: "codex", SessionID: "0000-4444", Transcript: codexRollout(t, "0000-4444", true)}
	empty := state.Lease{Provider: "codex", SessionID: "0000-5555", Transcript: codexRollout(t, "0000-5555", false)}
	marked := state.Lease{Provider: "codex", SessionID: "0000-6666", TurnStartedAt: 1}
	switch {
	case !s.carriesOver(withTurns, codexAcc):
		t.Error("a transcript with turns does not resume")
	case s.carriesOver(withTurns, claudeAcc):
		t.Error("another provider resumes")
	case s.carriesOver(empty, codexAcc):
		t.Error("a transcript with no turn resumes")
	case !s.carriesOver(marked, codexAcc):
		t.Error("a session with a turn does not resume")
	}
}

func TestResumeTranscript(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string) string {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	claudeFile := write("projects/x/1111.jsonl")
	codexFile := write("sessions/2026/01/01/rollout-2026-01-01T00-00-00-2222.jsonl")
	agyFile := write("brain/3333/.system_generated/logs/transcript.jsonl")
	link := filepath.Join(dir, "4444.jsonl")
	os.Symlink(claudeFile, link)
	// A linked conversation directory that leads to another conversation's.
	os.Symlink(filepath.Join(dir, "brain", "3333"), filepath.Join(dir, "brain", "6666"))
	linkedConv := filepath.Join(dir, "brain", "6666", ".system_generated", "logs", "transcript.jsonl")
	// An overlay-style linked parent with the same file name.
	os.Symlink(filepath.Join(dir, "projects"), filepath.Join(dir, "overlay-projects"))
	overlay := filepath.Join(dir, "overlay-projects", "x", "1111.jsonl")
	for _, c := range []struct {
		provider, id, path string
		ok                 bool
	}{
		{"claude", "1111", claudeFile, true},
		{"codex", "2222", codexFile, true},
		{"agy", "3333", agyFile, true},
		{"claude", "9999", claudeFile, false},
		{"codex", "2222", claudeFile, false},
		{"claude", "1111", "projects/x/1111.jsonl", false},
		{"claude", "x", filepath.Join(dir, "projects", "x"), false},
		{"claude", "4444", link, false},
		{"claude", "5555", filepath.Join(dir, "5555.jsonl"), false},
		{"claude", "", claudeFile, false},
		{"copilot", "1111", claudeFile, false},
		{"agy", "6666", linkedConv, false},
		{"claude", "1111", overlay, true},
	} {
		got, err := ResumeTranscript(c.provider, c.id, c.path)
		if (err == nil) != c.ok {
			t.Errorf("%s %s %s: err = %v, want ok=%v", c.provider, c.id, c.path, err, c.ok)
		}
		if c.ok && got != c.path {
			real, _ := filepath.EvalSymlinks(c.path)
			if got != real {
				t.Errorf("%s: recorded %s", c.path, got)
			}
		}
	}
}

// Turns once seen are not looked for again, however the transcript grows.
func TestTranscriptTurnsKeepsTurnsOnceSeen(t *testing.T) {
	s := &Supervisor{}
	tr := codexRollout(t, "0000-cdcd", true)
	l := state.Lease{Provider: "codex", SessionID: "0000-cdcd", Transcript: tr}
	if got := s.transcriptTurns(l); got != turnsSome {
		t.Fatalf("got %d, want some", got)
	}
	if err := os.WriteFile(tr, []byte("not json at all, and longer than before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.transcriptTurns(l); got != turnsSome {
		t.Fatalf("got %d after a rewrite, want some (no re-parse)", got)
	}
}

func TestForgetDropsEarlierDrains(t *testing.T) {
	s := &Supervisor{}
	l := state.Lease{ID: 7, StartedAt: 100, DrainAt: 200}
	s.sayOnce(l, "x")
	next := l
	next.DrainAt = 300
	s.sayOnce(next, "x")
	s.forget([]state.Lease{next})
	if len(s.said) != 1 || !s.said[episode{7, 100, 300, "x"}] {
		t.Fatalf("said = %v, want only the current drain", s.said)
	}
}
