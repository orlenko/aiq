package asks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

type stubScorer struct {
	seen []Entry
	err  func(Entry) error
}

func (s *stubScorer) Score(_ context.Context, e Entry) (state.AskPlan, error) {
	if s.err != nil {
		if err := s.err(e); err != nil {
			return state.AskPlan{}, err
		}
	}
	s.seen = append(s.seen, e)
	if e.Kind == EntryAnswer && strings.Contains(e.Text, "?") {
		return state.AskPlan{Inserts: []state.Ask{{Chain: e.Chain, Workspace: e.Workspace, Provider: e.Provider, SessionID: e.SessionID, RaisedAt: e.At, FirstQuote: e.Text}}}, nil
	}
	return state.AskPlan{}, nil
}

func (s *stubScorer) texts() []string {
	out := []string{}
	for _, e := range s.seen {
		out = append(out, e.Text)
	}
	return out
}

type fixture struct {
	t      *testing.T
	dir    string
	path   string
	reg    *state.Store
	leases *state.Store
	lease  int64
	clock  time.Time
	lines  []string
	sc     *stubScorer
	w      *Watcher
	n      int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	reg, err := state.Open(filepath.Join(dir, "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if err := reg.EnsureAsks(); err != nil {
		t.Fatal(err)
	}
	leases, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leases.Close() })
	path := filepath.Join(dir, "s.jsonl")
	id, err := leases.AddLease(state.Lease{Mode: state.ModeLong, Hostname: "h", Cwd: "/w", Workspace: "/w", Provider: "claude", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := leases.SetLeaseTranscript(id, path); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: dir, path: path, reg: reg, leases: leases, lease: id, clock: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC), sc: &stubScorer{}}
	f.w = &Watcher{Asks: reg, Leases: leases, Scorer: f.sc, Hostname: "h", Now: func() time.Time { return f.clock }}
	return f
}

func (f *fixture) ts() string {
	f.n++
	return f.clock.Add(-time.Hour).Add(time.Duration(f.n) * time.Second).Format(time.RFC3339)
}

func (f *fixture) user(text string) {
	f.lines = append(f.lines, fmt.Sprintf(`{"type":"user","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"message":{"role":"user","content":%q}}`, f.ts(), text))
}

func (f *fixture) agent(text string) {
	f.n++
	f.lines = append(f.lines, fmt.Sprintf(`{"type":"assistant","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"message":{"id":"m%d","model":"claude-opus-5","stop_reason":"end_turn","role":"assistant","content":[{"type":"text","text":%q}]}}`, f.ts(), f.n, text))
}

// end closes the turn the way current Claude Code does.
func (f *fixture) end() {
	f.lines = append(f.lines, fmt.Sprintf(`{"type":"system","subtype":"turn_duration","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"durationMs":1}`, f.ts()))
}

// write puts the file on disk, last written at the clock minus age.
func (f *fixture) write(age time.Duration) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(strings.Join(f.lines, "\n")+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	at := f.clock.Add(-age)
	if err := os.Chtimes(f.path, at, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) pass() Result {
	f.t.Helper()
	return f.w.Pass(context.Background())
}

func TestAskVisibleBeforeTheReply(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("Your call: A or B?")
	f.write(2 * time.Minute)
	if res := f.pass(); res.Err != nil {
		t.Fatal(res.Err)
	}
	items, _ := f.reg.ListAsks("/w")
	if len(items) != 1 {
		t.Fatalf("items = %d: an ask must show while the agent waits, entries %q", len(items), f.sc.texts())
	}
}

func TestHeldAnswerIsReadOnceQuietWithNoNewWrite(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("Your call: A or B?")
	f.write(10 * time.Second)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 {
		t.Fatalf("a held answer was scored: %d", len(items))
	}
	f.clock = f.clock.Add(2 * time.Minute)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("items = %d: the hold must end with no new write", len(items))
	}
}

