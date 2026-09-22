package selector

import (
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

var now = time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)

func win(kind string, used float64, resetIn time.Duration) state.Window {
	return state.Window{Kind: kind, UsedPct: used, ResetsAt: now.Add(resetIn).Unix(), ObservedAt: now.Unix(), Label: kind}
}

func cand(id string, ws ...state.Window) Candidate {
	return Candidate{ID: id, Enabled: true, HasCredential: true, Windows: ws}
}

func policy(mode string) Policy {
	return Policy{Now: now, Mode: mode, SwitchPct: 95, StaleAfter: 15 * time.Minute, WeeklyReservePct: 5, MaxWorkers: 3, MinHours: 0.25}
}

// The user's real numbers on the day this was written: the account whose
// session window resets in six minutes must win, even though another
// account has far more weekly quota left.
func TestRolloverPrefersSoonExpiringWindow(t *testing.T) {
	cands := []Candidate{
		cand("claude/alpha", win(state.KindShort, 100, 66*time.Minute), win(state.KindWeekly, 54, 57*time.Hour)),
		cand("claude/bravo", win(state.KindShort, 43, 6*time.Minute), win(state.KindWeekly, 8, 163*time.Hour)),
		cand("claude/gamma", win(state.KindShort, 2, 4*time.Hour), win(state.KindWeekly, 0, 141*time.Hour)),
	}
	res, err := Select(policy(state.ModeWorker), cands)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "claude/bravo" {
		t.Fatalf("want claude/bravo, got %s\n%v", res.ID, res.Ranked)
	}
	r := res.Ranked
	if r[0].ID != "claude/bravo" || r[1].ID != "claude/gamma" || r[2].ID != "claude/alpha" {
		t.Fatalf("ranking: %+v", r)
	}
	if r[2].Eligible || !strings.Contains(r[2].Reason, "exhausted") {
		t.Fatalf("alpha should be exhausted: %+v", r[2])
	}
	// 57 points expiring in 6 minutes, floored at 15 minutes → 228/h.
	if r[0].Score < 200 || r[0].Score > 240 {
		t.Fatalf("bravo score %.1f", r[0].Score)
	}
}

// An account whose weekly window rolls over in ten hours with 40% left must
// get the work ahead of a fresh account, because weekly points are worth
// several session points and vanish at the reset.
func TestWeeklyWeightPrefersApproachingRollover(t *testing.T) {
	a := cand("a", win(state.KindShort, 40, 3*time.Hour), win(state.KindWeekly, 60, 10*time.Hour))
	b := cand("b", win(state.KindShort, 0, 5*time.Hour), win(state.KindWeekly, 10, 6*24*time.Hour))
	p := policy(state.ModeWorker)
	if res, _ := Select(p, []Candidate{a, b}); res.ID != "b" {
		t.Fatalf("unweighted, b wins on its fresh session window; got %s", res.ID)
	}
	p.WeeklyWeight = 5
	res, _ := Select(p, []Candidate{a, b})
	if res.ID != "a" {
		t.Fatalf("weighted, a must win: %+v", res.Ranked)
	}
	// One hour before the rollover a wins either way.
	a.Windows[1].ResetsAt = now.Add(time.Hour).Unix()
	p.WeeklyWeight = 0
	if res, _ := Select(p, []Candidate{a, b}); res.ID != "a" {
		t.Fatalf("one hour out a must win unweighted too: %+v", res.Ranked)
	}
}

// A near-empty weekly caps how much of a fresh session window counts.
func TestWeeklyCapsSessionRemaining(t *testing.T) {
	c := cand("x", win(state.KindShort, 0, time.Hour), win(state.KindWeekly, 97, 100*time.Hour))
	score, terms := Score(c, policy(state.ModeInteractive))
	if score > 4 {
		t.Fatalf("score %.2f should be capped by the 3%% weekly: %v", score, terms)
	}
}

