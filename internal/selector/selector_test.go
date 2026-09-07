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
