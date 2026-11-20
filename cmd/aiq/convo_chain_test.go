package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

const (
	chainClaudeID = "11111111-1111-4111-8111-111111111111"
	chainCodexID  = "22222222-2222-4222-8222-222222222222"
	chainNowID    = "33333333-3333-4333-8333-333333333333"
	chainGoneID   = "44444444-4444-4444-8444-444444444444"
)

func chainSession(provider, id string, prompts ...string) *transcript.Session {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: provider, ID: id, Cwd: "/w", Started: at}
	for i, p := range prompts {
		start := at.Add(time.Duration(i) * time.Minute)
		s.Turns = append(s.Turns, transcript.Turn{Prompt: p, Reply: "re " + p, Started: start, Ended: start.Add(time.Second)})
	}
	return s
}

func chainEarlier(s *transcript.Session, missed bool) convoEarlier {
	return convoEarlier{t: convoTarget{provider: s.Provider, id: s.ID, stale: true}, s: s, missed: missed,
		movedAt: time.Date(2026, 9, 1, 15, 0, 0, 0, time.Local)}
}

// --last counts the person's prompts back across the whole chain.
func TestChainFromCountsPromptsAcrossSessions(t *testing.T) {
	a := chainSession("claude", "a", "a1", "a2")
	b := chainSession("codex", "b", "b1")
	c := chainSession("claude", "c", "c1", "c2")
	cases := []struct {
		last int
		from []int
		cut  []bool
	}{
		{0, []int{0, 0, 0}, []bool{false, false, false}},
		{1, []int{2, 1, 1}, []bool{true, true, false}},
		{2, []int{2, 1, 0}, []bool{true, true, false}},
		{3, []int{2, 0, 0}, []bool{true, false, false}},
		{4, []int{1, 0, 0}, []bool{false, false, false}},
		{9, []int{0, 0, 0}, []bool{false, false, false}},
	}
	for _, tc := range cases {
		from, cut := chainFrom([]*transcript.Session{a, b, c}, tc.last)
		for i := range from {
			if from[i] != tc.from[i] || cut[i] != tc.cut[i] {
				t.Fatalf("--last %d: from %v cut %v, want %v %v", tc.last, from, cut, tc.from, tc.cut)
			}
		}
	}
}

// The snapshot prints the earlier sessions first, each under a divider and in
// its own agent's name, then where the conversation went on.
func TestRenderConvoChainShowsEarlierSessionsInOrder(t *testing.T) {
	a := chainSession("claude", chainClaudeID, "plan the migration")
	gone := chainSession("codex", chainGoneID)
	b := chainSession("codex", chainCodexID, "write the script")
	cur := chainSession("claude", chainNowID, "run it")
	earlier := []convoEarlier{chainEarlier(a, false), chainEarlier(gone, true), chainEarlier(b, false)}
	out := renderConvoChain(cur, convoTarget{provider: "claude", account: "claude/b"}, earlier, 0, false, time.Now())
	order := []string{
		"session 33333333", // the header names the session asked for
		"── earlier session · claude · session 11111111 · 2026-09-01 14:00 ──",
		"▌ you", "plan the migration", "▌ claude", "re plan the migration",
		"── earlier session · codex · session 44444444 · 2026-09-01 14:00 · transcript not available ──",
		"── earlier session · codex · session 22222222 · 2026-09-01 14:00 ──",
		"write the script", "▌ codex", "re write the script",
		"── rolled over to claude/b · session 33333333 · 2026-09-01 15:00 ──",
		"run it", "▌ claude", "re run it",
	}
	rest := out
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing or out of order %q in:\n%s", want, out)
		}
		rest = rest[i+len(want):]
	}

	// --last 1 keeps only the newest prompt: the earlier sessions drop out, and the
	// header counts every turn left out.
	out = renderConvoChain(cur, convoTarget{provider: "claude"}, earlier, 1, false, time.Now())
	if strings.Contains(out, "plan the migration") || strings.Contains(out, "earlier session") || !strings.Contains(out, "(2 earlier turns not shown)") {
		t.Fatalf("--last 1:\n%s", out)
	}
	if !strings.Contains(out, "rolled over to claude") || !strings.Contains(out, "run it") {
		t.Fatalf("--last 1 lost the current session:\n%s", out)
	}
	// Without earlier sessions the output is what it always was.
	if renderConvoChain(cur, convoTarget{}, nil, 0, false, time.Now()) != renderConvo(cur, convoTarget{}, 0, false, time.Now()) {
		t.Fatal("renderConvo differs from a chain of one")
	}
}

