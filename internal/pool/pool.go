// Package pool ties config, state and the selector together: it builds
// candidates from the store, applies the configured policy, and assembles the
// view that `aiq status` and the daemon UI render.
package pool

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/provider/agy"
	"github.com/orlenko/aiq/internal/provider/claude"
	"github.com/orlenko/aiq/internal/provider/codex"
	"github.com/orlenko/aiq/internal/provider/copilot"
	"github.com/orlenko/aiq/internal/selector"
	"github.com/orlenko/aiq/internal/state"
)

// Providers are the routed CLIs, in display order.
var Providers = config.Providers

type Pool struct {
	Cfg *config.Config
	St  *state.Store
	// Command builds a child process for a provider's real binary (set by
	// the command layer, which owns binary resolution).
	Command func(provider string, args, env []string) *exec.Cmd
	// KeepLeases makes View and Candidates read the lease table without pruning it. On
	// Windows aiq only reads for now, and its process checks cannot judge leases written
	// elsewhere.
	KeepLeases bool
}

// leases is the lease table: pruned of this host's dead leases and stale ones, unless
// KeepLeases is set.
func (p *Pool) leases() ([]state.Lease, error) {
	if p.KeepLeases {
		return p.St.ListLeases()
	}
	return p.St.PruneLeases(Hostname(), proc.Alive)
}

// command returns the process builder for one provider, or nil.
func (p *Pool) command(provider string) func(args, env []string) *exec.Cmd {
	if p.Command == nil {
		return nil
	}
	return func(args, env []string) *exec.Cmd { return p.Command(provider, args, env) }
}

