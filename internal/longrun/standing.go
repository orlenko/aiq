package longrun

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/tmux"
)

// A long session that cannot move says why: in a line under it in
// `aiq long list` and on the status page, and in its tmux window's name. The
// daemon works both out after each decision, from the lease as it then is,
// and never feeds them back into a decision.

// Kinds of waiting, the reasons a decision could not move a lease.
const (
	waitNoSuccessor = "no successor"
	waitNoPane      = "no pane"
	waitNoLauncher  = "no launcher"
	waitRespawn     = "respawn failed"
)

// waiting is why a decision could not move a lease this tick.
type waiting struct {
	key      string   // one of the wait* kinds
	text     string   // the event to log once, "" when the caller logs its own
	none     *nowhere // for waitNoSuccessor
	provider string   // for waitNoLauncher: where it would have gone
	launcher string
}

// nowhere is why no account can take a lease over.
type nowhere struct {
	lead    string   // the first sentence
	own     bool     // logging the lease's own account in again would free it
	others  bool     // logging another account in would free one
	reasons []string // the own account first when it is out, then one per account or provider; never empty
	word    string   // the own account's short word when it is out
}

// login says whether a login would free an account.
func (n nowhere) login() bool { return n.own || n.others }

func (n nowhere) advice() string {
	switch {
	case n.others:
		return " Log in to free one: aiq doctor runs the logins."
	case n.own:
		return " Log in again to free it: aiq doctor runs the logins."
	}
	return ""
}

// line is the form stored for people: the lead, then one reason per line, as
// many whole reasons as fit the stored line. A reason is never cut, so a
// command in it is never cut either.
func (n nowhere) line() string {
	const room = len("\nand 999 more")
	b := n.lead + n.advice()
	for i, r := range n.reasons {
		rest := len(n.reasons) - i
		need := len(b) + 1 + len(r)
		if rest > 1 {
			need += room
		}
		if i == maxReasons || need > state.MaxWhy {
			return b + fmt.Sprintf("\nand %d more", rest)
		}
		b += "\n" + r
	}
	return b
}

// event is the form logged on one line.
func (n nowhere) event() string {
	rs := n.reasons
	if len(rs) > maxReasons {
		rs = append(rs[:maxReasons:maxReasons], fmt.Sprintf("and %d more", len(n.reasons)-maxReasons))
	}
	return n.lead + n.advice() + " " + strings.Join(rs, "; ")
}

// maxReasons caps the reasons a list names.
const maxReasons = 8

// noSuccessor is the wait for a lease no account can take over.
func (s *Supervisor) noSuccessor(l state.Lease, now time.Time) waiting {
	n := s.nowhere(l, now)
	return waiting{key: waitNoSuccessor, text: n.event(), none: &n}
}

