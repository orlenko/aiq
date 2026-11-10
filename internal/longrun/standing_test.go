package longrun

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

// step is one tick for one lease, as Tick runs it.
func (h *holdPool) step(t *testing.T, id int64) state.Lease {
	t.Helper()
	return h.stepAt(t, id, time.Now())
}

// stepAt is a step at now.
func (h *holdPool) stepAt(t *testing.T, id int64, now time.Time) state.Lease {
	t.Helper()
	l, err := h.st.GetLease(id)
	if err != nil {
		t.Fatal(err)
	}
	h.s.waits = map[int64]waiting{}
	h.s.evaluate(l, now)
	h.s.apply(l, now)
	got, _ := h.st.GetLease(id)
	return got
}

// renames records the window names the daemon sets.
func (h *holdPool) renames() *[]string {
	var names []string
	h.s.rename = func(pane, name string) error {
		names = append(names, pane+" "+name)
		return nil
	}
	return &names
}

// allEvents is every event's text, one a line.
func (h *holdPool) allEvents(t *testing.T) string {
	t.Helper()
	evs, err := h.st.ListEvents(500)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Detail)
	}
	return strings.Join(out, "\n")
}

func (h *holdPool) why(t *testing.T, l state.Lease) string {
	t.Helper()
	whys, err := h.st.LeaseWhys()
	if err != nil {
		t.Fatal(err)
	}
	return whys[state.LeaseRef{ID: l.ID, StartedAt: l.StartedAt}]
}

func TestStanding(t *testing.T) {
	none := &nowhere{lead: "no account to move to; it keeps going on the 4% left.", reasons: []string{"claude/acct-b: 2% left, at or below the 4% floor"}}
	out := &nowhere{lead: "its account is out and no other account can take it; it stays until one can or its own account is back.", reasons: []string{"x"}, word: "disabled", others: true}
	base := state.Lease{AccountID: "codex/acct-a", Pane: "%1", Provider: "codex"}
	with := func(edit func(*state.Lease)) state.Lease { l := base; edit(&l); return l }
	for _, c := range []struct {
		name        string
		l           state.Lease
		f           facts
		label, line string
	}{
		{"running", base, facts{found: true}, "codex/acct-a", ""},
		{"no pane", with(func(l *state.Lease) { l.Drain, l.Pane = state.DrainRequested, "" }), facts{found: true}, "", "no tmux pane recorded; it cannot be moved"},
		{"no launcher", with(func(l *state.Lease) { l.Drain = state.DrainWaiting }), facts{wait: waiting{key: waitNoLauncher, provider: "claude", launcher: "box"}},
			"codex/acct-a · cannot move", `no launcher for claude in the "box" chain, so it cannot move there`},
		{"respawn failed", with(func(l *state.Lease) { l.Drain = state.DrainWaiting }), facts{wait: waiting{key: waitRespawn}},
			"codex/acct-a · cannot move", "the move could not start in its pane; see the daemon log"},
		{"nowhere", with(func(l *state.Lease) { l.Drain = state.DrainDraining }), facts{none: none},
			"codex/acct-a · no account to move to", none.line()},
		{"out, nowhere", base, facts{blocked: true, none: out},
			"codex/acct-a · disabled, no account to move to: log in (aiq doctor)", out.line()},
		{"out, moving", base, facts{blocked: true, found: true, own: "exhausted until 14:00", word: "exhausted"},
			"codex/acct-a · exhausted, moving", "it moves to another account at the next check; its account is out: exhausted until 14:00"},
		{"silent launcher", with(func(l *state.Lease) { l.Drain, l.Launcher = state.DrainRequested, "box" }), facts{found: true},
			"codex/acct-a · moves after next turn", `no turn seen yet from launcher "box"; it moves after its first reported turn, or at once if its account runs out. If it has had turns, its hooks cannot reach aiq`},
		{"requested", with(func(l *state.Lease) { l.Drain, l.TurnEndedAt = state.DrainRequested, 2 }), facts{found: true},
			"codex/acct-a · moves after next turn", "asks for a handoff note at its next prompt or turn end, then moves to another account"},
		{"draining", with(func(l *state.Lease) { l.Drain = state.DrainDraining }), facts{found: true},
			"codex/acct-a · writing handoff note", "writing its handoff note; moves to another account when this turn ends"},
		{"ready in a turn", with(func(l *state.Lease) { l.Drain, l.TurnStartedAt = state.DrainReady, 5 }), facts{found: true},
			"codex/acct-a · moves when this turn ends", "wrapped up; moves to another account when the current turn ends"},
		{"waiting, idle", with(func(l *state.Lease) { l.Drain = state.DrainWaiting }), facts{found: true},
			"codex/acct-a · moving", "wrapped up; moves to another account at the next check"},
		{"literal id", with(func(l *state.Lease) { l.AccountID = "codex/a#[x];" }), facts{found: true}, "codex/a_[x]_", ""},
	} {
		label, line := standing(c.l, c.f)
		if label != c.label || line != c.line {
			t.Errorf("%s: got %q / %q, want %q / %q", c.name, label, line, c.label, c.line)
		}
	}
}

