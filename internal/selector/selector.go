// Package selector ranks accounts by how much quota they are about to lose.
//
// For every binding window w of an account:
//
//	remaining_w = 100 − used_w
//	hours_w     = time until the window resets (floored at MinHours)
//	perish_w    = min(remaining_w, remaining_weekly) / hours_w
//
// perish_w is the number of percentage points that vanish per hour if the
// account sits idle. Weekly windows are multiplied by WeeklyWeight, since one
// weekly point is several session points' worth of tokens. The score is
// Σ perish_w over binding windows, divided by (1 + worker leases). Highest
// score wins. Exhausted accounts, accounts below
// the weekly reserve (workers only), disabled accounts and accounts at their
// worker cap are filtered out first. Interactive sessions may be sticky: they
// keep their workspace's account until it crosses the switch threshold.
package selector

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

// Candidate is one account with everything the policy needs to rank it.
type Candidate struct {
	ID             string
	Enabled        bool
	HasCredential  bool
	Priority       int
	LastSelectedAt int64

	Windows       []state.Window
	CooldownUntil int64 // unix seconds, 0 = none
	ResetCredits  int

	InteractiveLeases int
	WorkerLeases      int
}

type Policy struct {
	Now  time.Time
	Mode string // state.ModeInteractive or state.ModeWorker

	// ModelScope names a scoped window (e.g. "Fable") that counts as binding.
	ModelScope string

	AffinityID string // sticky account for this workspace ("" = none)
	SkipID     string // excluded outright (--next)
	ForceID    string // short-circuits selection (--account)
	Sticky     bool   // honour AffinityID (interactive policy "sticky")

	SwitchPct        float64
	StaleAfter       time.Duration
	WeeklyReservePct float64
	MaxWorkers       int
	MinHours         float64
	WeeklyWeight     float64 // multiplier on weekly terms (0 = 1)
}

// Ranked is one candidate with its score and the reason it ranked there.
type Ranked struct {
	ID       string
	Score    float64
	Eligible bool
	Reason   string   // why it is ineligible, or "" when eligible
	Terms    []string // per-window contributions, for --explain
}

type Result struct {
	ID     string
	Notes  []string
	Ranked []Ranked
}

const neutralScore = 5 // score for an account with no usable telemetry

// binding reports whether a window counts toward the account's capacity.
func binding(w state.Window, modelScope string) bool {
	if w.Kind != state.KindShort && w.Kind != state.KindWeekly {
		return false
	}
	if w.Scope == "" {
		return true
	}
	if modelScope == "" {
		return false
	}
	return strings.Contains(strings.ToLower(w.Scope), strings.ToLower(modelScope))
}

// exhaustedWindow returns the window that blocks the account right now.
func exhaustedWindow(ws []state.Window, modelScope string, now time.Time) (state.Window, bool) {
	for _, w := range ws {
		if !binding(w, modelScope) {
			continue
		}
		if w.ResetsAt > 0 && w.ResetsAt <= now.Unix() {
			continue // the window has rolled over since we looked
		}
		if w.UsedPct >= 100 || w.Severity == "critical" {
			return w, true
		}
	}
	return state.Window{}, false
}

func weeklyRemaining(ws []state.Window, modelScope string) float64 {
	rem := 100.0
	found := false
	for _, w := range ws {
		if w.Kind != state.KindWeekly || !binding(w, modelScope) || w.UsedPct < 0 {
			continue
		}
		found = true
		if r := 100 - w.UsedPct; r < rem {
			rem = r
		}
	}
	if !found {
		return -1
	}
	return rem
}