// nowhere says, for every account the successor search looks at, why it did
// not take l over. It reads recorded state only.
func (s *Supervisor) nowhere(l state.Lease, now time.Time) nowhere {
	floor := s.Pool.Cfg.Long.DrainPct
	remaining, _, blocked := s.headroom(l.AccountID, l.Provider, now)
	ownRung := s.rung(l.AccountID, l.Provider, now)
	need := floor
	if blocked {
		need = 0
	}
	var n nowhere
	var own []string
	switch phrase, word, login, extra := s.ownOut(l, now); {
	case blocked:
		n.word, n.own = word, login
		n.lead = "its account is out and no other account can take it; it stays until one can or its own account is back."
		if word == "disabled" {
			n.lead = fmt.Sprintf("its account is disabled and no other account can take it; it stays until one can. aiq long stop %d ends it.", l.ID)
		}
		own = []string{l.AccountID + " (this session's): " + extra + phrase}
	case ownRung == selector.RungExtra:
		n.lead = fmt.Sprintf("it stays on extra usage until an account with more than %.0f%% plan quota frees up.", floor)
	case ownRung > selector.RungLastResort:
		n.lead = fmt.Sprintf("it stays on this last-resort account until an account with more than %.0f%% plan quota frees up.", floor)
	default:
		n.lead = fmt.Sprintf("no account to move to; it keeps going on the %.0f%% left.", remaining)
	}

	type entry struct {
		fixable bool // a login problem: listed first
		order   int  // the provider's place in the search
		id      string
		text    string
	}
	var es []entry
	for i, provider := range s.providerOrder(l) {
		if s.tierSkips(l, provider) {
			es = append(es, entry{order: i, text: provider + ": no model of this session's tier"})
			continue
		}
		// A login there would free nothing: the session cannot start there.
		if l.Launcher != "" && s.launcherFor(l, provider) == "" {
			es = append(es, entry{order: i, text: fmt.Sprintf("%s: the %q chain has no %s launcher", provider, l.Launcher, provider)})
			continue
		}
		cands, err := s.Pool.Candidates(provider)
		if err != nil {
			es = append(es, entry{order: i, text: provider + ": accounts unreadable"})
			continue
		}
		pol := s.policy(provider, now)
		ranked := map[string]selector.Ranked{}
		for _, r := range selector.Rank(pol, cands) {
			ranked[r.ID] = r
		}
		others := 0
		for _, c := range cands {
			if c.ID == l.AccountID {
				continue
			}
			others++
			r := ranked[c.ID]
			e := entry{order: i, id: c.ID}
			if r.Eligible {
				e.text = passedOver(c, r, pol.ModelScope, now, blocked, l.AccountID, floor)
			} else if phrase, _, login := outPhrase(c, pol.ModelScope, now); !login {
				e.text = phrase
			} else if r, h := loggedIn(pol, c); takes(r, h, need, ownRung, blocked) {
				// A login frees it: the search would then take it.
				e.text, e.fixable, n.others = phrase, true, true
				if r.Rung == selector.RungExtra {
					e.text = onExtra(c, pol.ModelScope, h, now) + phrase
				}
			} else if r.Eligible {
				// Logged in, the search would still pass it over.
				e.text = passedOver(c, r, pol.ModelScope, now, blocked, l.AccountID, floor) + ", and " + phrase
			} else {
				e.text = phrase
			}
			e.text = c.ID + ": " + e.text
			es = append(es, e)
		}
		switch {
		case others > 0:
		case provider == l.Provider:
			es = append(es, entry{order: i, text: provider + ": no other account"})
		default:
			es = append(es, entry{order: i, text: provider + ": no account"})
		}
	}
	sort.SliceStable(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.fixable != b.fixable {
			return a.fixable
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.id < b.id
	})
	n.reasons = own
	for _, e := range es {
		n.reasons = append(n.reasons, e.text)
	}
	if len(n.reasons) == 0 {
		n.reasons = []string{"no account to try (long.fallback names no provider)"}
	}
	return n
}

// loggedIn is how the search would rank c once its login is fixed, and the
// headroom it would then have.
func loggedIn(pol selector.Policy, c selector.Candidate) (selector.Ranked, float64) {
	c.HasCredential, c.Unavailable, c.UnavailableReason = true, "", ""
	h, _ := selector.Headroom(c.Windows, pol.ModelScope, pol.Now)
	return selector.Rank(pol, []selector.Candidate{c})[0], h
}

// takes says whether successor would take an account ranked r with headroom
// h, for a lease whose own account ranks ownRung: plan quota above need, then
// extra usage unless the session is already on it, then, once the account is
// blocked, a last resort with anything left.
func takes(r selector.Ranked, h, need float64, ownRung int, blocked bool) bool {
	switch {
	case !r.Eligible:
		return false
	case r.Rung == selector.RungPlan:
		return h > need
	case r.Rung == selector.RungExtra:
		return ownRung != selector.RungExtra
	}
	return r.Rung > selector.RungLastResort && blocked && h > 0
}

// ownOut is how the lease's own account is out, if it is: the phrase, a
// short word for the window name, and whether a login alone would let it
// serve again. extra goes before the phrase when that login would put it on
// extra usage.
func (s *Supervisor) ownOut(l state.Lease, now time.Time) (phrase, word string, login bool, extra string) {
	cands, err := s.Pool.Candidates(l.Provider)
	if err != nil {
		return "unavailable", "unavailable", false, ""
	}
	pol := s.policy(l.Provider, now)
	for _, c := range cands {
		if c.ID == l.AccountID {
			phrase, word, login = outPhrase(c, pol.ModelScope, now)
			if login {
				r, h := loggedIn(pol, c)
				login = r.Eligible
				if login && r.Rung == selector.RungExtra {
					extra = onExtra(c, pol.ModelScope, h, now)
				}
			}
			return phrase, word, login, extra
		}
	}
	return "unavailable", "unavailable", false, ""
}

// onExtra says, before a login phrase, that the login would put an account
// on extra usage, and what of its plan is spent.
func onExtra(c selector.Candidate, scope string, h float64, now time.Time) string {
	if w, ok := selector.ExhaustedWindow(c.Windows, scope, now); ok {
		return "would run on extra usage (" + windowOut(w, now) + "), and "
	}
	return fmt.Sprintf("would run on extra usage (%.0f%% plan quota left), and ", h)
}