// The reported case: the account is out and nothing can take over. The line
// and the window say so, once, and go when the account comes back.
func TestStandingOfAnAccountThatIsOut(t *testing.T) {
	h := newHoldPool(t, false, false)
	names := h.renames()
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.st.SetEnabled("codex/acct-a", false)
	for i := 0; i < 3; i++ {
		h.step(t, l.ID)
	}
	want := fmt.Sprintf("its account is disabled and no other account can take it; it stays until one can. aiq long stop %d ends it.\n"+
		"codex/acct-a (this session's): disabled\ncodex: no other account\nclaude: no account", l.ID)
	if got := h.why(t, l); got != want {
		t.Fatalf("why = %q, want %q", got, want)
	}
	if len(*names) != 1 || (*names)[0] != "%1 codex/acct-a · disabled, no account to move to" {
		t.Fatalf("renames = %q", *names)
	}
	h.st.SetEnabled("codex/acct-a", true)
	h.step(t, l.ID)
	if got := h.why(t, l); got != "" {
		t.Fatalf("why = %q after the account came back", got)
	}
	if len(*names) != 2 || (*names)[1] != "%1 codex/acct-a" {
		t.Fatalf("renames = %q", *names)
	}
}

// A lease the tick moved, or whose id another lease took meanwhile, is not
// shown: its window is the successor's now.
func TestStandingSkipsAMovedLease(t *testing.T) {
	h := newHoldPool(t, true, false)
	names := h.renames()
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	readopted := int64(0)
	h.s.move = func(l state.Lease, _ state.Account, _ time.Time) {
		h.st.ReleaseLease(l.ID)
		// A live pid: the pool prunes leases whose process is gone.
		retry := l
		retry.ID, retry.PID = 0, os.Getppid()
		readopted, _ = h.st.AddLease(retry)
	}
	h.step(t, l.ID)
	if readopted != l.ID {
		t.Fatalf("the re-adopted lease is %d, want the id %d reused", readopted, l.ID)
	}
	if len(*names) != 0 || h.why(t, l) != "" {
		t.Fatalf("renames = %q, why = %q; want neither", *names, h.why(t, l))
	}
}

// After a restart the daemon renames only what is not running normally.
func TestStandingAfterARestartRenamesNothingRunning(t *testing.T) {
	h := newHoldPool(t, true, false)
	names := h.renames()
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.step(t, l.ID)
	if len(*names) != 0 {
		t.Fatalf("renames = %q", *names)
	}
}

// A failed rename is not retried on the next ticks.
func TestStandingRenameFailureIsNotRetriedAtOnce(t *testing.T) {
	h := newHoldPool(t, false, false)
	tries := 0
	h.s.rename = func(string, string) error { tries++; return fmt.Errorf("no server") }
	l := h.lease(t, nil)
	for i := 0; i < 3; i++ {
		h.step(t, l.ID)
	}
	if tries != 1 {
		t.Fatalf("tries = %d, want 1", tries)
	}
}

// The waiting event carries the reasons and is said once while they change;
// the line follows them.
func TestNoSuccessorEventCarriesReasonsOnce(t *testing.T) {
	h := newHoldPool(t, true, false)
	h.renames()
	setUsed(t, h.st, "codex/acct-c", 97)
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	h.step(t, l.ID)
	first := h.why(t, l)
	setUsed(t, h.st, "codex/acct-c", 98)
	h.step(t, l.ID)
	if n := h.events(t, "no account to move to; it keeps going on the 4% left. codex/acct-c: 3% left, at or below the 4% floor"); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	if h.events(t, "acct-c: 2% left") != 0 {
		t.Fatal("a change of reasons logged again")
	}
	if second := h.why(t, l); first == second || !strings.Contains(second, "codex/acct-c: 2% left") {
		t.Fatalf("why %q then %q", first, second)
	}
}