// Score computes the perishability score and its explanation terms.
func Score(c Candidate, p Policy) (float64, []string) {
	minHours := p.MinHours
	if minHours <= 0 {
		minHours = 0.25
	}
	weekly := weeklyRemaining(c.Windows, p.ModelScope)
	// Several weekly windows (overall plus model-scoped caps) bound the same
	// week; only the tightest one counts, or the week would be added twice.
	tightestWeekly := -1
	for i, w := range c.Windows {
		if w.Kind == state.KindWeekly && binding(w, p.ModelScope) && w.UsedPct >= 0 && 100-w.UsedPct <= weekly+1e-9 {
			tightestWeekly = i
			break
		}
	}
	var terms []string
	total := 0.0
	fresh := false
	for i, w := range c.Windows {
		if !binding(w, p.ModelScope) || w.UsedPct < 0 {
			continue
		}
		if w.Kind == state.KindWeekly && i != tightestWeekly {
			name := w.Label
			if name == "" {
				name = w.Key
			}
			terms = append(terms, fmt.Sprintf("%s %.0f%% left (same week, not tightest)", name, 100-w.UsedPct))
			continue
		}
		if p.StaleAfter > 0 && w.ObservedAt > 0 && p.Now.Sub(time.Unix(w.ObservedAt, 0)) > p.StaleAfter {
			continue
		}
		fresh = true
		remaining := 100 - w.UsedPct
		rolled := w.ResetsAt > 0 && w.ResetsAt <= p.Now.Unix()
		if rolled {
			remaining = 100 // rolled over since the observation
		}
		capped := remaining
		if weekly >= 0 && weekly < capped {
			capped = weekly
		}
		hours := minHours
		if w.ResetsAt > 0 && !rolled {
			hours = math.Max(float64(w.ResetsAt-p.Now.Unix())/3600, minHours)
		} else if w.WindowSeconds > 0 {
			hours = math.Max(float64(w.WindowSeconds)/3600/2, minHours)
		} else if w.Kind == state.KindWeekly {
			hours = 84
		} else {
			hours = 2.5
		}
		perish := capped / hours
		weight := 1.0
		if w.Kind == state.KindWeekly && p.WeeklyWeight > 0 {
			weight = p.WeeklyWeight
		}
		perish *= weight
		total += perish
		name := w.Label
		if name == "" {
			name = w.Key
		}
		if weight != 1 {
			terms = append(terms, fmt.Sprintf("%s %.0f%% left / %.1fh × %g = %.1f", name, capped, hours, weight, perish))
		} else {
			terms = append(terms, fmt.Sprintf("%s %.0f%% left / %.1fh = %.1f", name, capped, hours, perish))
		}
	}
	if !fresh {
		total = neutralScore
		terms = []string{"no fresh telemetry: neutral prior"}
	}
	if c.WorkerLeases > 0 {
		total /= float64(1 + c.WorkerLeases)
		terms = append(terms, fmt.Sprintf("÷ (1+%d workers)", c.WorkerLeases))
	}
	return total, terms
}