// A pass that outlasts the quiet time does not release an answer still being written: the
// file is checked again first.
func TestHeldAnswerStaysHeldWhileTheFileGrows(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("Your call: A or B?")
	f.write(10 * time.Second)
	f.sc.err = func(e Entry) error {
		if e.Text == "do it" {
			f.clock = f.clock.Add(2 * time.Minute)
			f.agent("Or C, once the build is done?")
			f.write(0)
		}
		return nil
	}
	f.pass()
	if got := f.sc.texts(); len(got) != 1 {
		t.Fatalf("scored %q: a draft answer was released", got)
	}
	f.sc.err = nil
	f.clock = f.clock.Add(2 * time.Minute)
	f.pass()
	if got := f.sc.texts(); len(got) != 3 || got[2] != "Or C, once the build is done?" {
		t.Fatalf("after the file went quiet, scored %q", got)
	}
}

func TestNothingIsScoredTwice(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.pass()
	before := len(f.sc.seen)
	f.pass()
	f.w.stamps = nil // as after a restart
	f.pass()
	if len(f.sc.seen) != before {
		t.Fatalf("scored again: %q", f.sc.texts())
	}
	f.user("p1")
	f.agent("a1")
	f.end()
	f.write(time.Hour)
	f.pass()
	if got := f.sc.texts(); len(got) != before+2 || got[before] != "p1" {
		t.Fatalf("new turn: %q", got)
	}
}

func TestGapIsNeverSkippedAfterAnOutage(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.pass()
	f.sc.err = func(Entry) error { return &ModelError{Class: ClassServer, Status: 503} }
	for i := 1; i < 30; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	if res := f.pass(); res.Err == nil {
		t.Fatal("no error reported")
	}
	st, _ := f.reg.GetAsksStatus(f.clock)
	if st.LastError == "" || st.Skipped != 0 || st.Waiting == 0 {
		t.Fatalf("%+v", st)
	}
	f.sc.err = nil
	f.sc.seen = nil
	f.pass()
	if got := f.sc.texts(); len(got) != 58 {
		t.Fatalf("after the outage scored %d", len(got))
	}
}

func TestAuthFailureStopsAndSkipsNothing(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	calls := 0
	f.sc.err = func(Entry) error { calls++; return &ModelError{Class: ClassAuth, Status: 401} }
	res := f.pass()
	if res.Err == nil || res.RetryAfter == 0 || calls != 1 {
		t.Fatalf("res %+v calls %d", res, calls)
	}
	row, _ := f.reg.GetAskSession("claude", "s")
	if row.Skipped != 0 || row.Position.ItemsDone != 0 {
		t.Fatalf("%+v", row)
	}
}

func TestRejectedEntriesStopAfterThreeInARow(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	f.sc.err = func(Entry) error { return &ModelError{Class: ClassRejected, Status: 400} }
	if res := f.pass(); res.Err == nil {
		t.Fatal("no stop")
	}
	row, _ := f.reg.GetAskSession("claude", "s")
	if row.Skipped != 3 {
		t.Fatalf("skipped = %d", row.Skipped)
	}
}

func TestMalformedEntryIsSkippedAfterFiveTries(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	bad := true
	f.sc.err = func(e Entry) error {
		if bad && e.Text == "p0" {
			return &ModelError{Class: ClassMalformed}
		}
		return nil
	}
	for i := 0; i < 5; i++ {
		f.w.stamps = nil
		f.pass()
	}
	row, _ := f.reg.GetAskSession("claude", "s")
	if row.Skipped != 1 {
		t.Fatalf("skipped = %d", row.Skipped)
	}
	f.pass()
	if got := f.sc.texts(); len(got) != 1 || got[0] != "a0" {
		t.Fatalf("%q", got)
	}
}

func TestPanicInRulesDoesNotEscape(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.sc.err = func(Entry) error { panic("quoted text") }
	var logged string
	f.w.Logf = func(format string, args ...any) { logged += fmt.Sprintf(format, args...) }
	f.pass()
	if strings.Contains(logged, "quoted text") {
		t.Fatalf("panic value logged: %s", logged)
	}
}