func TestWorkerReserveAndCap(t *testing.T) {
	p := policy(state.ModeWorker)
	low := cand("low", win(state.KindShort, 0, time.Hour), win(state.KindWeekly, 96, 100*time.Hour))
	busy := cand("busy", win(state.KindShort, 10, time.Hour), win(state.KindWeekly, 10, 100*time.Hour))
	busy.WorkerLeases = 3
	ok := cand("ok", win(state.KindShort, 50, time.Hour), win(state.KindWeekly, 50, 100*time.Hour))
	res, err := Select(p, []Candidate{low, busy, ok})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "ok" {
		t.Fatalf("got %s: %+v", res.ID, res.Ranked)
	}
	// Interactive mode ignores the reserve and the cap.
	res, err = Select(policy(state.ModeInteractive), []Candidate{low, busy})
	if err != nil || res.ID != "busy" {
		t.Fatalf("interactive: %v %s", err, res.ID)
	}
}

func TestWorkersAvoidInteractiveAccount(t *testing.T) {
	fg := cand("fg", win(state.KindShort, 10, time.Hour), win(state.KindWeekly, 10, 100*time.Hour))
	fg.InteractiveLeases = 1
	bg := cand("bg", win(state.KindShort, 60, time.Hour), win(state.KindWeekly, 60, 100*time.Hour))
	res, err := Select(policy(state.ModeWorker), []Candidate{fg, bg})
	if err != nil || res.ID != "bg" {
		t.Fatalf("worker should avoid fg: %v %s", err, res.ID)
	}
	// With no alternative the interactive account is still used.
	bg.CooldownUntil = now.Add(time.Hour).Unix()
	res, err = Select(policy(state.ModeWorker), []Candidate{fg, bg})
	if err != nil || res.ID != "fg" {
		t.Fatalf("fallback: %v %s", err, res.ID)
	}
}

func TestStickyInteractive(t *testing.T) {
	p := policy(state.ModeInteractive)
	p.Sticky = true
	p.AffinityID = "a"
	a := cand("a", win(state.KindShort, 80, 2*time.Hour), win(state.KindWeekly, 50, 100*time.Hour))
	b := cand("b", win(state.KindShort, 0, 30*time.Minute), win(state.KindWeekly, 0, 100*time.Hour))
	res, _ := Select(p, []Candidate{a, b})
	if res.ID != "a" {
		t.Fatalf("sticky should keep a, got %s", res.ID)
	}
	a.Windows[0].UsedPct = 96
	res, _ = Select(p, []Candidate{a, b})
	if res.ID != "b" {
		t.Fatalf("above switch threshold should leave a, got %s", res.ID)
	}
	p.SkipID = "a"
	a.Windows[0].UsedPct = 10
	res, _ = Select(p, []Candidate{a, b})
	if res.ID != "b" {
		t.Fatalf("--next should skip a, got %s", res.ID)
	}
}

func TestScopedWindowBindsOnlyForConfiguredModel(t *testing.T) {
	fable := state.Window{Kind: state.KindWeekly, Scope: "Fable", UsedPct: 100, ResetsAt: now.Add(50 * time.Hour).Unix(), ObservedAt: now.Unix(), Label: "Fable"}
	c := cand("x", win(state.KindShort, 10, time.Hour), win(state.KindWeekly, 10, 100*time.Hour), fable)
	p := policy(state.ModeWorker)
	if r := Rank(p, []Candidate{c}); !r[0].Eligible {
		t.Fatalf("without a model scope the Fable cap must not bind: %+v", r[0])
	}
	p.ModelScope = "fable"
	if r := Rank(p, []Candidate{c}); r[0].Eligible {
		t.Fatalf("with model scope fable the account is exhausted: %+v", r[0])
	}
}

// The overall weekly and a model-scoped weekly bound the same week: only
// the tighter one may contribute.
func TestOnlyTightestWeeklyCounts(t *testing.T) {
	fable := state.Window{Kind: state.KindWeekly, Scope: "Fable", Label: "Fable", UsedPct: 70, ResetsAt: now.Add(10 * time.Hour).Unix(), ObservedAt: now.Unix()}
	c := cand("x", win(state.KindWeekly, 50, 10*time.Hour), fable)
	p := policy(state.ModeWorker)
	p.ModelScope = "fable"
	p.WeeklyWeight = 5
	score, terms := Score(c, p)
	// Fable has 30% left: 30/10h × 5 = 15. The overall weekly must not add its 50/10 × 5.
	if score < 14.9 || score > 15.1 {
		t.Fatalf("score %.2f terms %v", score, terms)
	}
}

