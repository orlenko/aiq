// Package pool ties config, state and the selector together: it builds
// candidates from the store, applies the configured policy, and assembles the
// view that `aiq status` and the daemon UI render.
package pool

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

var Providers = []string{"claude", "codex"}

type Pool struct {
	Cfg *config.Config
	St  *state.Store
	// CodexCommand builds a child process for the real codex binary (set by
	// the command layer, which owns binary resolution).
	CodexCommand func(args, env []string) *exec.Cmd
}

// ModelScope resolves the scoped-limit name for a provider.
func (p *Pool) ModelScope(provider string) string {
	var cfg config.Provider
	switch provider {
	case "claude":
		cfg = p.Cfg.Providers.Claude
	case "codex":
		cfg = p.Cfg.Providers.Codex
	}
	if cfg.ModelScope != "auto" {
		return cfg.ModelScope
	}
	switch provider {
	case "claude":
		return claude.DefaultModel()
	case "codex":
		return codex.DefaultModel()
	}
	return ""
}

// HasCredential reports whether an account can launch.
func HasCredential(a state.Account) bool {
	switch a.Provider {
	case "claude":
		return claude.HasCredential(a.Home, a.Native)
	case "codex":
		if a.Native {
			return true
		}
		return codex.HasCredential(a.Home)
	}
	return false
}

var machineID string

// Hostname is this machine's identity as recorded in leases. It prefers a
// stable machine id over os.Hostname, which on macOS changes with the network.
func Hostname() string {
	if machineID != "" {
		return machineID
	}
	if data, err := os.ReadFile("/etc/machine-id"); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		machineID = strings.TrimSpace(string(data))
		return machineID
	}
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		if m := platformUUIDRe.FindSubmatch(out); m != nil {
			machineID = string(m[1])
			return machineID
		}
	}
	machineID, _ = os.Hostname()
	return machineID
}

var platformUUIDRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

// Candidates assembles selector input for a provider. Dead leases on this
// host are pruned as a side effect.
func (p *Pool) Candidates(provider string) ([]selector.Candidate, error) {
	accounts, err := p.St.ListAccounts(provider)
	if err != nil {
		return nil, err
	}
	leases, err := p.St.PruneLeases(Hostname(), proc.Alive)
	if err != nil {
		return nil, err
	}
	interactive := map[string]int{}
	workers := map[string]int{}
	for _, l := range leases {
		if l.Mode == state.ModeWorker {
			workers[l.AccountID]++
		} else {
			interactive[l.AccountID]++
		}
	}
	var cands []selector.Candidate
	for _, acc := range accounts {
		c := selector.Candidate{
			ID:                acc.ID,
			Enabled:           acc.Enabled,
			HasCredential:     HasCredential(acc),
			Priority:          acc.Priority,
			LastSelectedAt:    acc.LastSelectedAt,
			InteractiveLeases: interactive[acc.ID],
			WorkerLeases:      workers[acc.ID],
		}
		if u, ok, err := p.St.GetUsage(acc.ID); err != nil {
			return nil, err
		} else if ok {
			c.CooldownUntil = u.CooldownUntil
			c.ResetCredits = u.ResetCredits
		}
		ws, err := p.St.ListWindows(acc.ID)
		if err != nil {
			return nil, err
		}
		c.Windows = ws
		cands = append(cands, c)
	}
	return cands, nil
}

// Policy builds the selector policy for a provider and mode.
func (p *Pool) Policy(provider, mode string, now time.Time) selector.Policy {
	sel := p.Cfg.Selection
	return selector.Policy{
		Now:              now,
		Mode:             mode,
		ModelScope:       p.ModelScope(provider),
		Sticky:           mode == state.ModeInteractive && sel.InteractivePolicy != "score",
		SwitchPct:        sel.SwitchPct,
		StaleAfter:       time.Duration(sel.StaleAfterSeconds) * time.Second,
		WeeklyReservePct: sel.WeeklyReservePct,
		MaxWorkers:       sel.MaxWorkersPerAccount,
		MinHours:         sel.MinHours,
		WeeklyWeight:     sel.WeeklyWeight,
	}
}

// Rank returns the ranking a launch would see right now.
func (p *Pool) Rank(provider, mode string) ([]selector.Ranked, error) {
	cands, err := p.Candidates(provider)
	if err != nil {
		return nil, err
	}
	return selector.Rank(p.Policy(provider, mode, time.Now()), cands), nil
}

// --- telemetry refresh ---

// Refresh polls every account with aiq's own pollers (see poll.go). It
// returns per-account results; a failed account keeps its last windows.
func (p *Pool) Refresh(ids ...string) []PollResult {
	timeout := time.Duration(p.Cfg.Poll.TimeoutSeconds) * time.Second
	return p.Poll(p.CodexCommand, timeout, ids...)
}

