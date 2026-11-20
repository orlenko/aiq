package state

import (
	"strconv"
	"testing"
	"time"
)

func chainIDs(t *testing.T, st *Store, host, provider, id string) []string {
	t.Helper()
	chain, err := st.SessionChain(host, provider, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range chain {
		out = append(out, l.Provider+":"+l.SessionID)
	}
	return out
}

func longLease(st *Store, t *testing.T, l Lease) int64 {
	t.Helper()
	l.Mode, l.Hostname, l.StartedAt = ModeLong, "here", time.Now().Unix()
	if l.AccountID == "" {
		l.AccountID = l.Provider + "/a"
	}
	id, err := st.AddLease(l)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// A fresh successor links to the session it continues once a hook reports its
// id; the chain then reads back oldest first, across providers.
func TestSessionChainFollowsFreshSuccessors(t *testing.T) {
	st := openTemp(t)
	a := longLease(st, t, Lease{Provider: "claude", Workspace: "/w"})
	st.SetLeaseSession(a, "aaaa")
	b := longLease(st, t, Lease{Provider: "codex", Workspace: "/w", PrevProvider: "claude", PrevSessionID: "aaaa", PrevTranscript: "/t/aaaa.jsonl"})
	st.SetLeaseSession(b, "bbbb")
	c := longLease(st, t, Lease{Provider: "claude", Workspace: "/w", PrevProvider: "codex", PrevSessionID: "bbbb"})
	st.SetLeaseSession(c, "cccc")
	st.SetLeaseSession(c, "cccc") // hooks report again; the link stands

	if got := chainIDs(t, st, "here", "claude", "cccc"); len(got) != 2 || got[0] != "claude:aaaa" || got[1] != "codex:bbbb" {
		t.Fatalf("chain of cccc = %v", got)
	}
	chain, _ := st.SessionChain("here", "codex", "bbbb")
	if len(chain) != 1 || chain[0].Transcript != "/t/aaaa.jsonl" || chain[0].Workspace != "/w" || chain[0].LinkedAt.IsZero() {
		t.Fatalf("chain of bbbb = %+v", chain)
	}
	if got := chainIDs(t, st, "here", "claude", "aaaa"); len(got) != 0 {
		t.Fatalf("aaaa continues nothing, got %v", got)
	}
	if got := chainIDs(t, st, "elsewhere", "claude", "cccc"); len(got) != 0 {
		t.Fatalf("another host's chain = %v", got)
	}
}

// A resume that kept its id is the same conversation: no link to itself. Only
// the first id a lease reports is the session the takeover started. The hook
// reports a kept id with SettleLeasePredecessor, not SetLeaseSession.
func TestSessionChainSkipsAResumeThatKeptItsID(t *testing.T) {
	st := openTemp(t)
	b := longLease(st, t, Lease{Provider: "claude", SessionID: "aaaa", PrevProvider: "claude", PrevSessionID: "aaaa"})
	st.SettleLeasePredecessor(b)
	if got := chainIDs(t, st, "here", "claude", "aaaa"); len(got) != 0 {
		t.Fatalf("self link: %v", got)
	}
	// /clear in the resumed session: a new conversation, not a rollover.
	st.SetLeaseSession(b, "a2a2")
	if got := chainIDs(t, st, "here", "claude", "a2a2"); len(got) != 0 {
		t.Fatalf("cleared session linked: %v", got)
	}
	// A resume the CLI gave a new id from the start does continue the old one.
	f := longLease(st, t, Lease{Provider: "claude", SessionID: "aaaa", PrevProvider: "claude", PrevSessionID: "aaaa"})
	st.SetLeaseSession(f, "fork")
	if got := chainIDs(t, st, "here", "claude", "fork"); len(got) != 1 || got[0] != "claude:aaaa" {
		t.Fatalf("forked resume chain = %v", got)
	}
}

// A link a busy database kept from being written is written when the next
// hook reports the same id, and the predecessor is cleared then.
func TestSettleWritesALinkTheFirstHookMissed(t *testing.T) {
	st := openTemp(t)
	b := longLease(st, t, Lease{Provider: "codex", PrevProvider: "claude", PrevSessionID: "aaaa"})
	// The first hook's session id landed, its link did not.
	st.db.Exec(`UPDATE leases SET session_id = 'bbbb' WHERE id = ?`, b)
	st.SettleLeasePredecessor(b)
	if got := chainIDs(t, st, "here", "codex", "bbbb"); len(got) != 1 || got[0] != "claude:aaaa" {
		t.Fatalf("bbbb chain = %v", got)
	}
	st.SetLeaseSession(b, "cccc")
	if got := chainIDs(t, st, "here", "codex", "cccc"); len(got) != 0 {
		t.Fatalf("cleared session linked: %v", got)
	}
}

// /clear in a fresh successor starts a new conversation: it is not linked to
// the session the takeover continued.
func TestSessionChainIgnoresAClearAfterTheTakeover(t *testing.T) {
	st := openTemp(t)
	b := longLease(st, t, Lease{Provider: "codex", PrevProvider: "claude", PrevSessionID: "aaaa"})
	st.SetLeaseSession(b, "bbbb")
	st.SetLeaseSession(b, "cccc")
	if got := chainIDs(t, st, "here", "codex", "cccc"); len(got) != 0 {
		t.Fatalf("cleared session linked: %v", got)
	}
	if got := chainIDs(t, st, "here", "codex", "bbbb"); len(got) != 1 || got[0] != "claude:aaaa" {
		t.Fatalf("bbbb chain = %v", got)
	}
}

// A lease id reused by a later session without a predecessor links nothing,
// and the first link recorded for a session is the one kept.
func TestSessionChainIgnoresLeasesWithoutAPredecessor(t *testing.T) {
	st := openTemp(t)
	b := longLease(st, t, Lease{Provider: "codex", PrevProvider: "claude", PrevSessionID: "aaaa"})
	st.SetLeaseSession(b, "bbbb")
	st.ReleaseLease(b)
	reused := longLease(st, t, Lease{ID: b, Provider: "codex"})
	st.SetLeaseSession(reused, "dddd")
	if got := chainIDs(t, st, "here", "codex", "dddd"); len(got) != 0 {
		t.Fatalf("plain session got a chain: %v", got)
	}
	if got := chainIDs(t, st, "here", "codex", "bbbb"); len(got) != 1 || got[0] != "claude:aaaa" {
		t.Fatalf("bbbb chain = %v", got)
	}
}

// Links that loop, or run longer than MaxChain, still end.
func TestSessionChainStopsOnLoopsAndLength(t *testing.T) {
	st := openTemp(t)
	link := func(prov, id, prevProv, prevID string) {
		st.db.Exec(`INSERT INTO session_links (hostname, provider, session_id, prev_provider, prev_session_id, linked_at_ms) VALUES ('here',?,?,?,?,1)`,
			prov, id, prevProv, prevID)
	}
	link("claude", "x1", "codex", "x2")
	link("codex", "x2", "claude", "x1")
	if got := chainIDs(t, st, "here", "claude", "x1"); len(got) != 1 || got[0] != "codex:x2" {
		t.Fatalf("loop chain = %v", got)
	}
	for i := 0; i < MaxChain+10; i++ {
		link("claude", "n"+strconv.Itoa(i+1), "claude", "n"+strconv.Itoa(i))
	}
	if got := chainIDs(t, st, "here", "claude", "n"+strconv.Itoa(MaxChain+10)); len(got) != MaxChain {
		t.Fatalf("long chain has %d members, want %d", len(got), MaxChain)
	}
}