// ModelScope resolves the scoped-limit name for a provider.
func (p *Pool) ModelScope(provider string) string {
	cfg := p.Cfg.Provider(provider)
	if cfg.ModelScope != "auto" {
		return cfg.ModelScope
	}
	switch provider {
	case "claude":
		return claude.DefaultModel()
	case "codex":
		return codex.DefaultModel()
	case "agy":
		return agy.ScopeOf(agy.DefaultModel(paths.RealAgyHome()))
	case "copilot":
		return copilot.DefaultModel()
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
	case "agy":
		return agy.HasCredential(a.Home, a.Native)
	case "copilot":
		// Native home is enough. No multiple accounts supported yet.
		return a.Native
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
	// On Windows both probes below would look in the wrong place: /etc/machine-id resolves
	// against the current drive, and ioreg is a macOS command.
	if runtime.GOOS == "windows" {
		machineID, _ = os.Hostname()
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
	leases, err := p.leases()
	if err != nil {
		return nil, err
	}
	f := &Facts{Now: time.Now(), Leases: leases, HasCredential: HasCredential}
	if err := p.gather(f, provider); err != nil {
		return nil, err
	}
	return p.candidatesFrom(provider, f), nil
}

// Facts are what Candidates and View read from the store, gathered once so the
// same assembly can run on a snapshot read elsewhere. Leases are the live set:
// already pruned, or filtered without pruning.
type Facts struct {
	Now      time.Time
	Hostname string
	Accounts []state.Account // ListAccounts order
	Usage    map[string]state.Usage
	Windows  map[string][]state.Window
	Leases   []state.Lease
	Whys     map[state.LeaseRef]string
	// Scope is each provider's model scope, as ModelScope resolves it.
	Scope map[string]string
	// HasCredential decides an account's credential, as HasCredential does.
	HasCredential func(state.Account) bool
}

// gather appends one provider's accounts, with their usage and windows, to f.
func (p *Pool) gather(f *Facts, provider string) error {
	if f.Usage == nil {
		f.Usage = map[string]state.Usage{}
	}
	if f.Windows == nil {
		f.Windows = map[string][]state.Window{}
	}
	accounts, err := p.St.ListAccounts(provider)
	if err != nil {
		return err
	}
	for _, acc := range accounts {
		if u, ok, err := p.St.GetUsage(acc.ID); err != nil {
			return err
		} else if ok {
			f.Usage[acc.ID] = u
		}
		ws, err := p.St.ListWindows(acc.ID)
		if err != nil {
			return err
		}
		f.Windows[acc.ID] = ws
	}
	f.Accounts = append(f.Accounts, accounts...)
	return nil
}

func (p *Pool) candidatesFrom(provider string, f *Facts) []selector.Candidate {
	interactive := map[string]int{}
	workers := map[string]int{}
	for _, l := range f.Leases {
		if l.Mode == state.ModeWorker {
			workers[l.AccountID]++
		} else {
			interactive[l.AccountID]++
		}
	}
	var cands []selector.Candidate
	for _, acc := range f.Accounts {
		if acc.Provider != provider {
			continue
		}
		c := selector.Candidate{
			ID:                acc.ID,
			Enabled:           acc.Enabled,
			HasCredential:     f.HasCredential(acc),
			Priority:          acc.Priority,
			LastSelectedAt:    acc.LastSelectedAt,
			InteractiveLeases: interactive[acc.ID],
			WorkerLeases:      workers[acc.ID],
		}
		if u, ok := f.Usage[acc.ID]; ok {
			c.CooldownUntil = u.CooldownUntil
			c.ResetCredits = u.ResetCredits
			c.ResetCreditExpiry = u.ResetCreditExpiry
			// A credential file can still hold a revoked token. Codex's
			// periodic app-server poll is also an authentication probe; once
			// it has a definitive auth rejection, keep routing off the account
			// until a successful poll clears PollError.
			if acc.Provider == "codex" && strings.Contains(strings.ToLower(u.PollError), codex.ErrAuthRequired.Error()) {
				c.UnavailableReason, c.Unavailable = "authentication required", selector.UnavailableAuthRequired
			}
			// Claude polls through aiq's own grant, so the CLI is asked
			// separately (probeClaudeLogin); a logged-out or wrongly
			// logged-in home would fail the launch or spend someone else's
			// quota.
			// An unreadable login is not excluded here: the launch decides
			// (an interactive one stops and says why, a worker moves on and
			// says so), so the account is never skipped silently.
			if why := CLILoginProblem(acc, u); why != "" && u.CLILogin != state.LoginUnreadable {
				c.UnavailableReason, c.Unavailable = why, selector.UnavailableLoginMismatch
				if u.CLILogin == state.LoginOut {
					c.Unavailable = selector.UnavailableLoginOut
				}
			}
			if ok, _ := u.Extra.Spendable(f.Now); ok && p.Cfg.Selection.ExtraUsage {
				c.ExtraUsage = fmt.Sprintf("%s prepaid left", u.Extra.Money(u.Extra.BalanceMinor))
			}
		}
		c.LastResort = p.Cfg.LastResortRank(acc.Provider)
		c.Windows = f.Windows[acc.ID]
		cands = append(cands, c)
	}
	return cands
}

// Policy builds the selector policy for a provider and mode.
func (p *Pool) Policy(provider, mode string, now time.Time) selector.Policy {
	return p.policyScope(provider, mode, p.ModelScope(provider), now)
}

func (p *Pool) policyScope(provider, mode, scope string, now time.Time) selector.Policy {
	sel := p.Cfg.Selection
	return selector.Policy{
		Now:              now,
		Mode:             mode,
		ModelScope:       scope,
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
	results := p.Poll(timeout, ids...)
	p.SpendResetCredits()
	return results
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
	// Why says why a long session cannot move, or what it waits for: free
	// text for people, whose wording may change.
	Why string `json:"why,omitempty"`
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
	CreditExpiry  int64        `json:"reset_credit_expires_at,omitempty"`
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
	// Rung is the worker ranking's cost class (selector.RungPlan, RungExtra,
	// or RungLastResort + n).
	Rung int `json:"rung"`
	// ExtraUsage is the account's paid extra usage, when it has any on;
	// ExtraUsageNote says whether aiq may spend it and, if not, why.
	ExtraUsage     *state.ExtraUsage `json:"extra_usage,omitempty"`
	ExtraUsageNote string            `json:"extra_usage_note,omitempty"`
	// CLILogin is what the CLI last said about its own login in the home:
	// "in", "out", "unreadable", or empty when never asked (only Claude is asked, since
	// its quota poll does not use the CLI's credential). CLIIdentity is the
	// email it reported; CLIMismatch is set when that is not Identity.
	CLILogin     string `json:"cli_login,omitempty"`
	CLIIdentity  string `json:"cli_identity,omitempty"`
	CLIMismatch  bool   `json:"cli_mismatch,omitempty"`
	CLICheckedAt int64  `json:"cli_checked_at,omitempty"`
	// CLILoginProblem is why the CLI login cannot be routed to, if it can't.
	CLILoginProblem string `json:"cli_login_problem,omitempty"`
}

type RankView struct {
	ID       string   `json:"id"`
	Score    float64  `json:"score"`
	Rung     int      `json:"rung"`
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
	leases, err := p.leases()
	if err != nil {
		return nil, err
	}
	f := &Facts{Now: time.Now(), Hostname: Hostname(), Leases: leases, Scope: map[string]string{}, HasCredential: HasCredential}
	f.Whys, _ = p.St.LeaseWhys()
	for _, provider := range Providers {
		f.Scope[provider] = p.ModelScope(provider)
		if err := p.gather(f, provider); err != nil {
			return nil, err
		}
	}
	v := p.ViewFrom(f)
	events, _ := p.St.ListEvents(eventLimit)
	for _, e := range events {
		v.Events = append(v.Events, EventView{Timestamp: e.Timestamp, Provider: e.Provider, AccountID: e.AccountID, Type: e.Type, Detail: e.Detail})
	}
	if v.Events == nil {
		v.Events = []EventView{}
	}
	return v, nil
}

// ViewFrom assembles a view from facts gathered beforehand, without the event
// history. It reads nothing itself: the store, the clock, the hostname, the
// model scopes and the credential check all come from f.
func (p *Pool) ViewFrom(f *Facts) *View {
	now := f.Now
	v := &View{SchemaVersion: SchemaVersion, GeneratedAt: now.Unix(), Hostname: f.Hostname, Rankings: map[string][]RankView{}, ModelScope: map[string]string{}, WorkerCapacity: map[string]int{}, WorkerCapacityByScope: map[string]map[string]int{}}
	// Display order and labels come from config [display].
	order := map[string]int{}
	for i, id := range p.Cfg.Display.Order {
		order[id] = i
	}
	leasesBy := map[string][]LeaseView{}
	whys := f.Whys
	for _, l := range f.Leases {
		leasesBy[l.AccountID] = append(leasesBy[l.AccountID], LeaseView{
			ID: l.ID, AccountID: l.AccountID, PID: l.PID, Hostname: l.Hostname, Mode: l.Mode, Cwd: l.Cwd,
			Depth: l.Depth, ParentAccount: l.ParentAccount, Args: l.Args, StartedAt: l.StartedAt,
			Drain: l.Drain, Pane: l.Pane, Workspace: l.Workspace, Why: whys[state.LeaseRef{ID: l.ID, StartedAt: l.StartedAt}],
		})
	}
	for _, provider := range Providers {
		scope := f.Scope[provider]
		v.ModelScope[provider] = scope
		cands := p.candidatesFrom(provider, f)
		ranked := map[string]map[string]RankView{}
		for _, mode := range []string{state.ModeInteractive, state.ModeWorker} {
			pol := p.policyScope(provider, mode, scope, now)
			rs := selector.Rank(pol, cands)
			ranked[mode] = map[string]RankView{}
			for i, r := range rs {
				rv := RankView{ID: r.ID, Score: r.Score, Rung: r.Rung, Eligible: r.Eligible, Reason: r.Reason, Terms: r.Terms}
				ranked[mode][r.ID] = rv
				_ = i
				v.Rankings[provider+"/"+mode] = append(v.Rankings[provider+"/"+mode], rv)
			}
		}
		v.WorkerCapacityByScope[provider] = p.capacityByScope(provider, cands, now)
		for _, a := range f.Accounts {
			if a.Provider != provider {
				continue
			}
			av := AccountView{
				ID: a.ID, Provider: a.Provider, Name: a.Name, Enabled: a.Enabled, Native: a.Native,
				Home: a.Home, QuotaID: a.QuotaID, Identity: a.Identity, HasCredential: f.HasCredential(a),
				Leases: leasesBy[a.ID], Terms: []string{}, Label: p.Cfg.Display.Labels[a.ID], Order: 1 << 20,
			}
			if idx, ok := order[a.ID]; ok {
				av.Order = idx
			}
			if u, ok := f.Usage[a.ID]; ok {
				av.Plan, av.ResetCredits, av.PollError, av.ObservedAt = u.Plan, u.ResetCredits, u.PollError, u.ObservedAt
				av.CreditExpiry = u.ResetCreditExpiry
				av.CLILogin, av.CLIIdentity, av.CLICheckedAt = u.CLILogin, u.CLIIdentity, u.CLICheckedAt
				av.CLIMismatch = CLIMismatch(a.Identity, u)
				av.CLILoginProblem = CLILoginProblem(a, u)
				if u.Extra != nil && u.Extra.Enabled {
					av.ExtraUsage = u.Extra
					ok, why := u.Extra.Spendable(now)
					switch {
					case !ok:
						av.ExtraUsageNote = why
					case !p.Cfg.Selection.ExtraUsage:
						av.ExtraUsageNote = "not spent: selection.extra_usage is off"
					default:
						av.ExtraUsageNote = "spent once plan quota runs out everywhere"
					}
				}
				if u.Exhausted && u.CooldownUntil > now.Unix() {
					av.Exhausted, av.Reason, av.CooldownUntil = true, u.ExhaustedReason, u.CooldownUntil
				}
			}
			for _, w := range f.Windows[a.ID] {
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
				av.Score, av.Eligible, av.Ineligible, av.Terms, av.Rung = r.Score, r.Eligible, r.Reason, r.Terms, r.Rung
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
	v.Events = []EventView{}
	return v
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
	// An account that can run on extra usage was not stopped by its plan
	// windows; whatever stopped it is not tied to their reset.
	extra := false
	if u, ok, _ := p.St.GetUsage(a.ID); ok && p.Cfg.Selection.ExtraUsage {
		extra, _ = u.Extra.Spendable(now)
	}
	if best > 0 && !extra {
		until = time.Unix(best, 0)
	}
	p.St.MarkExhausted(a.ID, reason, until, now)
	p.St.LogEvent(a.Provider, a.ID, "exhausted", fmt.Sprintf("%s (until %s)", reason, until.Local().Format("Mon 15:04")), now)
	return until
}