// clearStaleCooldowns lifts a manual/observed exhaustion once fresh
// telemetry shows every binding window below 100%.
func (p *Pool) clearStaleCooldowns(accounts []state.Account) {
	now := time.Now()
	for _, a := range accounts {
		u, ok, _ := p.St.GetUsage(a.ID)
		if !ok || !u.Exhausted {
			continue
		}
		if u.CooldownUntil > 0 && u.CooldownUntil <= now.Unix() {
			p.St.MarkReady(a.ID, now)
			continue
		}
		ws, _ := p.St.ListWindows(a.ID)
		fresh := false
		blocked := false
		for _, w := range ws {
			if u.ExhaustedAt == 0 || w.ObservedAt <= u.ExhaustedAt {
				continue
			}
			fresh = true
			if w.UsedPct >= 100 || w.Severity == "critical" {
				blocked = true
			}
		}
		if fresh && !blocked {
			p.St.MarkReady(a.ID, now)
			p.St.LogEvent(a.Provider, a.ID, "ready", "fresh telemetry shows capacity", now)
		}
	}
}

// --- view ---

type WindowView struct {
	Key           string  `json:"key"`
	Label         string  `json:"label"`
	Kind          string  `json:"kind"`
	Scope         string  `json:"scope,omitempty"`
	Binding       bool    `json:"binding"`
	UsedPct       float64 `json:"used_pct"`
	ResetsAt      int64   `json:"resets_at"`
	WindowSeconds int64   `json:"window_seconds"`
	Severity      string  `json:"severity,omitempty"`
	Source        string  `json:"source"`
	ObservedAt    int64   `json:"observed_at"`
}