func TestStaleTelemetryFallsBackToNeutral(t *testing.T) {
	old := win(state.KindShort, 0, time.Hour)
	old.ObservedAt = now.Add(-time.Hour).Unix()
	c := cand("x", old)
	score, terms := Score(c, policy(state.ModeWorker))
	if score != neutralScore || len(terms) != 1 {
		t.Fatalf("stale: %.1f %v", score, terms)
	}
}

func TestRolledOverWindowCountsAsFull(t *testing.T) {
	// Observed at 100% but the reset time has passed: the window is fresh.
	w := win(state.KindShort, 100, -10*time.Minute)
	c := cand("x", w, win(state.KindWeekly, 10, 100*time.Hour))
	r := Rank(policy(state.ModeWorker), []Candidate{c})
	if !r[0].Eligible {
		t.Fatalf("rolled-over window must not block: %+v", r[0])
	}
}

func TestForceAndNoEligible(t *testing.T) {
	p := policy(state.ModeWorker)
	p.ForceID = "x"
	x := cand("x", win(state.KindShort, 100, time.Hour))
	res, err := Select(p, []Candidate{x})
	if err != nil || res.ID != "x" {
		t.Fatalf("force: %v %s", err, res.ID)
	}
	p.ForceID = ""
	if _, err := Select(p, []Candidate{x}); err == nil {
		t.Fatal("expected no eligible account")
	}
}

// Reset credits expire: an account holding two that lapse in two days must
// win over accounts whose only edge is a nearer weekly rollover, and a worker
// may drain it past the weekly reserve, since the credit refills the week.
func TestResetCreditsFavourAndWaiveReserve(t *testing.T) {
	p := policy(state.ModeWorker)
	p.WeeklyWeight = 5
	usky := cand("codex/usky", win(state.KindWeekly, 20, 6*24*time.Hour))
	usky.ResetCredits, usky.ResetCreditExpiry = 2, now.Add(48*time.Hour).Unix()
	other := cand("codex/netflix", win(state.KindWeekly, 40, 4*24*time.Hour))
	res, err := Select(p, []Candidate{other, usky})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "codex/usky" {
		t.Fatalf("want codex/usky, got %s\n%+v", res.ID, res.Ranked)
	}
	if !strings.Contains(strings.Join(res.Ranked[0].Terms, " "), "reset credit") {
		t.Fatalf("explain should name the credit term: %v", res.Ranked[0].Terms)
	}

	low := cand("codex/usky", win(state.KindWeekly, 98, 6*24*time.Hour))
	low.ResetCredits = 1
	if r := Rank(p, []Candidate{low}); !r[0].Eligible {
		t.Fatalf("credit holder below reserve should stay eligible for workers: %+v", r[0])
	}
	low.ResetCredits = 0
	if r := Rank(p, []Candidate{low}); r[0].Eligible {
		t.Fatalf("without a credit the reserve applies: %+v", r[0])
	}
}

// An expired credit adds nothing.
func TestExpiredResetCreditIgnored(t *testing.T) {
	p := policy(state.ModeWorker)
	c := cand("codex/a", win(state.KindWeekly, 50, 3*24*time.Hour))
	base, _ := Score(c, p)
	c.ResetCredits, c.ResetCreditExpiry = 1, now.Add(-time.Hour).Unix()
	if got, _ := Score(c, p); got != base {
		t.Fatalf("expired credit changed the score: %.2f → %.2f", base, got)
	}
}