// Login problems name the command, never an identity, and come first; a
// login is advised only where it would free an account.
func TestReasonsForLogins(t *testing.T) {
	h := newHoldPool(t, false, false)
	add := func(id string, used float64) {
		provider, name, _ := strings.Cut(id, "/")
		if err := h.st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true, Identity: "a@example.com"}); err != nil {
			t.Fatal(err)
		}
		setUsed(t, h.st, id, used)
	}
	add("codex/acct-d", 10)
	add("codex/acct-e", 10)
	h.st.SetCLILogin("codex/acct-e", true, "b@example.com", time.Now())
	h.st.SetCLILogin("codex/acct-d", false, "", time.Now())
	h.st.MarkExhausted("codex/acct-d", "test", time.Now().Add(time.Hour), time.Now())
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	n := h.s.nowhere(l, time.Now())
	all := n.line() + n.event()
	if strings.Contains(all, "@") {
		t.Fatalf("an identity reached the text: %q", all)
	}
	if n.reasons[0] != "codex/acct-e: CLI logged in as another identity — run: aiq account login codex/acct-e" ||
		!strings.HasPrefix(n.reasons[1], "codex/acct-d: exhausted until ") ||
		!strings.HasSuffix(n.reasons[1], ", and CLI not logged in — run: aiq account login codex/acct-d") {
		t.Fatalf("reasons = %q", n.reasons)
	}
	if !n.login() {
		t.Fatal("acct-e frees with a login, but login is not advised")
	}
	h.st.MarkExhausted("codex/acct-e", "test", time.Now().Add(time.Hour), time.Now())
	if h.s.nowhere(l, time.Now()).login() {
		t.Fatal("login advised for accounts that are also exhausted")
	}
}

// The session's own account is the first reason. A login is advised for it
// only when nothing else holds it back, and an account that is out two ways
// names both.
func TestReasonsForTheOwnAccount(t *testing.T) {
	h := newHoldPool(t, false, false)
	names := h.renames()
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.st.SetCLILogin("codex/acct-a", false, "", time.Now())
	h.step(t, l.ID)
	want := "its account is out and no other account can take it; it stays until one can or its own account is back. Log in again to free it: aiq doctor runs the logins.\n" +
		"codex/acct-a (this session's): CLI not logged in — run: aiq account login codex/acct-a\ncodex: no other account\nclaude: no account"
	if got := h.why(t, l); got != want {
		t.Fatalf("why = %q, want %q", got, want)
	}
	if len(*names) != 1 || (*names)[0] != "%1 codex/acct-a · logged out, no account to move to: log in (aiq doctor)" {
		t.Fatalf("renames = %q", *names)
	}
	if h.events(t, "it stays until one can or its own account is back. Log in again to free it: aiq doctor runs the logins. codex/acct-a (this session's): CLI not logged in") != 1 {
		t.Fatal("the blocked event does not carry the advice and the reasons")
	}

	h.st.MarkExhausted("codex/acct-a", "test", time.Now().Add(3*time.Hour), time.Now())
	h.step(t, l.ID)
	got := h.why(t, l)
	if strings.Contains(got, "Log in") || !strings.Contains(got, "codex/acct-a (this session's): exhausted until ") ||
		!strings.Contains(got, ", and CLI not logged in — run: aiq account login codex/acct-a\n") {
		t.Fatalf("why = %q", got)
	}
	if (*names)[len(*names)-1] != "%1 codex/acct-a · exhausted, no account to move to" {
		t.Fatalf("renames = %q", *names)
	}
}

// A command is never followed by a full stop, which a copy would pick up,
// and an id the shell would split is quoted.
func TestLoginCommandsCopyWhole(t *testing.T) {
	h := newHoldPool(t, false, false)
	setUsed(t, h.st, "codex/acct-a", 40)
	h.st.AddAccount(state.Account{ID: "codex/acct;b", Provider: "codex", Name: "acct;b", Enabled: true, Native: true})
	setUsed(t, h.st, "codex/acct;b", 10)
	h.st.SetCLILogin("codex/acct;b", false, "", time.Now())
	h.st.SetCLILogin("codex/acct-a", false, "", time.Now())
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.step(t, l.ID)
	n := h.s.nowhere(l, time.Now())
	for _, text := range []string{h.why(t, l), n.line(), n.event(), h.allEvents(t)} {
		if !strings.Contains(text, "aiq account login 'codex/acct;b'") {
			t.Fatalf("not quoted: %q", text)
		}
		for _, cmd := range []string{"aiq account login codex/acct-a", "aiq account login 'codex/acct;b'"} {
			for rest := text; strings.Contains(rest, cmd); {
				_, rest, _ = strings.Cut(rest, cmd)
				if rest != "" && rest[0] != '\n' && rest[0] != ';' {
					t.Fatalf("%q is followed by %q", cmd, rest)
				}
			}
		}
	}
	if !strings.Contains(n.event(), "Log in to free one") {
		t.Fatalf("event = %q", n.event())
	}
}

