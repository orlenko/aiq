package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// convoEarlier is a session an aiq long rollover went on from: the
// conversation before the one aiq convo was asked for.
type convoEarlier struct {
	t       convoTarget
	s       *transcript.Session // with no turns when it could not be read
	missed  bool                // its transcript could not be read
	movedAt time.Time           // when its successor took over, as far as aiq knows
}

// earlier finds the sessions t continues, oldest first. aiq long records the
// link when a takeover starts a fresh session; a resumed session is already one
// transcript. Without a database, or for a session with no recorded
// predecessor, there are none.
func (r *convoResolver) earlier(t convoTarget) []convoEarlier {
	if r == nil || r.st == nil || r.host == nil {
		return nil
	}
	id := t.id
	if id == "" {
		id = sessionIDOf(t.path)
	}
	chain, err := r.st.SessionChain(r.host(), t.provider, id)
	if err != nil || len(chain) == 0 {
		return nil
	}
	out := make([]convoEarlier, 0, len(chain))
	for _, l := range chain {
		out = append(out, r.loadEarlier(l))
	}
	return out
}

// loadEarlier reads one earlier session: from the transcript its successor
// recorded, else wherever the configured roots hold it. Either file must be
// that session's own.
func (r *convoResolver) loadEarlier(l state.SessionLink) convoEarlier {
	e := convoEarlier{t: convoTarget{provider: l.Provider, id: l.SessionID, stale: true, global: true}, movedAt: l.LinkedAt}
	try := func(path string) bool {
		if path == "" {
			return false
		}
		e.t.path = path
		s, err := loadConvo(e.t)
		if err != nil || s == nil {
			return false
		}
		e.s = s
		return true
	}
	if try(l.Transcript) {
		return e
	}
	if hit, err := transcript.FindByID(context.Background(), r.discovery(), l.Provider, l.SessionID, transcript.ParseSummary); err == nil && try(hit.Path) {
		return e
	}
	e.t.path, e.missed = "", true
	e.s = &transcript.Session{Provider: l.Provider, ID: l.SessionID}
	return e
}

// chainFrom is where --last N starts in each of a chain's sessions, oldest
// first: the Nth of the person's prompts counted back across all of them. A
// session wholly before that point starts at its end and is cut.
func chainFrom(sessions []*transcript.Session, last int) (from []int, cut []bool) {
	from, cut = make([]int, len(sessions)), make([]bool, len(sessions))
	if last <= 0 {
		return from, cut
	}
	n := 0
	for k := len(sessions) - 1; k >= 0; k-- {
		turns := sessions[k].Turns
		if n >= last {
			from[k], cut[k] = len(turns), true
			continue
		}
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].Source == transcript.Human {
				if n++; n == last {
					from[k] = i
					break
				}
			}
		}
	}
	return from, cut
}

// earlierDivider names an earlier session above its turns.
func earlierDivider(e convoEarlier) string {
	what := fmt.Sprintf("earlier session · %s · session %s", e.t.provider, shortID(e.t.id))
	if !e.s.Started.IsZero() {
		what += " · " + e.s.Started.Local().Format("2006-01-02 15:04")
	}
	if e.missed {
		what += " · transcript not available"
	}
	return what
}

// rolloverDivider says where the conversation went on in t, the session after
// the earlier ones.
func rolloverDivider(t convoTarget, s *transcript.Session, at time.Time) string {
	what := fmt.Sprintf("rolled over to %s · session %s", t.label(), shortID(s.ID))
	if !at.IsZero() {
		what += " · " + at.Local().Format("2006-01-02 15:04")
	}
	return what
}

// convoDivider draws a divider line in a conversation: a heavy bar in accent with
// colour, else a plain rule, with the count of turns --last leaves out below it.
func convoDivider(p *convoPrinter, what string, hidden, width int) string {
	earlier := ""
	if hidden > 0 {
		earlier = fmt.Sprintf("\n(%d earlier turn%s not shown)", hidden, plural(hidden))
	}
	if !p.color {
		return "── " + sanitize(what) + " ──" + earlier
	}
	th := kitThemeFor(false)
	bar := kitBar(" "+strings.ToUpper(what)+" ", sgrBold+th.accent, kitMarksFor(os.Getenv).bar, width)
	return kitLine(bar, "", width) + p.paint(th.dim, earlier)
}

// convoChain is a conversation with the sessions it went on from: the parts the
// text and the screen show, oldest first, the last being the session asked for.
type convoChain struct {
	parts []convoPart
}

// newConvoChain lays out earlier sessions before s, with --last counted across them.
func newConvoChain(earlier []convoEarlier, s *transcript.Session, t convoTarget, last int) convoChain {
	sessions := make([]*transcript.Session, 0, len(earlier)+1)
	for _, e := range earlier {
		sessions = append(sessions, e.s)
	}
	sessions = append(sessions, s)
	from, cut := chainFrom(sessions, last)
	var c convoChain
	for i, e := range earlier {
		c.parts = append(c.parts, convoPart{s: e.s, t: e.t, from: from[i], divider: earlierDivider(e), earlier: true, cut: cut[i]})
	}
	cur := convoPart{s: s, t: t, from: from[len(earlier)]}
	if n := len(earlier); n > 0 {
		cur.divider = rolloverDivider(t, s, earlier[n-1].movedAt)
	}
	c.parts = append(c.parts, cur)
	return c
}

// current is the part for the session asked for.
func (c convoChain) current() convoPart { return c.parts[len(c.parts)-1] }

// hidden counts the turns --last leaves out across the chain.
func (c convoChain) hidden() int { return chainHidden(c.parts) }

// chainHidden counts the turns --last leaves out of the earlier parts and the last one.
func chainHidden(parts []convoPart) int {
	n := 0
	for i, pt := range parts {
		if pt.earlier || i == len(parts)-1 {
			n += min(pt.from, len(pt.s.Turns))
		}
	}
	return n
}

// earlierText prints the earlier parts, each under its divider, and the divider of the
// session asked for, for the stream: the snapshot and --follow without a screen.
func (c convoChain) earlierText(color bool, width int) string {
	var b strings.Builder
	for _, pt := range c.parts {
		if pt.cut {
			continue
		}
		p := newConvoPrinter(pt.s.Provider, color, false)
		p.width = width
		if pt.divider != "" {
			b.WriteString("\n" + convoDivider(p, pt.divider, 0, width) + "\n")
		}
		if !pt.earlier {
			break
		}
		p.skip(pt.s.Turns[:min(pt.from, len(pt.s.Turns))])
		b.WriteString(p.emit(pt.s))
	}
	return b.String()
}

// renderConvoChain is renderConvo with the sessions s went on from above it.
func renderConvoChain(s *transcript.Session, t convoTarget, earlier []convoEarlier, last int, color bool, now time.Time) string {
	c := newConvoChain(earlier, s, t, last)
	width := termWidth(100) // read before the pager takes the terminal
	p := newConvoPrinter(s.Provider, color, false)
	p.width = width
	p.skip(s.Turns[:c.current().from])
	out := convoHeader(s, t, c.hidden(), color) + c.earlierText(color, width) + p.emit(s)
	if w := workingLabel(s, now); w != "" {
		out += "\n" + p.paint(pal.accent, w) + "\n"
	}
	return out
}
