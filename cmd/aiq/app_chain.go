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
// (1 is the first session), and the members, oldest first.
type appChain struct {
	Key     string           `json:"key"`
	Seq     int              `json:"seq"`
	Size    int              `json:"size"`
	Members []appChainMember `json:"members"`
}

// appChains links sessions into chains. Each session has at most one
// predecessor (a link per session) and is given the successor linked first.
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
		if _, ok := c.prev[cur]; !ok {
			c.prev[cur] = prev
		}
		if _, ok := c.next[prev]; !ok {
			c.next[prev] = cur
		}
	}
	return c
}

// of is provider/id's chain, or false when no rollover touches it. Loops and
// chains past state.MaxChain sessions are cut.
func (c appChains) of(provider, id string) (appChain, bool) {
	self := appChainMember{provider, id}
	_, hasPrev := c.prev[self]
	_, hasNext := c.next[self]
	if !hasPrev && !hasNext {
		return appChain{}, false
	}
	seen := map[appChainMember]bool{self: true}
	before := []appChainMember{}
	for m := self; len(before) < state.MaxChain; {
		p, ok := c.prev[m]
		if !ok || seen[p] {
			break
		}
		seen[p] = true
		before = append(before, p)
		m = p
	}
	members := make([]appChainMember, 0, len(before)+1)
	for i := len(before) - 1; i >= 0; i-- {
		members = append(members, before[i])
	}
	members = append(members, self)
	seq := len(members)
	for m := self; len(members) < 2*state.MaxChain+1; {
		n, ok := c.next[m]
		// Only a successor that names m as its own predecessor continues it.
		if !ok || seen[n] || c.prev[n] != m {
			break
		}
		seen[n] = true
		members = append(members, n)
		m = n
	}
	if len(members) < 2 {
		return appChain{}, false
	}
	sum := sha256.Sum256([]byte(members[0].Provider + "\x00" + members[0].ID))
	return appChain{Key: hex.EncodeToString(sum[:8]), Seq: seq, Size: len(members), Members: members}, true
}

// links reads this host's rollover links; nil without a reader.
func (d appHistoryDeps) links(ctx context.Context) appChains {
	if d.input.links == nil {
		return newAppChains(nil)
	}
	links, err := d.input.links(ctx)
	if err != nil {
		return newAppChains(nil)
	}
	return newAppChains(links)
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
	chain, ok := d.links(ctx).of(provider, id)
	if !ok {
		chain = appChain{Members: []appChainMember{{provider, id}}}
	}
	var all []appHistoryMessage
	sessions := make([]appChainSession, 0, len(chain.Members))
	skipped := 0
	for k, m := range chain.Members {
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
			place := k
			msgs[i].Key = fmt.Sprintf("s%d-%s", k, msgs[i].Key)
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
			out.Partial = true
			out.Omitted["sessions"]++
		}
	}
	return out, nil
}