// A credit holder with an interactive session still takes workers: the
// credit refills what they spend.
func TestResetCreditHolderTakesWorkersDespiteInteractive(t *testing.T) {
	p := policy(state.ModeWorker)
	p.WeeklyWeight = 5
	usky := cand("codex/usky", win(state.KindWeekly, 20, 7*24*time.Hour))
	usky.ResetCredits, usky.InteractiveLeases = 2, 1
	bjola := cand("codex/bjola", win(state.KindWeekly, 74, 4*24*time.Hour))
	res, err := Select(p, []Candidate{bjola, usky})
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "codex/usky" {
		t.Fatalf("want codex/usky, got %s\n%+v", res.ID, res.Ranked)
	}
	usky.ResetCredits = 0
	if res, _ := Select(p, []Candidate{bjola, usky}); res.ID != "codex/bjola" {
		t.Fatalf("without a credit the interactive guard applies, got %s", res.ID)
	}
}

// Once a credit holder reaches the normal switch boundary, finish its current
// allowance before sending work to a freshly rolled-over account. This makes
// the credit redeemable instead of stranding it behind the last few percent.
func TestResetCreditReadyDrainsBeforeFreshAccount(t *testing.T) {
	p := policy(state.ModeInteractive)
	p.WeeklyWeight = 5
	nearlySpent := cand("codex/netflix", win(state.KindWeekly, 98, 92*time.Hour))
	nearlySpent.ResetCredits = 1
	nearlySpent.ResetCreditExpiry = now.Add(718 * time.Hour).Unix()
	fresh := cand("codex/charlotte", win(state.KindWeekly, 2, 7*24*time.Hour))

	res, err := Select(p, []Candidate{fresh, nearlySpent})
	if err != nil || res.ID != nearlySpent.ID {
		t.Fatalf("credit holder should drain first: err=%v result=%+v", err, res)
	}
	if !res.Ranked[0].ResetCreditReady || !strings.Contains(strings.Join(res.Ranked[0].Terms, " "), "drain first") {
		t.Fatalf("ranking should explain reset-credit priority: %+v", res.Ranked[0])
	}

	// Workspace affinity must not strand that credit on another account.
	p.Sticky = true
	p.AffinityID = fresh.ID
	res, err = Select(p, []Candidate{fresh, nearlySpent})
	if err != nil || res.ID != nearlySpent.ID {
		t.Fatalf("credit drain should override fresh affinity: err=%v result=%+v", err, res)
	}

	// Once already on the near-empty account, stick there until it blocks and
	// the automatic reset-credit redemption runs.
	p.AffinityID = nearlySpent.ID
	res, err = Select(p, []Candidate{fresh, nearlySpent})
	if err != nil || res.ID != nearlySpent.ID {
		t.Fatalf("credit-draining affinity should stay put: err=%v result=%+v", err, res)
	}

	// After that credit has redeemed, a second nearly spent holder becomes
	// the next drain target instead of continuing on the freshly reset one.
	nearlySpent.Windows[0].UsedPct = 0
	nearlySpent.ResetCredits = 0
	second := cand("codex/bjola", win(state.KindWeekly, 98, 95*time.Hour))
	second.ResetCredits = 1
	p.AffinityID = nearlySpent.ID
	res, err = Select(p, []Candidate{fresh, nearlySpent, second})
	if err != nil || res.ID != second.ID {
		t.Fatalf("second credit holder should drain next: err=%v result=%+v", err, res)
	}
}

func TestKnownAuthenticationFailureIsIneligible(t *testing.T) {
	p := policy(state.ModeInteractive)
	bad := cand("codex/usky", win(state.KindWeekly, 0, 7*24*time.Hour))
	bad.UnavailableReason = "authentication required"
	good := cand("codex/charlotte", win(state.KindWeekly, 98, 7*24*time.Hour))

	res, err := Select(p, []Candidate{bad, good})
	if err != nil || res.ID != good.ID {
		t.Fatalf("known-bad auth must be skipped: err=%v result=%+v", err, res)
	}
	if res.Ranked[1].Eligible || res.Ranked[1].Reason != "authentication required" {
		t.Fatalf("auth failure reason lost: %+v", res.Ranked[1])
	}

	p.ForceID = bad.ID
	if _, err := Select(p, []Candidate{bad, good}); err == nil || !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("forcing known-bad auth should fail clearly, got %v", err)
	}
}