func TestOffWorkspaceSendsNothingAndSkipsItsGap(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.reg.SetAskWorkspaceOff("/w", true, f.clock)
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("sent while off: %q", f.sc.texts())
	}
	f.user("p1")
	f.agent("a1")
	f.end()
	f.write(time.Hour)
	f.pass()
	f.reg.SetAskWorkspaceOff("/w", false, f.clock)
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("text from the off time was sent: %q", f.sc.texts())
	}
	f.clock = f.clock.Add(2 * time.Hour) // what follows is written after it is back on
	f.user("p2")
	f.agent("a2")
	f.end()
	f.write(time.Hour)
	f.pass()
	if got := f.sc.texts(); len(got) != 2 || got[0] != "p2" {
		t.Fatalf("%q", got)
	}
}

func TestMissingKeySendsNothing(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.w.KeyOK = func() bool { return false }
	res := f.pass()
	if res.Err == nil || len(f.sc.seen) != 0 {
		t.Fatalf("%+v %q", res, f.sc.texts())
	}
}

func TestRepliesLeaveOutPastesToolOutputAndCommands(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/compact", ""},
		{"<pasted_content id=1>env dump</pasted_content>", ""},
		{"<pasted_content id=1>env dump</pasted_content>\nfix it", "fix it"},
		{"<bash-stdout>x</bash-stdout>", ""},
		{"go with A", "go with A"},
	} {
		got, ok := replyText(tc.in)
		if (tc.want != "") != ok || got != tc.want {
			t.Errorf("%q: %q %v", tc.in, got, ok)
		}
	}
}

func TestEndedSessionIsReadToItsEndThenDropped(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.write(time.Hour)
	f.pass()
	if err := f.leases.ReleaseLease(f.lease); err != nil {
		t.Fatal(err)
	}
	f.agent("Your call: A or B?")
	f.end()
	f.write(time.Hour)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("the final answer was lost: %d", len(items))
	}
	rows, _ := f.reg.ListAskSessions()
	if len(rows) != 0 {
		t.Fatalf("still reading %d", len(rows))
	}
}

func TestErrorTextIsFixed(t *testing.T) {
	for _, e := range []*ModelError{{Class: ClassAuth, Status: 401}, {Class: ClassServer}, {Class: ClassRate}, {Class: ClassMalformed}, {Class: ClassRejected, Status: 413}} {
		if strings.ContainsAny(e.Error(), "\"/") {
			t.Errorf("%q", e.Error())
		}
	}
	if classOf(errors.New("x")) != ClassServer {
		t.Error("an unknown error must count as a service failure")
	}
}

type countModel struct{ calls int }

func (m *countModel) Ask(context.Context, string, map[string]Question) (map[string]float64, error) {
	m.calls++
	return map[string]float64{}, nil
}

func TestCallsAreCountedAndNeverRefused(t *testing.T) {
	f := newFixture(t)
	inner := &countModel{}
	c := &CountingModel{Inner: inner, St: f.reg, Now: func() time.Time { return f.clock }}
	const many = 6000
	for i := 0; i < many; i++ {
		if _, err := c.Ask(context.Background(), "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != many {
		t.Fatalf("sent %d of %d", inner.calls, many)
	}
	if st, _ := f.reg.GetAsksStatus(f.clock); st.CallsToday != many {
		t.Fatalf("counted %d", st.CallsToday)
	}
}

func TestRunningPassReportsItself(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	var during state.AsksStatus
	f.sc.err = func(Entry) error {
		during, _ = f.reg.GetAsksStatus(f.clock)
		return nil
	}
	f.pass()
	if during.PassAt == 0 {
		t.Fatal("a running pass wrote no status, so it reads as no watcher")
	}
}

func TestCLIErrorIsNotAnAsk(t *testing.T) {
	f := newFixture(t)
	f.user("do it")
	f.agent("API Error: Can't reach the API server, why?")
	f.end()
	f.write(time.Hour)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 {
		t.Fatalf("an API error became an ask: %d", len(items))
	}
}

func TestStartsFromTheWatchersStart(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.user(fmt.Sprintf("old p%d", i))
		f.agent("Old: your call?")
		f.end()
	}
	f.write(time.Hour)
	f.w.Start = f.clock
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("read from before the start: %q", f.sc.texts())
	}
	st, _ := f.reg.GetAsksStatus(f.clock)
	if st.Waiting != 0 || st.Skipped != 0 {
		t.Fatalf("%+v", st)
	}
	f.clock = f.clock.Add(2 * time.Hour)
	f.user("new p")
	f.agent("New: your call?")
	f.end()
	f.write(time.Minute * 5)
	f.pass()
	if got := f.sc.texts(); len(got) != 2 || got[0] != "new p" {
		t.Fatalf("%q", got)
	}
}