// Rank evaluates every candidate and sorts them best first.
func Rank(p Policy, cands []Candidate) []Ranked {
	var out []Ranked
	for _, c := range cands {
		r := Ranked{ID: c.ID, Eligible: true}
		switch {
		case c.ID == p.SkipID && p.SkipID != "":
			r.Eligible, r.Reason = false, "skipped (--next)"
		case !c.Enabled:
			r.Eligible, r.Reason = false, "disabled"
		case !c.HasCredential:
			r.Eligible, r.Reason = false, "no credential"
		case c.CooldownUntil > p.Now.Unix():
			r.Eligible, r.Reason = false, "exhausted until "+fmtReset(c.CooldownUntil, p.Now)
		}
		if r.Eligible {
			if w, ok := exhaustedWindow(c.Windows, p.ModelScope, p.Now); ok {
				name := w.Label
				if name == "" {
					name = w.Key
				}
				when := ""
				if w.ResetsAt > 0 {
					when = " until " + fmtReset(w.ResetsAt, p.Now)
				}
				r.Eligible, r.Reason = false, fmt.Sprintf("%s window exhausted%s", strings.ToLower(name), when)
			}
		}
		if r.Eligible && p.Mode == state.ModeWorker {
			if p.MaxWorkers > 0 && c.WorkerLeases >= p.MaxWorkers {
				r.Eligible, r.Reason = false, fmt.Sprintf("at worker cap (%d)", p.MaxWorkers)
			} else if rem := weeklyRemaining(c.Windows, p.ModelScope); rem >= 0 && rem <= p.WeeklyReservePct {
				r.Eligible, r.Reason = false, fmt.Sprintf("weekly %.0f%% left ≤ reserve %.0f%%", rem, p.WeeklyReservePct)
			}
		}
		r.Score, r.Terms = Score(c, p)
		out = append(out, r)
	}
	byID := map[string]Candidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Eligible != b.Eligible {
			return a.Eligible
		}
		// Workers stay off accounts that carry an interactive session when
		// any alternative exists.
		if p.Mode == state.ModeWorker {
			fa, fb := byID[a.ID].InteractiveLeases > 0, byID[b.ID].InteractiveLeases > 0
			if fa != fb {
				return !fa
			}
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		ca, cb := byID[a.ID], byID[b.ID]
		if ca.Priority != cb.Priority {
			return ca.Priority < cb.Priority
		}
		if ca.LastSelectedAt != cb.LastSelectedAt {
			return ca.LastSelectedAt < cb.LastSelectedAt
		}
		return a.ID < b.ID
	})
	return out
}

// Select picks an account or returns an error naming why none is available.
func Select(p Policy, cands []Candidate) (Result, error) {
	var res Result
	byID := map[string]Candidate{}
	for _, c := range cands {
		byID[c.ID] = c
	}

	if p.ForceID != "" {
		c, ok := byID[p.ForceID]
		if !ok {
			return res, fmt.Errorf("account %s not found", p.ForceID)
		}
		if !c.Enabled {
			return res, fmt.Errorf("account %s is disabled", p.ForceID)
		}
		if !c.HasCredential {
			return res, fmt.Errorf("account %s has no credential", p.ForceID)
		}
		if c.CooldownUntil > p.Now.Unix() {
			res.Notes = append(res.Notes, fmt.Sprintf("Warning: %s is exhausted until %s (forced anyway)",
				c.ID, fmtReset(c.CooldownUntil, p.Now)))
		}
		res.ID = c.ID
		return res, nil
	}

	res.Ranked = Rank(p, cands)
	eligible := map[string]Ranked{}
	for _, r := range res.Ranked {
		if r.Eligible {
			eligible[r.ID] = r
		} else if r.Reason != "disabled" && r.Reason != "skipped (--next)" {
			res.Notes = append(res.Notes, fmt.Sprintf("Skipping %s — %s", r.ID, r.Reason))
		}
	}
	if len(eligible) == 0 {
		return res, fmt.Errorf("no eligible account")
	}

	if p.Sticky && p.AffinityID != "" && p.AffinityID != p.SkipID {
		if _, ok := eligible[p.AffinityID]; ok {
			c := byID[p.AffinityID]
			healthy := true
			for _, w := range c.Windows {
				if binding(w, p.ModelScope) && w.UsedPct >= p.SwitchPct && p.SwitchPct > 0 {
					if w.ResetsAt > 0 && w.ResetsAt <= p.Now.Unix() {
						continue
					}
					healthy = false
				}
			}
			if healthy {
				res.ID = p.AffinityID
				return res, nil
			}
			res.Notes = append(res.Notes, fmt.Sprintf("Leaving %s — usage above %.0f%%", p.AffinityID, p.SwitchPct))
		}
	}

	for _, r := range res.Ranked {
		if r.Eligible {
			res.ID = r.ID
			return res, nil
		}
	}
	return res, fmt.Errorf("no eligible account")
}

func fmtReset(unix int64, now time.Time) string {
	t := time.Unix(unix, 0).Local()
	if t.Sub(now) > 24*time.Hour {
		return t.Format("Mon 15:04")
	}
	return t.Format("15:04")
}