// outPhrase says why an account cannot serve, from its fields only: never
// from UnavailableReason or a provider's text, which can name identities.
// login says whether a login alone would let it serve again; an account that
// is also exhausted names both, the command last.
func outPhrase(c selector.Candidate, scope string, now time.Time) (phrase, word string, login bool) {
	if !c.Enabled {
		return "disabled", "disabled", false
	}
	// An exhausted window holds back only an account without extra usage,
	// as in the search.
	var spent string
	if c.CooldownUntil > now.Unix() {
		spent = "exhausted until " + resetAt(c.CooldownUntil, now)
	} else if w, ok := selector.ExhaustedWindow(c.Windows, scope, now); ok && c.ExtraUsage == "" {
		spent = windowOut(w, now)
	}
	var out string
	switch {
	case !c.HasCredential:
		out = "no credential"
	case c.Unavailable == selector.UnavailableLoginOut:
		out = "CLI not logged in"
	case c.Unavailable == selector.UnavailableLoginMismatch:
		out = "CLI logged in as another identity"
	case c.Unavailable == selector.UnavailableAuthRequired:
		out = "not logged in (authentication required)"
	}
	if out != "" {
		out += " — run: aiq account login " + tmux.QuoteWord(c.ID)
	}
	switch {
	case spent != "" && out != "":
		return spent + ", and " + out, "exhausted", false
	case spent != "":
		return spent, "exhausted", false
	case out != "":
		return out, "logged out", true
	}
	return "unavailable", "unavailable", false
}

// windowOut is an exhausted window as people read it.
func windowOut(w state.Window, now time.Time) string {
	kind := "a"
	if w.Kind == state.KindShort || w.Kind == state.KindWeekly {
		kind = w.Kind
	}
	phrase := kind + " window exhausted"
	if w.ResetsAt > 0 {
		phrase += " until " + resetAt(w.ResetsAt, now)
	}
	return phrase
}

// passedOver says why the search passed over an account that could serve.
func passedOver(c selector.Candidate, r selector.Ranked, scope string, now time.Time, blocked bool, own string, floor float64) string {
	switch {
	case r.Rung == selector.RungExtra:
		return "extra usage; this session is already on extra usage"
	case r.Rung > selector.RungLastResort && !blocked:
		return "last resort; used only once " + own + " runs out"
	}
	h, _ := selector.Headroom(c.Windows, scope, now)
	if h <= 0 {
		return "no quota left"
	}
	return fmt.Sprintf("%.0f%% left, at or below the %.0f%% floor", h, floor)
}

// resetAt is a reset time as people read it: the clock today, the weekday
// further out.
func resetAt(unix int64, now time.Time) string {
	t := time.Unix(unix, 0).Local()
	if t.Sub(now) > 24*time.Hour {
		return t.Format("Mon 15:04")
	}
	return t.Format("15:04")
}

// facts is what apply learned about a lease after its decision.
type facts struct {
	blocked bool
	own     string // the own account's phrase when it is out
	word    string // and its short word
	wait    waiting
	found   bool     // some account can take it over
	none    *nowhere // why not, when none can
}

// standing is the window name and the line a lease shows, from its state
// after the decision. The line is "" for a session running normally; the
// name is "" where there is no window.
func standing(l state.Lease, f facts) (label, line string) {
	acc := tmux.SafeName(l.AccountID)
	switch {
	case l.Drain == state.DrainNone && !f.blocked:
		return acc, ""
	case l.Pane == "":
		return "", "no tmux pane recorded; it cannot be moved"
	case f.wait.key == waitNoLauncher:
		return acc + " · cannot move", fmt.Sprintf("no launcher for %s in the %q chain, so it cannot move there", f.wait.provider, f.wait.launcher)
	case f.wait.key == waitRespawn:
		return acc + " · cannot move", "the move could not start in its pane; see the daemon log"
	case !f.found && f.none != nil:
		label = acc + " · "
		if f.blocked {
			label += f.none.word + ", "
		}
		label += "no account to move to"
		if f.none.login() {
			label += ": log in (aiq doctor)"
		}
		return label, f.none.line()
	case f.blocked:
		return acc + " · " + f.word + ", moving", "it moves to another account at the next check; its account is out: " + f.own
	case l.Drain == state.DrainRequested && silentLauncher(l):
		return acc + " · moves after next turn", fmt.Sprintf(
			"no turn seen yet from launcher %q; it moves after its first reported turn, or at once if its account runs out. If it has had turns, its hooks cannot reach aiq", l.Launcher)
	case l.Drain == state.DrainRequested:
		return acc + " · moves after next turn", "asks for a handoff note at its next prompt or turn end, then moves to another account"
	case l.Drain == state.DrainDraining:
		return acc + " · writing handoff note", "writing its handoff note; moves to another account when this turn ends"
	case l.InTurn():
		return acc + " · moves when this turn ends", "wrapped up; moves to another account when the current turn ends"
	}
	return acc + " · moving", "wrapped up; moves to another account at the next check"
}