type LeaseView struct {
	ID            int64  `json:"id"`
	AccountID     string `json:"account"`
	PID           int    `json:"pid"`
	Hostname      string `json:"host"`
	Mode          string `json:"mode"`
	Cwd           string `json:"cwd"`
	Depth         int    `json:"depth"`
	ParentAccount string `json:"parent,omitempty"`
	Args          string `json:"args"`
	StartedAt     int64  `json:"started_at"`
	Drain         string `json:"drain,omitempty"`
	Pane          string `json:"pane,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
}

type AccountView struct {
	ID            string       `json:"id"`
	Provider      string       `json:"provider"`
	Name          string       `json:"name"`
	Enabled       bool         `json:"enabled"`
	Native        bool         `json:"native"`
	Home          string       `json:"home"`
	QuotaID       string       `json:"quota_id,omitempty"`
	Identity      string       `json:"identity,omitempty"`
	Label         string       `json:"label,omitempty"` // aiquota's label for the account
	Order         int          `json:"order"`           // display position (aiquota grid order)
	Plan          string       `json:"plan,omitempty"`
	HasCredential bool         `json:"has_credential"`
	Exhausted     bool         `json:"exhausted"`
	Reason        string       `json:"reason,omitempty"`
	CooldownUntil int64        `json:"cooldown_until,omitempty"`
	ResetCredits  int          `json:"reset_credits"`
	PollError     string       `json:"poll_error,omitempty"`
	ObservedAt    int64        `json:"observed_at"`
	Windows       []WindowView `json:"windows"`
	Leases        []LeaseView  `json:"leases"`
	Interactive   int          `json:"interactive"`
	Workers       int          `json:"workers"`
	Score         float64      `json:"score"`
	Eligible      bool         `json:"eligible"`
	Ineligible    string       `json:"ineligible,omitempty"`
	Rank          int          `json:"rank"`
	Terms         []string     `json:"terms"`
	// WorkerSlots is how many more workers this account accepts right now
	// (0 when it is not eligible for workers).
	WorkerSlots int `json:"worker_slots"`
}

type RankView struct {
	ID       string   `json:"id"`
	Score    float64  `json:"score"`
	Eligible bool     `json:"eligible"`
	Reason   string   `json:"reason,omitempty"`
	Terms    []string `json:"terms"`
}

type EventView struct {
	Timestamp int64  `json:"ts"`
	Provider  string `json:"provider"`
	AccountID string `json:"account"`
	Type      string `json:"type"`
	Detail    string `json:"detail"`
}

// SchemaVersion of the status JSON. Bumped only when a documented field
// changes meaning or goes away; additions do not bump it.
const SchemaVersion = 1

type View struct {
	SchemaVersion int                   `json:"schema_version"`
	GeneratedAt   int64                 `json:"generated_at"`
	Hostname      string                `json:"hostname"`
	Accounts      []AccountView         `json:"accounts"`
	Rankings      map[string][]RankView `json:"rankings"` // "<provider>/<mode>"
	Events        []EventView           `json:"events"`
	ModelScope    map[string]string     `json:"model_scope"`
	// WorkerCapacity is the number of worker slots free per provider, summed
	// over eligible accounts: the size a batch fleet can launch right now.
	WorkerCapacity map[string]int `json:"worker_capacity"`
	// WorkerCapacityByScope is the same figure per model scope: "none"
	// (scoped windows ignored) plus every scope label seen in the provider's
	// windows, lower-cased. A launcher sizing a tier-0 fleet reads
	// worker_capacity_by_scope[provider]["fable"].
	WorkerCapacityByScope map[string]map[string]int `json:"worker_capacity_by_scope"`
}

// View assembles everything the status renderers need.
func (p *Pool) View(eventLimit int) (*View, error) {
	now := time.Now()
	v := &View{SchemaVersion: SchemaVersion, GeneratedAt: now.Unix(), Hostname: Hostname(), Rankings: map[string][]RankView{}, ModelScope: map[string]string{}, WorkerCapacity: map[string]int{}, WorkerCapacityByScope: map[string]map[string]int{}}
	leases, err := p.St.PruneLeases(Hostname(), proc.Alive)
	if err != nil {
		return nil, err
	}
	// Display order and labels come from config [display].
	order := map[string]int{}
	for i, id := range p.Cfg.Display.Order {
		order[id] = i
	}
	leasesBy := map[string][]LeaseView{}
	for _, l := range leases {
		leasesBy[l.AccountID] = append(leasesBy[l.AccountID], LeaseView{
			ID: l.ID, AccountID: l.AccountID, PID: l.PID, Hostname: l.Hostname, Mode: l.Mode, Cwd: l.Cwd,
			Depth: l.Depth, ParentAccount: l.ParentAccount, Args: l.Args, StartedAt: l.StartedAt,
			Drain: l.Drain, Pane: l.Pane, Workspace: l.Workspace,
		})
	}
	for _, provider := range Providers {
		scope := p.ModelScope(provider)
		v.ModelScope[provider] = scope
		cands, err := p.Candidates(provider)
		if err != nil {
			return nil, err
		}
		ranked := map[string]map[string]RankView{}
		for _, mode := range []string{state.ModeInteractive, state.ModeWorker} {
			pol := p.Policy(provider, mode, now)
			rs := selector.Rank(pol, cands)
			ranked[mode] = map[string]RankView{}
			for i, r := range rs {
				rv := RankView{ID: r.ID, Score: r.Score, Eligible: r.Eligible, Reason: r.Reason, Terms: r.Terms}
				ranked[mode][r.ID] = rv
				_ = i
				v.Rankings[provider+"/"+mode] = append(v.Rankings[provider+"/"+mode], rv)
			}
		}
		v.WorkerCapacityByScope[provider] = p.capacityByScope(provider, cands, now)
		accounts, err := p.St.ListAccounts(provider)
		if err != nil {
			return nil, err
		}
		for _, a := range accounts {
			av := AccountView{
				ID: a.ID, Provider: a.Provider, Name: a.Name, Enabled: a.Enabled, Native: a.Native,
				Home: a.Home, QuotaID: a.QuotaID, Identity: a.Identity, HasCredential: HasCredential(a),
				Leases: leasesBy[a.ID], Terms: []string{}, Label: p.Cfg.Display.Labels[a.ID], Order: 1 << 20,
			}
			if idx, ok := order[a.ID]; ok {
				av.Order = idx
			}
			if u, ok, _ := p.St.GetUsage(a.ID); ok {
				av.Plan, av.ResetCredits, av.PollError, av.ObservedAt = u.Plan, u.ResetCredits, u.PollError, u.ObservedAt
				if u.Exhausted && u.CooldownUntil > now.Unix() {
					av.Exhausted, av.Reason, av.CooldownUntil = true, u.ExhaustedReason, u.CooldownUntil
				}
			}
			ws, _ := p.St.ListWindows(a.ID)
			for _, w := range ws {
				av.Windows = append(av.Windows, WindowView{
					Key: w.Key, Label: w.Label, Kind: w.Kind, Scope: w.Scope,
					Binding: isBinding(w, scope), UsedPct: w.UsedPct, ResetsAt: w.ResetsAt,
					WindowSeconds: w.WindowSeconds, Severity: w.Severity, Source: w.Source, ObservedAt: w.ObservedAt,
				})
				if w.ObservedAt > av.ObservedAt {
					av.ObservedAt = w.ObservedAt
				}
			}
			if av.Windows == nil {
				av.Windows = []WindowView{}
			}
			if av.Leases == nil {
				av.Leases = []LeaseView{}
			}
			for _, l := range av.Leases {
				if l.Mode == state.ModeWorker {
					av.Workers++
				} else {
					av.Interactive++
				}
			}
			if r, ok := ranked[state.ModeWorker][a.ID]; ok {
				av.Score, av.Eligible, av.Ineligible, av.Terms = r.Score, r.Eligible, r.Reason, r.Terms
				if !av.Exhausted && !r.Eligible && strings.Contains(r.Reason, "exhausted") {
					av.Exhausted, av.Reason = true, r.Reason
				}
				if r.Eligible {
					av.WorkerSlots = p.Cfg.Selection.MaxWorkersPerAccount - av.Workers
					if p.Cfg.Selection.MaxWorkersPerAccount <= 0 {
						av.WorkerSlots = 1 << 20
					}
					if av.WorkerSlots < 0 {
						av.WorkerSlots = 0
					}
					v.WorkerCapacity[provider] += av.WorkerSlots
				}
			}
			if _, ok := v.WorkerCapacity[provider]; !ok {
				v.WorkerCapacity[provider] = 0
			}
			for i, r := range v.Rankings[provider+"/"+state.ModeWorker] {
				if r.ID == a.ID {
					av.Rank = i + 1
				}
			}
			v.Accounts = append(v.Accounts, av)
		}
	}
	if v.Accounts == nil {
		v.Accounts = []AccountView{}
	}
	sort.SliceStable(v.Accounts, func(i, j int) bool {
		a, b := v.Accounts[i], v.Accounts[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return a.ID < b.ID
	})
	events, _ := p.St.ListEvents(eventLimit)
	for _, e := range events {
		v.Events = append(v.Events, EventView{Timestamp: e.Timestamp, Provider: e.Provider, AccountID: e.AccountID, Type: e.Type, Detail: e.Detail})
	}
	if v.Events == nil {
		v.Events = []EventView{}
	}
	return v, nil
}

// capacityByScope computes free worker slots per model scope: "none" plus
// every scope label present in the provider's windows.
func (p *Pool) capacityByScope(provider string, cands []selector.Candidate, now time.Time) map[string]int {
	scopes := map[string]bool{"none": true}
	for _, c := range cands {
		for _, w := range c.Windows {
			if w.Scope != "" && (w.Kind == state.KindShort || w.Kind == state.KindWeekly) {
				scopes[strings.ToLower(w.Scope)] = true
			}
		}
	}
	workers := map[string]int{}
	for _, c := range cands {
		workers[c.ID] = c.WorkerLeases
	}
	max := p.Cfg.Selection.MaxWorkersPerAccount
	out := map[string]int{}
	for scope := range scopes {
		pol := p.Policy(provider, state.ModeWorker, now)
		pol.ModelScope = scope
		if scope == "none" {
			pol.ModelScope = ""
		}
		total := 0
		for _, r := range selector.Rank(pol, cands) {
			if !r.Eligible {
				continue
			}
			if max <= 0 {
				total += 1 << 20
				continue
			}
			if free := max - workers[r.ID]; free > 0 {
				total += free
			}
		}
		out[scope] = total
	}
	return out
}

func isBinding(w state.Window, scope string) bool {
	if w.Kind != state.KindShort && w.Kind != state.KindWeekly {
		return false
	}
	if w.Scope == "" {
		return true
	}
	return scope != "" && strings.Contains(strings.ToLower(w.Scope), strings.ToLower(scope))
}

// ExhaustUntil marks an account exhausted and logs the event. The bench lasts
// until the reset of a binding window that telemetry shows near its cap; when
// no window corroborates the rejection, only until now+fallback.
func (p *Pool) ExhaustUntil(a state.Account, reason string, fallback time.Duration, now time.Time) time.Time {
	until := now.Add(fallback)
	ws, _ := p.St.ListWindows(a.ID)
	scope := p.ModelScope(a.Provider)
	best := int64(0)
	for _, w := range ws {
		if !isBinding(w, scope) || w.ResetsAt <= now.Unix() {
			continue
		}
		if w.UsedPct >= 90 && (best == 0 || w.ResetsAt < best) {
			best = w.ResetsAt
		}
	}
	if best > 0 {
		until = time.Unix(best, 0)
	}
	p.St.MarkExhausted(a.ID, reason, until, now)
	p.St.LogEvent(a.Provider, a.ID, "exhausted", fmt.Sprintf("%s (until %s)", reason, until.Local().Format("Mon 15:04")), now)
	return until
}