// A resumed long session is known from its launch, before its first prompt.
// What it wrote before aiq supervised it is not read, even after the watcher started.
func TestResumedSessionStartsAtItsLease(t *testing.T) {
	f := newFixture(t)
	f.leases.ReleaseLease(f.lease)
	id, err := f.leases.AddLease(state.Lease{Mode: state.ModeLong, Hostname: "h", Cwd: "/w", Workspace: "/w", Provider: "claude",
		SessionID: "s", Transcript: f.path, StartedAt: f.clock.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	f.lease = id
	f.user("before the lease")
	f.agent("Before: your call?")
	f.end()
	f.write(time.Hour)
	f.w.Start = f.clock.Add(-2 * time.Hour)
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("read from before the lease: %q", f.sc.texts())
	}
	f.clock = f.clock.Add(2 * time.Hour)
	f.user("after")
	f.agent("After: your call?")
	f.end()
	f.write(time.Minute * 5)
	f.pass()
	if got := f.sc.texts(); len(got) != 2 || got[0] != "after" {
		t.Fatalf("%q", got)
	}
}

func TestResetErasesAndOldTextStaysGone(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Your call: A or B?")
	f.end()
	f.write(time.Hour)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	n, err := f.reg.ResetAsks(func(w string) bool { return w == "/w" }, f.clock)
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	f.sc.seen = nil
	f.w.stamps = nil
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 || len(f.sc.seen) != 0 {
		t.Fatalf("old text came back: %d items, %q", len(items), f.sc.texts())
	}
	f.clock = f.clock.Add(2 * time.Hour)
	f.user("p1")
	f.agent("Next: your call?")
	f.end()
	f.write(5 * time.Minute)
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 1 {
		t.Fatalf("new asks after a reset: %d", len(items))
	}
}

func TestPlanScoredBeforeAResetDoesNotLand(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Your call: A or B?")
	f.end()
	f.write(time.Hour)
	f.sc.err = func(e Entry) error {
		if e.Kind == EntryAnswer {
			f.reg.ResetAsks(func(string) bool { return true }, f.clock)
		}
		return nil
	}
	f.pass()
	if items, _ := f.reg.ListAsks("/w"); len(items) != 0 {
		t.Fatalf("a plan from before the reset landed: %d", len(items))
	}
}

// A turned-off session in a running turn still moves past what it writes, and turning it
// back on reads only what comes after.
func TestOffWorkspaceInATurnSendsNothingAfterward(t *testing.T) {
	f := newFixture(t)
	if err := f.leases.MarkTurn(f.lease, true, f.clock); err != nil {
		t.Fatal(err)
	}
	f.reg.SetAskWorkspaceOff("/w", true, f.clock)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Second)
	f.pass()
	f.reg.SetAskWorkspaceOff("/w", false, f.clock)
	f.clock = f.clock.Add(time.Hour)
	f.leases.MarkTurn(f.lease, false, f.clock)
	f.write(time.Hour)
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("text from the off time was sent: %q", f.sc.texts())
	}
}

// Turning a workspace off during a pass stops what it sends next.
func TestOffDuringAPassStopsSending(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	f.sc.err = func(e Entry) error {
		if e.Text == "a0" {
			f.reg.SetAskWorkspaceOff("/w", true, f.clock)
		}
		return nil
	}
	f.pass()
	if got := f.sc.texts(); len(got) != 2 {
		t.Fatalf("sent after it was turned off: %q", got)
	}
}

// A session read to its end retires once its lease is gone and it has gone quiet, with no
// new write.
func TestReadSessionRetiresWithNoNewWrite(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.pass()
	if err := f.leases.ReleaseLease(f.lease); err != nil {
		t.Fatal(err)
	}
	f.pass()
	if rows, _ := f.reg.ListAskSessions(); len(rows) != 0 {
		t.Fatalf("still watched: %+v", rows)
	}
}