// apply shows a lease's standing after its decision. It acts only on the
// lease that was judged: a move releases it, and its id can be taken by
// another lease before this runs.
func (s *Supervisor) apply(l state.Lease, now time.Time) {
	cur, err := s.Pool.St.GetLease(l.ID)
	if err != nil || cur.StartedAt != l.StartedAt || cur.PID != l.PID || cur.Pane != l.Pane {
		return
	}
	f := facts{wait: s.waits[cur.ID], found: true}
	_, _, f.blocked = s.headroom(cur.AccountID, cur.Provider, now)
	if (cur.Drain != state.DrainNone || f.blocked) && cur.Pane != "" {
		switch f.wait.key {
		case waitNoLauncher, waitRespawn:
		case waitNoSuccessor:
			f.found, f.none = false, f.wait.none
		default:
			if _, err := s.successor(cur, now); err != nil {
				n := s.nowhere(cur, now)
				f.found, f.none = false, &n
			}
		}
		if f.blocked {
			f.own, f.word, _, _ = s.ownOut(cur, now)
		}
	}
	label, line := standing(cur, f)
	s.show(cur, label, line, now)
}

// shown names one lease in one pane, for what the daemon last showed of it.
// A re-adopted lease can take the old one's id, start time and pane; its pid
// tells them apart.
type shown struct {
	id, startedAt int64
	pid           int
	pane          string
}

// named is the window name the daemon last set for a lease, and when setting
// it last failed.
type named struct {
	label  string
	failed time.Time
}

// renameRetry is how long a failed rename waits before it is tried again.
const renameRetry = 5 * time.Minute

// show writes a lease's line and names its window, each only when it
// changed. A window first seen holds the account id, as the launch named it,
// or a name an earlier daemon gave it, which it gets back once the session
// runs normally. A name the person set is kept, unless it starts as aiq's own
// labels do, with the account id and " · ".
func (s *Supervisor) show(l state.Lease, label, line string, now time.Time) {
	if s.lines == nil {
		s.lines, s.labels = map[shown]string{}, map[shown]named{}
	}
	k := shown{l.ID, l.StartedAt, l.PID, l.Pane}
	if last, ok := s.lines[k]; !ok || last != line {
		if err := s.Pool.St.SetLeaseWhy(l.ID, l.StartedAt, l.PID, line); err == nil {
			s.lines[k] = line
		} else if !s.whyFailed {
			s.whyFailed = true
			s.Logf("lease %d: recording why it waits: %v", l.ID, err)
		}
	}
	if label == "" {
		return
	}
	last, ok := s.labels[k]
	if !ok {
		acc := tmux.SafeName(l.AccountID)
		last = named{label: acc}
		if label == acc {
			windowName := tmux.WindowName
			if s.windowName != nil {
				windowName = s.windowName
			}
			name, err := windowName(l.Pane)
			if err != nil {
				return // read again at the next tick
			}
			if strings.HasPrefix(name, acc+" · ") {
				last.label = name
			}
		}
	}
	if label == last.label && (last.failed.IsZero() || now.Sub(last.failed) < renameRetry) {
		s.labels[k] = last
		return
	}
	rename := tmux.NameWindow
	if s.rename != nil {
		rename = s.rename
	}
	s.labels[k] = named{label: label}
	if err := rename(l.Pane, label); err != nil {
		s.labels[k] = named{label: label, failed: now}
		if label != last.label {
			s.Logf("lease %d: naming the window of %s %q: %v", l.ID, l.Pane, label, err)
		}
	}
}

// providerOrder is the order the successor search walks providers in: the
// lease's fallback list, or its provider then long.fallback, with the last
// resorts last and each provider once.
func (s *Supervisor) providerOrder(l state.Lease) []string {
	order := strings.Split(l.Fallback, ",")
	if l.Fallback == "" {
		order = append([]string{l.Provider}, s.Pool.Cfg.Long.Fallback...)
	}
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := s.Pool.Cfg.LastResortRank(strings.TrimSpace(order[i])), s.Pool.Cfg.LastResortRank(strings.TrimSpace(order[j]))
		return ri < rj
	})
	seen := map[string]bool{}
	var out []string
	for _, provider := range order {
		provider = strings.TrimSpace(provider)
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	return out
}