// No account in the pool is still a reason, and a long list is cut.
func TestReasonsAreNeverEmptyAndAreCapped(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, nil)
	if got := h.s.nowhere(l, time.Now()).reasons; strings.Join(got, "|") != "codex: no other account|claude: no account" {
		t.Fatalf("reasons = %q", got)
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("codex/acct-%02d", i)
		h.st.AddAccount(state.Account{ID: id, Provider: "codex", Name: id[6:], Enabled: true, Native: true})
		setUsed(t, h.st, id, 99)
	}
	n := h.s.nowhere(l, time.Now())
	lines := strings.Split(n.line(), "\n")[1:]
	if len(lines) != maxReasons+1 || lines[0] != "codex/acct-00: 1% left, at or below the 4% floor" || lines[maxReasons] != "and 3 more" {
		t.Fatalf("line = %q", lines)
	}
	if !strings.HasSuffix(n.event(), "; and 3 more") {
		t.Fatalf("event = %q", n.event())
	}
}

// A long list is cut between reasons, never inside one, so no command in it
// names a shorter id.
func TestReasonsAreCutWhole(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, nil)
	var ids []string
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("codex/acct-%02d-%s", i, strings.Repeat("x", 110))
		h.st.AddAccount(state.Account{ID: id, Provider: "codex", Name: id[6:], Enabled: true, Native: true})
		setUsed(t, h.st, id, 10)
		h.st.SetCLILogin(id, false, "", time.Now())
		ids = append(ids, id)
	}
	h.renames()
	h.step(t, l.ID)
	got := h.why(t, l)
	lines := strings.Split(got, "\n")
	if len(got) > state.MaxWhy || len(lines) < 3 {
		t.Fatalf("why = %d bytes: %q", len(got), got)
	}
	shown := 0
	for _, line := range lines[1 : len(lines)-1] {
		id, _, _ := strings.Cut(line, ":")
		if !strings.HasSuffix(line, "aiq account login "+id) {
			t.Fatalf("a reason was cut: %q", line)
		}
		shown++
	}
	if want := fmt.Sprintf("and %d more", 11-shown); lines[len(lines)-1] != want {
		t.Fatalf("last line %q, want %q", lines[len(lines)-1], want)
	}
}

// A login is advised only where the session can start, and a provider with
// no account says so.
func TestReasonsFollowTheLauncherChain(t *testing.T) {
	h := newHoldPool(t, false, false)
	h.st.AddAccount(state.Account{ID: "claude/acct-b", Provider: "claude", Name: "acct-b", Enabled: true, Native: true})
	setUsed(t, h.st, "claude/acct-b", 10)
	h.st.SetCLILogin("claude/acct-b", false, "", time.Now())
	l := h.lease(t, func(l *state.Lease) { l.Launcher = "box" })
	n := h.s.nowhere(l, time.Now())
	if n.login() || strings.Join(n.reasons, "|") != `codex: no other account|claude: the "box" chain has no claude launcher` {
		t.Fatalf("login %v, reasons %q", n.login(), n.reasons)
	}
	l = h.lease(t, func(l *state.Lease) { l.Fallback = "codex,agy" })
	if got := strings.Join(h.s.nowhere(l, time.Now()).reasons, "|"); got != "codex: no other account|agy: no account" {
		t.Fatalf("reasons = %q", got)
	}
}

// Showing a standing never changes a decision: the same ticks with and
// without it leave the same lease rows, events and moves.
func TestStandingDoesNotChangeDecisions(t *testing.T) {
	base := time.Now().Truncate(time.Second)
	disable := func(h *holdPool) { h.st.SetEnabled("codex/acct-a", false) }
	running := func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 }
	for _, c := range []struct {
		name          string
		codex, claude bool
		edit          func(*state.Lease)
		setup         func(*holdPool)
	}{
		{"fresh and idle", true, false, nil, nil},
		{"held with context", false, true, func(l *state.Lease) { l.SessionID, l.TurnStartedAt, l.TurnEndedAt = "0000-1111", 1, 2 }, nil},
		{"blocked, with a successor", true, false, running, disable},
		{"blocked, nowhere to go", false, false, running, disable},
		{"waiting in a turn", true, false, func(l *state.Lease) { l.Drain, l.TurnStartedAt = state.DrainWaiting, 5 }, nil},
		{"silent launcher", true, false, func(l *state.Lease) { l.Launcher = "box" }, nil},
		{"manual drain", true, false, func(l *state.Lease) { l.DrainManual = true }, nil},
	} {
		run := func(apply bool) string {
			h := newHoldPool(t, c.codex, c.claude)
			h.renames()
			h.s.windowName = func(string) (string, error) { return "", fmt.Errorf("no tmux") }
			l := h.lease(t, func(l *state.Lease) {
				l.StartedAt, l.DrainAt = base.Add(-2*time.Minute).Unix(), base.Add(-time.Minute).Unix()
				if c.edit != nil {
					c.edit(l)
				}
			})
			if c.setup != nil {
				c.setup(h)
			}
			var out []string
			// Closer together than the idle grace, so a drain clock reset
			// on every tick would hold a move back.
			for i := 0; i < 4; i++ {
				now := base.Add(time.Duration(i) * 10 * time.Second)
				cur, _ := h.st.GetLease(l.ID)
				h.s.waits = map[int64]waiting{}
				h.s.evaluate(cur, now)
				if apply {
					h.s.apply(cur, now)
				}
				got, _ := h.st.GetLease(l.ID)
				b, _ := json.Marshal(got)
				out = append(out, string(b))
			}
			return strings.Join(append(append(out, h.allEvents(t)), h.moves...), "\n")
		}
		if without, with := run(false), run(true); without != with {
			t.Errorf("%s:\nwithout:\n%s\nwith:\n%s", c.name, without, with)
		}
	}
}