// Replies that need no model call do not end a run of rejected answers.
func TestRejectionsStopThePassAcrossReplies(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 10; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
	}
	f.write(time.Hour)
	f.sc.err = func(e Entry) error {
		if e.Kind == EntryAnswer {
			return &ModelError{Class: ClassRejected, Status: 422}
		}
		return nil
	}
	res := f.pass()
	st, _ := f.reg.GetAsksStatus(f.clock)
	if res.Err == nil || st.Skipped != maxSkipsInRow {
		t.Fatalf("result %+v, skipped %d", res, st.Skipped)
	}
}

// After a restart the watcher goes on from the new place instead of stalling on the old one.
func TestRestartKeepsTheWatcherGoing(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	f.pass()
	f.reg.RestartAskSessions(f.clock)
	f.clock = f.clock.Add(2 * time.Hour)
	f.sc.seen = nil
	for i := 1; i < 3; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("a%d", i))
		f.end()
		f.write(time.Hour)
		f.pass()
	}
	if got := f.sc.texts(); len(got) != 4 || got[0] != "p1" || got[3] != "a2" {
		t.Fatalf("%q", got)
	}
}

// A turn's reply with no answer item is cut off by the turn's start, since the turn's end
// can be a later write of something else.
func TestFallbackReplyIsCutOffByTheTurnStart(t *testing.T) {
	w := &Watcher{}
	cut := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	wk := &work{row: state.AskSession{NotBefore: cut.UnixMilli(), Position: state.AskPosition{Gen: 3}},
		turns: []transcript.Turn{{Prompt: "p", Source: transcript.Human, Started: cut.Add(-time.Minute), Ended: cut.Add(time.Minute), Reply: "old reply"}}}
	for _, st := range w.pending(wk) {
		if !st.skip {
			t.Fatalf("sent from before the cutoff: %+v", st)
		}
		if st.to.Gen != 3 {
			t.Fatalf("position lost its generation: %+v", st.to)
		}
	}
}

// Two sessions of one workspace are read in the order their texts were written, so a
// successor's answer comes after the question it answers.
func TestSessionsOfOneChainGoInWrittenOrder(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Pick A or B?")
	f.end()
	f.write(time.Hour)
	successorReplies(t, f, "A")
	f.pass()
	if q, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); q < 0 || a < 0 || a < q {
		t.Fatalf("order %q", f.sc.texts())
	}
}

// A prompt the watcher skips, such as an orchestra notice, does not let a later session
// of the chain go before the question after it.
func TestSkippedPromptKeepsTheChainInOrder(t *testing.T) {
	f := newFixture(t)
	f.user(transcript.OrchestraNotice + " Read the example inbox.")
	f.agent("Pick A or B?")
	f.end()
	f.write(time.Hour)
	successorReplies(t, f, "A")
	f.pass()
	if q, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); q < 0 || a < 0 || a < q {
		t.Fatalf("order %q", f.sc.texts())
	}
}

// A question that fails this pass holds back the reply to it in a later session until it
// is tried again.
func TestFailedQuestionHoldsBackItsReply(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Pick A or B?")
	f.end()
	f.write(time.Hour)
	successorReplies(t, f, "A")
	failed := false
	f.sc.err = func(e Entry) error {
		if e.Text == "Pick A or B?" && !failed {
			failed = true
			return &ModelError{Class: ClassMalformed}
		}
		return nil
	}
	f.pass()
	if _, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); a >= 0 {
		t.Fatalf("the reply went first: %q", f.sc.texts())
	}
	f.sc.seen = nil
	f.pass()
	if q, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); q < 0 || a < 0 || a < q {
		t.Fatalf("order on the next pass %q", f.sc.texts())
	}
}

// A question still held, as its session may still be writing it, holds back the reply to it
// in a later session until the hold ends.
func TestHeldQuestionHoldsBackItsReply(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Pick A or B?")
	f.write(10 * time.Second)
	successorReplies(t, f, "A")
	f.pass()
	if _, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); a >= 0 {
		t.Fatalf("the reply went before its held question: %q", f.sc.texts())
	}
	f.clock = f.clock.Add(2 * time.Minute)
	f.pass()
	if q, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); q < 0 || a < 0 || a < q {
		t.Fatalf("order once quiet %q", f.sc.texts())
	}
}