// The screen keeps the chain above the current session and the header on it.
func TestConvoScreenShowsTheChain(t *testing.T) {
	a := chainSession("claude", chainClaudeID, "plan the migration")
	cur := chainSession("codex", chainNowID, "run it")
	c := newConvoScreen(false)
	c.w, c.h = 100, 40
	for _, pt := range newConvoChain([]convoEarlier{chainEarlier(a, false)}, cur, convoTarget{provider: "codex"}, 0).parts {
		c.apply(convoUpdate{s: pt.s, t: pt.t, begin: true, from: pt.from, divider: pt.divider, earlier: pt.earlier, cut: pt.cut})
	}
	frame := strings.Join(plainFrame(c), "\n")
	order := []string{"session 33333333", "EARLIER SESSION · CLAUDE · SESSION 11111111", "plan the migration",
		"ROLLED OVER TO CODEX · SESSION 33333333", "run it"}
	rest := frame
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing or out of order %q in:\n%s", want, frame)
		}
		rest = rest[i+len(want):]
	}
	if got := c.msgs[c.sel].raw; !strings.Contains(got, "re run it") {
		t.Fatalf("selected %q, want the newest message", got)
	}
}

// The resolver reads the chain aiq long recorded: from the transcript the link
// names, else by id under the roots, and marks one it cannot find.
func TestConvoResolverReadsEarlierSessions(t *testing.T) {
	home := t.TempDir()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	claudeDir := filepath.Join(home, ".claude", "projects", "-w")
	codexDir := filepath.Join(home, ".codex", "sessions", "2026", "09", "15")
	for _, d := range []string{claudeDir, codexDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var c fixtureLines
	c.typed("plan the migration")
	c.answer("m1", "Planned.")
	claudePath := filepath.Join(claudeDir, chainClaudeID+".jsonl")
	codexPath := filepath.Join(codexDir, "rollout-2026-09-15T21-00-00-"+chainCodexID+".jsonl")
	codexLines := strings.ReplaceAll(strings.Join(codexConvoFixture(), "\n"), `"id":"c"`, `"id":"`+chainCodexID+`"`)
	if err := os.WriteFile(claudePath, []byte(strings.Join(c.lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte(codexLines+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	add := func(provider, prevProvider, prevID, prevTranscript, id string) {
		lid, err := st.AddLease(state.Lease{AccountID: provider + "/a", Hostname: "box", Mode: state.ModeLong, Provider: provider,
			StartedAt: time.Now().Unix(), PrevProvider: prevProvider, PrevSessionID: prevID, PrevTranscript: prevTranscript})
		if err != nil {
			t.Fatal(err)
		}
		st.SetLeaseSession(lid, id)
	}
	// The codex session's link names no transcript: the roots find it.
	add("codex", "claude", chainClaudeID, claudePath, chainCodexID)
	add("claude", "codex", chainCodexID, "", chainNowID)

	r := newConvoResolver(st, transcript.Roots{ClaudeProjects: []string{filepath.Join(home, ".claude", "projects")}, CodexSessions: []string{filepath.Join(home, ".codex", "sessions")}})
	r.host = func() string { return "box" }
	got := r.earlier(convoTarget{provider: "claude", path: filepath.Join(claudeDir, chainNowID+".jsonl")})
	if len(got) != 2 || got[0].missed || got[1].missed || got[0].t.provider != "claude" || got[1].t.provider != "codex" {
		t.Fatalf("earlier = %+v", got)
	}
	if got[0].s.Turns[0].Prompt != "plan the migration" || got[1].s.ID != chainCodexID || len(got[1].s.Turns) == 0 {
		t.Fatalf("sessions read: %+v / %+v", got[0].s, got[1].s)
	}

	// --follow prints the chain above the session it follows.
	var now fixtureLines
	now.typed("run it")
	now.answer("m9", "Ran.")
	nowPath := filepath.Join(claudeDir, chainNowID+".jsonl")
	if err := os.WriteFile(nowPath, []byte(strings.Join(now.lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	f := &convoFollow{r: r, tgt: convoTarget{provider: "claude", path: nowPath, stale: true}, w: &buf,
		statEvery: time.Hour, parseGap: time.Hour, resolveEvery: time.Hour}
	stop := make(chan struct{})
	close(stop)
	if err := f.loop(stop); err != nil {
		t.Fatal(err)
	}
	rest := buf.String()
	for _, want := range []string{"session 33333333", "earlier session · claude", "plan the migration", "earlier session · codex",
		"deploy the site", "rolled over to claude", "run it", "Ran."} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("--follow: missing or out of order %q in:\n%s", want, buf.String())
		}
		rest = rest[i+len(want):]
	}

	// A file that is gone leaves the session in the chain, marked.
	os.Remove(claudePath)
	got = r.earlier(convoTarget{provider: "claude", id: chainNowID})
	if len(got) != 2 || !got[0].missed || got[0].s == nil || len(got[0].s.Turns) != 0 || got[1].missed {
		t.Fatalf("after removal: %+v", got)
	}
	// Another host's links, or no database, give no chain.
	r.host = func() string { return "elsewhere" }
	if got := r.earlier(convoTarget{provider: "claude", id: chainNowID}); len(got) != 0 {
		t.Fatalf("another host: %+v", got)
	}
	r.st = nil
	if got := r.earlier(convoTarget{provider: "claude", id: chainNowID}); len(got) != 0 {
		t.Fatalf("no database: %+v", got)
	}
}