// A lease re-adopted under the old one's id, start time and pane is shown
// afresh: its pid tells it from the old one.
func TestStandingOfAReadoptedLease(t *testing.T) {
	h := newHoldPool(t, false, false)
	names := h.renames()
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	h.step(t, l.ID)
	line := h.why(t, l)
	if line == "" || len(*names) != 1 {
		t.Fatalf("why %q, renames %q", line, *names)
	}
	cur, _ := h.st.GetLease(l.ID)
	h.st.ReleaseLease(l.ID)
	cur.ID, cur.PID = 0, os.Getppid() // live: the pool prunes leases whose process is gone
	id, err := h.st.AddLease(cur)
	if err != nil || id != l.ID {
		t.Fatalf("re-adopted as %d (%v), want the id %d reused", id, err, l.ID)
	}
	h.step(t, id)
	if got := h.why(t, l); got != line {
		t.Fatalf("why = %q, want %q", got, line)
	}
	if len(*names) != 2 || (*names)[1] != (*names)[0] {
		t.Fatalf("renames = %q", *names)
	}
}

// What was shown of a lease is forgotten once it is gone.
func TestStandingForgetsAGoneLease(t *testing.T) {
	h := newHoldPool(t, false, false)
	h.renames()
	l := h.lease(t, nil)
	h.s.Tick()
	if len(h.s.lines) == 0 || len(h.s.labels) == 0 {
		t.Fatalf("lines %v, labels %v", h.s.lines, h.s.labels)
	}
	h.st.ReleaseLease(l.ID)
	h.s.Tick()
	if len(h.s.lines) != 0 || len(h.s.labels) != 0 {
		t.Fatalf("lines %v, labels %v after the lease went", h.s.lines, h.s.labels)
	}
}

// After a restart, a window an earlier daemon named gets the account id
// back once the session runs normally. A name the person set is kept, unless
// it starts as aiq's own labels do, with the account id and " · ".
func TestStandingAfterARestartRestoresItsOwnName(t *testing.T) {
	for window, want := range map[string]string{
		"codex/acct-a · moves after next turn": "%1 codex/acct-a",
		"codex/acct-a":                         "",
		"my work":                              "",
		"codex/acct-a · exhausted, no account to move to: log in (aiq doctor)": "%1 codex/acct-a",
		"": "",
	} {
		h := newHoldPool(t, true, false)
		names := h.renames()
		h.s.windowName = func(pane string) (string, error) {
			if window == "" {
				return "", fmt.Errorf("no server")
			}
			return window, nil
		}
		setUsed(t, h.st, "codex/acct-a", 40)
		l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
		h.step(t, l.ID)
		h.step(t, l.ID)
		if got := strings.Join(*names, "|"); got != want {
			t.Errorf("window %q: renames %q, want %q", window, got, want)
		}
	}
}

// A failed rename is tried again, but not on every tick.
func TestStandingRetriesAFailedRename(t *testing.T) {
	h := newHoldPool(t, false, false)
	var tries, logged []string
	h.s.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	fail := true
	h.s.rename = func(pane, name string) error {
		tries = append(tries, name)
		if fail {
			return fmt.Errorf("no server")
		}
		return nil
	}
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.st.SetEnabled("codex/acct-a", false)
	t0 := time.Now()
	fail = false
	h.stepAt(t, l.ID, t0)
	h.st.SetEnabled("codex/acct-a", true)
	fail = true
	h.stepAt(t, l.ID, t0)
	for _, at := range []time.Duration{time.Minute, 6 * time.Minute, 7 * time.Minute, 12 * time.Minute, 13 * time.Minute} {
		fail = at < 12*time.Minute
		h.stepAt(t, l.ID, t0.Add(at))
	}
	if got := strings.Join(tries, "|"); got != "codex/acct-a · disabled, no account to move to|codex/acct-a|codex/acct-a|codex/acct-a" {
		t.Fatalf("tries = %q", got)
	}
	n := 0
	for _, m := range logged {
		if strings.Contains(m, "naming the window") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %q; want the failed name once", logged)
	}
}