// A session in a running turn is not read, but still holds back later texts of its chain:
// the reply to its question waits for the turn to end.
func TestRunningTurnHoldsBackItsChain(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.agent("Pick A or B?")
	f.end()
	f.write(time.Minute)
	successorReplies(t, f, "A")
	if err := f.leases.MarkTurn(f.lease, true, f.clock); err != nil {
		t.Fatal(err)
	}
	f.pass()
	if got := f.sc.texts(); len(got) != 0 {
		t.Fatalf("scored during the running turn: %q", got)
	}
	if err := f.leases.MarkTurn(f.lease, false, f.clock.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	f.pass()
	if q, a := orderOf(f.sc.texts(), "Pick A or B?", "A"); q < 0 || a < 0 || a < q {
		t.Fatalf("order after the turn %q", f.sc.texts())
	}
}

// A reply with no answer item takes its place in the chain from when it was written, held
// or not, and not from later activity in its turn.
func TestFallbackReplyKeepsItsPlaceByItsWriteTime(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	start := now.Add(-time.Hour)
	turn := transcript.Turn{Prompt: "p", Source: transcript.Human, Started: start, Reply: "Pick A or B?", ReplyAt: start.Add(2 * time.Second), Ended: start.Add(100 * time.Second)}
	w := &Watcher{Now: func() time.Time { return now }}
	wk := &work{row: state.AskSession{Provider: "agy"}, session: &transcript.Session{}, turns: []transcript.Turn{turn}, mod: now.Add(-10 * time.Second)}
	// The prompt comes first; the reply is next.
	wk.row.Position = state.AskPosition{TurnStarted: start.UnixMilli(), ItemsDone: 1}
	if at, ok := w.upcoming(wk); !ok || !at.Equal(turn.ReplyAt) {
		t.Fatalf("held: %v %v", at, ok)
	}
	wk.mod = now.Add(-time.Hour)
	if at, ok := w.upcoming(wk); !ok || !at.Equal(turn.ReplyAt) {
		t.Fatalf("quiet: %v %v", at, ok)
	}
	// With no time for the reply, a held one holds the chain from its turn's start.
	turn.ReplyAt = time.Time{}
	wk.turns, wk.mod = []transcript.Turn{turn}, now.Add(-10*time.Second)
	if at, ok := w.upcoming(wk); !ok || !at.Equal(start) {
		t.Fatalf("held, time unknown: %v %v", at, ok)
	}
}

// successorReplies starts a second session of the same chain whose first prompt is reply.
func successorReplies(t *testing.T, f *fixture, reply string) {
	t.Helper()
	path2 := filepath.Join(f.dir, "s2.jsonl")
	id, err := f.leases.AddLease(state.Lease{Mode: state.ModeLong, Hostname: "h", Cwd: "/w", Workspace: "/w", Provider: "claude", SessionID: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.leases.SetLeaseTranscript(id, path2); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","isSidechain":false,"cwd":"/w","sessionId":"s2","timestamp":%q,"message":{"role":"user","content":%q}}`
	if err := os.WriteFile(path2, []byte(fmt.Sprintf(line, f.ts(), reply)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	at := f.clock.Add(-time.Hour)
	os.Chtimes(path2, at, at)
}

func orderOf(texts []string, q, a string) (int, int) {
	qi, ai := -1, -1
	for i, s := range texts {
		switch s {
		case q:
			qi = i
		case a:
			ai = i
		}
	}
	return qi, ai
}

// When a reset lands while an entry is being rejected, the watcher goes on from the reset,
// so nothing written before it is sent afterwards.
func TestRejectionDuringAResetTakesTheNewCutoff(t *testing.T) {
	f := newFixture(t)
	f.w.Start = f.clock.Add(-2 * time.Hour)
	f.user("p0")
	f.agent("Older question?")
	f.end()
	f.write(time.Hour)
	var after []string
	first := true
	f.sc.err = func(e Entry) error {
		if first {
			first = false
			f.reg.ResetAsks(func(string) bool { return true }, f.clock)
			return &ModelError{Class: ClassRejected, Status: 400}
		}
		after = append(after, e.Text)
		return nil
	}
	if res := f.pass(); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(after) != 0 {
		t.Fatalf("sent after the reset: %q", after)
	}
}

// A reset of one workspace during a pass stops the texts from before it, even while the
// pass is busy with another workspace.
func TestResetDuringAPassStopsTheOtherWorkspace(t *testing.T) {
	f := newFixture(t)
	f.w.Start = f.clock.Add(-2 * time.Hour)
	f.user("p0")
	f.agent("a0")
	f.end()
	f.write(time.Hour)
	path2 := filepath.Join(f.dir, "v.jsonl")
	id, err := f.leases.AddLease(state.Lease{Mode: state.ModeLong, Hostname: "h", Cwd: "/v", Workspace: "/v", Provider: "claude", SessionID: "v"})
	if err != nil {
		t.Fatal(err)
	}
	f.leases.SetLeaseTranscript(id, path2)
	var lines []string
	for i := 0; i < 3; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"user","isSidechain":false,"cwd":"/v","sessionId":"v","timestamp":%q,"message":{"role":"user","content":"v%d"}}`, f.ts(), i))
	}
	os.WriteFile(path2, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	at := f.clock.Add(-time.Hour)
	os.Chtimes(path2, at, at)
	reset := false
	f.sc.err = func(e Entry) error {
		if !reset && e.Workspace == "/w" {
			reset = true
			f.reg.ResetAsks(func(ws string) bool { return ws == "/v" }, f.clock)
		}
		return nil
	}
	f.pass()
	// /v texts sent before the first /w text predate the reset; none may follow it.
	sentAfter := 0
	resetSeen := false
	for _, e := range f.sc.seen {
		if e.Workspace == "/w" {
			resetSeen = true
			continue
		}
		if resetSeen {
			sentAfter++
		}
	}
	if sentAfter != 0 {
		t.Fatalf("sent %d texts of /v from before its reset: %q", sentAfter, f.sc.texts())
	}
}

// An answer a later answer replaced is not read.
func TestReplacedAnswerIsNotRead(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	list := entriesOf(transcript.Turn{Prompt: "p", Source: transcript.Human, Started: at, Items: []transcript.TurnItem{
		{Kind: transcript.ItemAnswer, Text: "Old question?", At: at},
		{Kind: transcript.ItemAnswer, Text: "New answer.", At: at, Supersedes: true},
	}})
	if len(list) != 3 || !list[1].skip || list[2].skip {
		t.Fatalf("%+v", list)
	}
}

// next gives what pending would start with: a run of skips taken as one, else the first entry.
func TestNextMatchesPending(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	w := &Watcher{Now: func() time.Time { return at.Add(time.Hour) }}
	wk := &work{row: state.AskSession{Position: state.AskPosition{TurnStarted: -1}}, mod: at, session: &transcript.Session{},
		turns: []transcript.Turn{
			{Prompt: "auto", Source: transcript.Aiq, Started: at, Ended: at, Reply: "r0"},
			{Prompt: "p1", Source: transcript.Human, Started: at.Add(time.Minute), Ended: at.Add(time.Minute), Reply: "r1"},
		}}
	list := w.pending(wk)
	want := list[0]
	for i := 1; want.skip && i < len(list) && list[i].skip; i++ {
		want = list[i]
	}
	got, ok := w.next(wk)
	if !ok || got.to != want.to || got.skip != want.skip {
		t.Fatalf("next %+v, want %+v", got, want)
	}
}

// Text written while off, in the same second as turning back on, stays unread.
func TestOnWithinASecondKeepsTheOffText(t *testing.T) {
	for _, offPass := range []bool{false, true} {
		f := newFixture(t)
		base := f.clock.Truncate(time.Second)
		f.w.Start = base.Add(-2 * time.Hour)
		f.user("before")
		f.agent("an answer before")
		f.end()
		f.write(time.Hour)
		f.pass()
		f.sc.seen = nil
		f.reg.SetAskWorkspaceOff("/w", true, base)
		line := func(at time.Duration, body string) string {
			return fmt.Sprintf(body, base.Add(at).Format(time.RFC3339Nano))
		}
		f.lines = append(f.lines,
			line(500*time.Millisecond, `{"type":"user","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"message":{"role":"user","content":"written while off"}}`),
			line(600*time.Millisecond, `{"type":"assistant","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"message":{"id":"off-1","model":"claude-opus-5","stop_reason":"end_turn","role":"assistant","content":[{"type":"text","text":"a question while off?"}]}}`),
			line(650*time.Millisecond, `{"type":"system","subtype":"turn_duration","isSidechain":false,"cwd":"/w","sessionId":"s","timestamp":%q,"durationMs":150}`))
		f.clock = base.Add(700 * time.Millisecond)
		f.write(0)
		if offPass {
			f.leases.MarkTurn(f.lease, true, f.clock)
			f.pass()
		}
		f.clock = base.Add(750 * time.Millisecond)
		f.reg.SetAskWorkspaceOff("/w", false, f.clock)
		f.leases.MarkTurn(f.lease, false, f.clock)
		f.clock = base.Add(2 * time.Minute)
		f.pass()
		if len(f.sc.seen) != 0 {
			t.Fatalf("off pass %v: read %q", offPass, f.sc.texts())
		}
	}
}

// An answer written after the cutoff, in a turn that started before it, is read: its own
// time counts, not the turn's start.
func TestAnswerAfterTheCutoffInAnEarlierTurnIsRead(t *testing.T) {
	f := newFixture(t)
	f.user("p0")
	f.w.Start = f.clock.Add(-time.Hour).Add(2 * time.Second)
	f.agent("Pick A or B?")
	f.write(time.Hour)
	f.pass()
	if _, q := orderOf(f.sc.texts(), "p0", "Pick A or B?"); q < 0 {
		t.Fatalf("read %q", f.sc.texts())
	}
}

// A session first seen after its workspace was turned back on starts from that moment, so
// what it wrote while off stays unread.
func TestSessionSeenAfterOnKeepsTheOffText(t *testing.T) {
	f := newFixture(t)
	f.w.Start = f.clock.Add(-3 * time.Hour)
	f.user("p0")
	f.agent("an answer before")
	f.end()
	f.write(time.Hour)
	f.pass()
	f.sc.seen = nil
	off := f.clock.Add(-time.Hour)
	f.reg.SetAskWorkspaceOff("/w", true, off)
	successorReplies(t, f, "written while off") // dated within the hour after off
	f.reg.SetAskWorkspaceOff("/w", false, f.clock.Add(-time.Minute))
	f.pass()
	if len(f.sc.seen) != 0 {
		t.Fatalf("read %q", f.sc.texts())
	}
}

// A long backlog prepares each text a fixed number of times, even with the status row
// written after every text.
func TestBacklogIsPreparedOnce(t *testing.T) {
	f := newFixture(t)
	f.w.Start = f.clock.Add(-2 * time.Hour)
	const n = 80
	for i := 0; i < n; i++ {
		f.user(fmt.Sprintf("p%d", i))
		f.agent(fmt.Sprintf("answer %d", i))
		f.end()
	}
	f.write(time.Hour)
	f.sc.err = func(Entry) error {
		f.clock = f.clock.Add(reportEvery)
		return nil
	}
	f.pass()
	if len(f.sc.seen) != 2*n {
		t.Fatalf("scored %d", len(f.sc.seen))
	}
	if f.w.prepared > 3*2*n {
		t.Fatalf("prepared %d texts for %d", f.w.prepared, 2*n)
	}
}

// A turn whose prompt was a record too long to read has no reply from the
// person to score: its empty prompt is skipped.
func TestSkippedPromptIsNotRead(t *testing.T) {
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	list := entriesOf(transcript.Turn{Source: transcript.Human, PromptSkipped: true, Skipped: 1, Started: at, Items: []transcript.TurnItem{
		{Kind: transcript.ItemAnswer, Text: "Done.", At: at},
	}})
	if len(list) != 2 || !list[0].skip || list[1].skip || list[1].text != "Done." {
		t.Fatalf("%+v", list)
	}
	for _, e := range list {
		if e.kind == EntryReply && e.text == "" && !e.skip {
			t.Errorf("an empty reply from the person is scored: %+v", e)
		}
	}
}
