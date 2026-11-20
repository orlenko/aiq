package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// appChainLinks caps the links one read considers: the daemon keeps 50000.
const appChainLinks = 50000

// appChainMember is one session of an aiq long rollover chain.
type appChainMember struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// appChain is where a session sits in its chain: the chain's key, its place
// (1 is the first session) and the chain's length. Members, oldest first, are
// for the reader; a row does not carry them.
type appChain struct {
	Key     string           `json:"key"`
	Seq     int              `json:"seq"`
	Size    int              `json:"size"`
	Members []appChainMember `json:"-"`
}

// appChains links sessions into chains. Each session has at most one
// predecessor (a link per session), and each predecessor keeps the successor
// linked first. A link the predecessor does not keep (a second successor) is
// no link at all, so every member of a chain sees the same chain.
type appChains struct {
	prev, next map[appChainMember]appChainMember
}

func newAppChains(links []state.SessionLinkRow) appChains {
	c := appChains{prev: map[appChainMember]appChainMember{}, next: map[appChainMember]appChainMember{}}
	// Links come newest first: walk them oldest first so the first successor stands.
	for i := len(links) - 1; i >= 0; i-- {
		l := links[i]
		cur, prev := appChainMember{l.Provider, l.SessionID}, appChainMember{l.PrevProvider, l.PrevSessionID}
		if cur == prev || !transcript.IsFullID(cur.Provider, cur.ID) || !transcript.IsFullID(prev.Provider, prev.ID) {
			continue
		}
		if _, ok := c.prev[cur]; ok {
			continue
		}
		if _, ok := c.next[prev]; ok {
			continue // prev already went on in another session
		}
		c.prev[cur], c.next[prev] = prev, cur
	}
	return c
}

// of is provider/id's chain, or false when no rollover touches it. The chain is
// found whole, from its first session to its last, before anything limits what
// is shown, so its key and members are the same from any member. A loop has no
// first session; it starts at the session met again.
func (c appChains) of(provider, id string) (appChain, bool) {
	self := appChainMember{provider, id}
	_, hasPrev := c.prev[self]
	_, hasNext := c.next[self]
	if !hasPrev && !hasNext {
		return appChain{}, false
	}
	first, seen, looped := self, map[appChainMember]bool{self: true}, false
	for {
		p, ok := c.prev[first]
		if !ok {
			break
		}
		if seen[p] {
			looped = true
			break
		}
		seen[p] = true
		first = p
	}
	members := []appChainMember{first}
	at := map[appChainMember]bool{first: true}
	for m := first; ; {
		n, ok := c.next[m]
		if !ok || at[n] {
			break
		}
		at[n] = true
		members = append(members, n)
		m = n
	}
	seq := 0
	for i, m := range members {
		if m == self {
			seq = i + 1
		}
	}
	if len(members) < 2 || seq == 0 {
		return appChain{}, false
	}
	// A loop is keyed by its least member, so every member finds the same key.
	root := members[0]
	if looped {
		for _, m := range members {
			if m.Provider+"\x00"+m.ID < root.Provider+"\x00"+root.ID {
				root = m
			}
		}
	}
	sum := sha256.Sum256([]byte(root.Provider + "\x00" + root.ID))
	return appChain{Key: hex.EncodeToString(sum[:8]), Seq: seq, Size: len(members), Members: members}, true
}

// links reads this host's rollover links; none without a reader. ok is false
// when a reader failed, so a chain read can say its chain may be incomplete.
func (d appHistoryDeps) links(ctx context.Context) (appChains, bool) {
	if d.input.links == nil {
		return newAppChains(nil), true
	}
	links, err := d.input.links(ctx)
	if err != nil {
		return newAppChains(nil), false
	}
	return newAppChains(links), true
}

// appChainSession is one session of a chain conversation, for the reader.
type appChainSession struct {
	Provider  string `json:"provider"`
	ID        string `json:"conversation_id"`
	StartedMS int64  `json:"started_ms"`
	Available bool   `json:"available"`
}

// chainConversation is the conversation of provider/id's whole chain: every
// session's messages in order, each marked with its session and keyed with
// its session's place so cursors work across them.
func (d appHistoryDeps) chainConversation(ctx context.Context, provider, id string, s *transcript.Session, before, after string) (appHistoryOutput, error) {
	chains, linked := d.links(ctx)
	chain, ok := chains.of(provider, id)
	if !ok {
		chain = appChain{Members: []appChainMember{{provider, id}}}
	}
	// The whole chain is read, whichever member was asked for, up to
	// state.MaxChain sessions: its last ones, moved back only as far as needed to
	// keep provider/id. The rest are counted as left out.
	members, start := chain.Members, 0
	if len(members) > state.MaxChain {
		start = len(members) - state.MaxChain
		if chain.Seq > 0 {
			start = min(start, chain.Seq-1)
		}
		members = members[start : start+state.MaxChain]
	}
	left := len(chain.Members) - len(members)
	var all []appHistoryMessage
	sessions := make([]appChainSession, 0, len(members))
	skipped := 0
	for k, m := range members {
		one := s
		if m.Provider != provider || m.ID != id {
			var err error
			if one, err = d.session(ctx, m.Provider, m.ID); err != nil {
				if ctx.Err() != nil {
					return appHistoryOutput{}, appHistoryFailure("preparing")
				}
				sessions = append(sessions, appChainSession{Provider: m.Provider, ID: m.ID})
				continue
			}
		}
		sessions = append(sessions, appChainSession{Provider: m.Provider, ID: m.ID, StartedMS: appHistoryMillis(one.Started), Available: true})
		msgs, err := appHistoryMessages(ctx, one)
		if err != nil {
			return appHistoryOutput{}, err
		}
		for i := range msgs {
			// Keys count from the chain's first session, which a rollover leaves in
			// place, so a cursor still names its message after the window moves.
			place := k
			msgs[i].Key = fmt.Sprintf("s%d-%s", start+k, msgs[i].Key)
			msgs[i].Session = &place
		}
		all = append(all, msgs...)
		skipped += one.Skipped
		for _, t := range one.Turns {
			skipped += t.Skipped
		}
	}
	data, partial, err := appHistorySlice(s, all, -1, before, after)
	if err != nil {
		return appHistoryOutput{}, err
	}
	data["sessions"] = sessions
	out := appHistoryOutput{Data: data, Partial: partial, Omitted: map[string]int{}}
	if skipped > 0 {
		out.Partial = true
		out.Omitted["records"] = skipped
	}
	for _, one := range sessions {
		if !one.Available {
			out.Omitted["sessions"]++
		}
	}
	if left > 0 {
		out.Omitted["chain_limit"] = left
	}
	if !linked {
		out.Omitted["chain_unknown"] = 1
	}
	if out.Omitted["sessions"] == 0 {
		delete(out.Omitted, "sessions")
	}
	if len(out.Omitted) > 0 {
		out.Partial = true
	}
	return out, nil
}