// A line that cannot be recorded is said once, not on every tick.
func TestStandingSaysOnceWhenTheLineCannotBeRecorded(t *testing.T) {
	h := newHoldPool(t, false, false)
	h.renames()
	var logged []string
	h.s.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	l := h.lease(t, nil)
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE leases DROP COLUMN why`); err != nil {
		t.Fatal(err)
	}
	h.step(t, l.ID)
	h.step(t, l.ID)
	n := 0
	for _, m := range logged {
		if strings.Contains(m, "recording why it waits") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("logged %q", logged)
	}
}

// The line is the daemon's alone: a drain change keeps it, and a new lease
// starts without one.
func TestWhyStaysWithItsLease(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, nil)
	if err := h.st.SetLeaseWhy(l.ID, l.StartedAt, l.PID+1, "other pid"); err != nil {
		t.Fatal(err)
	}
	if h.why(t, l) != "" {
		t.Fatal("written for another pid")
	}
	h.st.SetLeaseWhy(l.ID, l.StartedAt, l.PID, "a\x1b[31m\nb")
	h.st.SetLeaseDrain(l.ID, state.DrainReady, time.Now())
	h.st.MarkTurn(l.ID, true, time.Now())
	if got := h.why(t, l); got != "a?[31m\nb" {
		t.Fatalf("why = %q", got)
	}
	cur, _ := h.st.GetLease(l.ID)
	cur.ID = 0
	id, _ := h.st.AddLease(cur)
	if h.why(t, state.Lease{ID: id, StartedAt: cur.StartedAt}) != "" {
		t.Fatal("a new lease carried the line")
	}
	v, err := (&pool.Pool{Cfg: h.s.Pool.Cfg, St: h.st}).View(0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if !strings.Contains(string(b), `"why":"a?[31m\nb"`) {
		t.Fatalf("the view has no why: %s", b)
	}
}

// Every phrase comes from the account's fields. A reason text the pool set,
// or a provider's window label, never reaches it.
func TestOutPhraseUsesFieldsOnly(t *testing.T) {
	now := time.Now()
	soon := now.Add(time.Hour).Unix()
	ok := selector.Candidate{ID: "codex/acct-b", Enabled: true, HasCredential: true}
	with := func(edit func(*selector.Candidate)) selector.Candidate { c := ok; edit(&c); return c }
	for _, c := range []struct {
		c            selector.Candidate
		phrase, word string
		login        bool
	}{
		{with(func(c *selector.Candidate) { c.Enabled = false }), "disabled", "disabled", false},
		{with(func(c *selector.Candidate) { c.HasCredential = false }), "no credential — run: aiq account login codex/acct-b", "logged out", true},
		{with(func(c *selector.Candidate) {
			c.UnavailableReason, c.Unavailable = "CLI logged in as b@example.com, not a@example.com", selector.UnavailableLoginMismatch
		}), "CLI logged in as another identity — run: aiq account login codex/acct-b", "logged out", true},
		{with(func(c *selector.Candidate) { c.Unavailable = selector.UnavailableAuthRequired }),
			"not logged in (authentication required) — run: aiq account login codex/acct-b", "logged out", true},
		{with(func(c *selector.Candidate) { c.UnavailableReason = "a@example.com" }), "unavailable", "unavailable", false},
		{with(func(c *selector.Candidate) { c.CooldownUntil, c.Unavailable = soon, selector.UnavailableLoginOut }),
			"exhausted until " + resetAt(soon, now) + ", and CLI not logged in — run: aiq account login codex/acct-b", "exhausted", false},
		{with(func(c *selector.Candidate) { c.ID, c.HasCredential = "codex/acct;b", false }),
			"no credential — run: aiq account login 'codex/acct;b'", "logged out", true},
		{with(func(c *selector.Candidate) { c.CooldownUntil = soon }), "exhausted until " + resetAt(soon, now), "exhausted", false},
		{with(func(c *selector.Candidate) {
			c.Windows = []state.Window{{Label: "<b>\x1b[31m", Kind: state.KindWeekly, UsedPct: 100, ResetsAt: soon}}
		}), "weekly window exhausted until " + resetAt(soon, now), "exhausted", false},
	} {
		phrase, word, login := outPhrase(c.c, "", now)
		if phrase != c.phrase || word != c.word || login != c.login {
			t.Errorf("got %q %q %v, want %q %q %v", phrase, word, login, c.phrase, c.word, c.login)
		}
	}
}

// An account that could serve says which pass of the search left it out.
func TestPassedOver(t *testing.T) {
	now := time.Now()
	c := selector.Candidate{Windows: []state.Window{{Kind: state.KindWeekly, UsedPct: 98, ResetsAt: now.Add(time.Hour).Unix()}}}
	spent := selector.Candidate{Windows: []state.Window{{Kind: state.KindWeekly, UsedPct: 100}}}
	for _, c := range []struct {
		c       selector.Candidate
		rung    int
		blocked bool
		want    string
	}{
		{c, selector.RungPlan, false, "2% left, at or below the 4% floor"},
		{spent, selector.RungPlan, true, "no quota left"},
		{c, selector.RungExtra, false, "extra usage; this session is already on extra usage"},
		{c, selector.RungLastResort + 1, false, "last resort; used only once codex/acct-a runs out"},
		{spent, selector.RungLastResort + 1, true, "no quota left"},
	} {
		if got := passedOver(c.c, selector.Ranked{Eligible: true, Rung: c.rung}, "", now, c.blocked, "codex/acct-a", 4); got != c.want {
			t.Errorf("rung %d blocked %v: %q, want %q", c.rung, c.blocked, got, c.want)
		}
	}
}

// A provider the search skips for the session's tier is named as such.
func TestReasonsNameATierSkip(t *testing.T) {
	h := newHoldPool(t, false, false)
	l := h.lease(t, func(l *state.Lease) { l.Args, l.Fallback = "--model gpt-6-astra", "codex,agy" })
	if got := strings.Join(h.s.nowhere(l, time.Now()).reasons, "|"); got != "codex: no other account|agy: no model of this session's tier" {
		t.Fatalf("reasons = %q", got)
	}
}

// Tick shows each lease's standing after its decision, so the tick that asks
// for a drain already says so.
func TestTickShowsTheStandingAfterTheDecision(t *testing.T) {
	h := newHoldPool(t, true, false)
	h.renames()
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.s.Tick()
	if got := h.why(t, l); got != "asks for a handoff note at its next prompt or turn end, then moves to another account" {
		t.Fatalf("why = %q", got)
	}
}

// A window name that cannot be read yet is read again at the next tick.
func TestStandingReadsTheWindowNameAgainAfterAnError(t *testing.T) {
	h := newHoldPool(t, true, false)
	names := h.renames()
	reads := 0
	h.s.windowName = func(string) (string, error) {
		reads++
		if reads == 1 {
			return "", fmt.Errorf("no server")
		}
		return "codex/acct-a · moves after next turn", nil
	}
	setUsed(t, h.st, "codex/acct-a", 40)
	l := h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.step(t, l.ID)
	if len(*names) != 0 {
		t.Fatalf("renames after a failed read: %q", *names)
	}
	for i := 0; i < 3; i++ {
		h.step(t, l.ID)
	}
	if strings.Join(*names, "|") != "%1 codex/acct-a" || reads != 2 {
		t.Fatalf("renames %q, reads %d", *names, reads)
	}
}

// What was shown of a lease is forgotten once it is gone, even when its line
// could never be recorded.
func TestStandingForgetsLabelsWithoutLines(t *testing.T) {
	h := newHoldPool(t, false, false)
	h.renames()
	l := h.lease(t, nil)
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`ALTER TABLE leases DROP COLUMN why`); err != nil {
		t.Fatal(err)
	}
	h.s.Tick()
	if len(h.s.labels) == 0 {
		t.Fatal("no label recorded")
	}
	h.st.ReleaseLease(l.ID)
	h.s.Tick()
	if len(h.s.labels) != 0 {
		t.Fatalf("labels %v after the lease went", h.s.labels)
	}
}

// extraUsage gives an account spendable extra usage.
func extraUsage(t *testing.T, st *state.Store, id string) {
	t.Helper()
	now := time.Now()
	st.SetUsageMeta(id, "", 0, "", now)
	if err := st.SetExtraUsage(id, &state.ExtraUsage{Enabled: true, Currency: "CAD", BalanceMinor: 500, ObservedAt: now.Unix(), BalanceAt: now.Unix()}); err != nil {
		t.Fatal(err)
	}
}

// addAccount adds an enabled native account with used percent of its week
// spent.
func (h *holdPool) addAccount(t *testing.T, id string, used float64) {
	t.Helper()
	provider, name, _ := strings.Cut(id, "/")
	if err := h.st.AddAccount(state.Account{ID: id, Provider: provider, Name: name, Enabled: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	setUsed(t, h.st, id, used)
}

// A login is advised for an account exactly when the search would take it
// once logged in.
func TestLoginAdviceFollowsTheSearch(t *testing.T) {
	const window = "would run on extra usage (weekly window exhausted until "
	for _, c := range []struct {
		name    string
		id      string
		used    float64
		extra   bool // the account has extra usage
		ownOnX  bool // the session's own account is on extra usage
		blocked bool // the session's own account is out
		freed   bool
		onExtra string // what a freed reason says before the login, "" for plan quota
	}{
		{"plan quota", "codex/acct-x", 50, false, false, false, true, ""},
		{"plan at the floor", "codex/acct-x", 98, false, false, false, false, ""},
		{"plan at the floor, blocked", "codex/acct-x", 98, false, false, true, true, ""},
		{"at the floor, with extra usage", "codex/acct-x", 98, true, false, false, true, "would run on extra usage (2% plan quota left"},
		{"spent, with extra usage", "codex/acct-x", 100, true, false, false, true, window},
		{"spent, with extra usage, already on extra", "codex/acct-x", 100, true, true, false, false, ""},
		{"last resort", "claude/acct-b", 50, false, false, false, false, ""},
		{"last resort, blocked", "claude/acct-b", 50, false, false, true, true, ""},
	} {
		h := newHoldPool(t, false, false)
		h.s.Pool.Cfg.Selection.ExtraUsage = true
		h.s.Pool.Cfg.Selection.LastResort = []string{"claude"}
		h.addAccount(t, c.id, c.used)
		h.st.SetCLILogin(c.id, false, "", time.Now())
		if c.extra {
			extraUsage(t, h.st, c.id)
		}
		if c.ownOnX {
			extraUsage(t, h.st, "codex/acct-a")
		}
		if c.blocked {
			h.st.MarkExhausted("codex/acct-a", "test", time.Now().Add(time.Hour), time.Now())
		}
		l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
		n := h.s.nowhere(l, time.Now())
		var reason string
		for _, r := range n.reasons {
			if strings.HasPrefix(r, c.id+": ") {
				reason = r
			}
		}
		h.st.SetCLILogin(c.id, true, "", time.Now())
		acc, err := h.s.successor(l, time.Now())
		taken := err == nil && acc.ID == c.id
		if n.others != c.freed || taken != c.freed {
			t.Errorf("%s: advised %v, taken after a login %v, want %v; reasons %q", c.name, n.others, taken, c.freed, n.reasons)
		}
		// A freed account comes first, after the session's own when it is out.
		first := n.reasons[0]
		if c.blocked {
			first = n.reasons[1]
		}
		login := "CLI not logged in — run: aiq account login " + c.id
		want := reason == c.id+": "+login
		if c.onExtra != "" {
			want = strings.HasPrefix(reason, c.id+": "+c.onExtra) && strings.HasSuffix(reason, "), and "+login)
		}
		if c.freed && (!want || first != reason) {
			t.Errorf("%s: reason %q, reasons %q", c.name, reason, n.reasons)
		}
		if !c.freed && !strings.HasSuffix(reason, ", and "+login) {
			t.Errorf("%s: reason %q, reasons %q", c.name, reason, n.reasons)
		}
	}
}

// The two cases reviewers found: an account a login frees onto extra usage,
// and one a login does not free at the floor.
func TestLoginAdviceExamples(t *testing.T) {
	h := newHoldPool(t, false, false)
	h.s.Pool.Cfg.Selection.ExtraUsage = true
	h.addAccount(t, "claude/acct-b", 98)
	h.st.SetCLILogin("claude/acct-b", false, "", time.Now())
	l := h.lease(t, func(l *state.Lease) { l.Drain = state.DrainReady })
	n := h.s.nowhere(l, time.Now())
	if n.login() || strings.Join(n.reasons, "|") != "codex: no other account|claude/acct-b: 2% left, at or below the 4% floor, and CLI not logged in — run: aiq account login claude/acct-b" {
		t.Fatalf("login %v, reasons %q", n.login(), n.reasons)
	}

	// The session's own account, logged out, on extra usage with its week spent.
	h = newHoldPool(t, false, false)
	h.s.Pool.Cfg.Selection.ExtraUsage = true
	names := h.renames()
	setUsed(t, h.st, "codex/acct-a", 100)
	extraUsage(t, h.st, "codex/acct-a")
	h.st.SetCLILogin("codex/acct-a", false, "", time.Now())
	l = h.lease(t, func(l *state.Lease) { l.Drain, l.DrainAt = state.DrainNone, 0 })
	h.step(t, l.ID)
	lines := strings.Split(h.why(t, l), "\n")
	if len(lines) != 4 || lines[0] != "its account is out and no other account can take it; it stays until one can or its own account is back. Log in again to free it: aiq doctor runs the logins." ||
		!strings.HasPrefix(lines[1], "codex/acct-a (this session's): would run on extra usage (weekly window exhausted until ") ||
		!strings.HasSuffix(lines[1], "), and CLI not logged in — run: aiq account login codex/acct-a") ||
		strings.Join(lines[2:], "|") != "codex: no other account|claude: no account" {
		t.Fatalf("why = %q", lines)
	}
	if strings.Join(*names, "|") != "%1 codex/acct-a · logged out, no account to move to: log in (aiq doctor)" {
		t.Fatalf("renames = %q", *names)
	}
}
