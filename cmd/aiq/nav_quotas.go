package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/daemon"
	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/fleet"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

// navQuotaDeps are the quota read's sources, each owned and bounded by its
// caller. Nothing here polls a provider, prunes a lease or writes the store.
type navQuotaDeps struct {
	// read takes one admitted snapshot of accounts, usage, windows and leases.
	read func(context.Context, state.QuotaSnapshotLimits) (*state.QuotaSnapshot, error)
	// host is this machine's lease identity, the one PruneLeases compares.
	host func(context.Context) (string, error)
	// scope is a provider's model scope; "" when none is resolved.
	scope func(context.Context, string) string
	// poll copies the daemon's poll bookkeeping.
	poll func() daemon.PollStatus
	// alive and credential are PruneLeases' and HasCredential's checks.
	alive      func(int) bool
	credential func(state.Account) bool
	// view assembles the ranked view from facts, with the daemon's config.
	view func(*pool.Facts) *pool.View
	// fleet is what the other machines in [fleet] last reported, in config
	// order; nil when none are configured. It never runs ssh itself for
	// longer than its own short wait.
	fleet func(context.Context) []fleet.Host
}

// The wire document of GET /api/nav/quotas, schema 2. A null value is unknown;
// zero is never used to mean unknown. It carries what the status page shows
// and nothing it hides: no account home, quota id, launch arguments, lease host,
// events or worker capacity.
type navQuotasBody struct {
	Schema      int64             `json:"schema"`
	GeneratedAt int64             `json:"generated_at"`
	Hostname    string            `json:"hostname"`
	Poll        navQuotaPoll      `json:"poll"`
	ModelScope  []navQuotaScope   `json:"model_scope"`
	Accounts    []navQuotaAccount `json:"accounts"`
	Rankings    []navQuotaRanking `json:"rankings"`
	// Machines whose sessions the accounts list: this one first, then [fleet]
	// hosts in config order.
	Machines []navQuotaMachine `json:"machines"`
}
type navQuotaMachine struct {
	Name     string `json:"name"` // "" for this machine
	Here     bool   `json:"here"`
	Status   string `json:"status"` // ok, error or pending
	SeenAt   *int64 `json:"seen_at"`
	Error    string `json:"error"`
	Sessions int64  `json:"sessions"` // shown under an account here
	// Unmatched counts sessions on accounts this machine does not have.
	Unmatched int64 `json:"unmatched"`
}
type navQuotaPoll struct {
	LastAt    *int64 `json:"last_at"`
	IntervalS int64  `json:"interval_s"`
	Polling   bool   `json:"polling"`
	Error     string `json:"error"`
}
type navQuotaScope struct {
	Provider string `json:"provider"`
	Scope    string `json:"scope"`
}
type navQuotaAccount struct {
	ID                   string           `json:"id"`
	Provider             string           `json:"provider"`
	Plan                 string           `json:"plan"`
	Label                string           `json:"label"`
	Identity             string           `json:"identity"`
	Enabled              bool             `json:"enabled"`
	Login                navQuotaLogin    `json:"login"`
	Eligible             bool             `json:"eligible"`
	Ineligible           string           `json:"ineligible"`
	Exhausted            bool             `json:"exhausted"`
	Reason               string           `json:"reason"`
	CooldownUntil        *int64           `json:"cooldown_until"`
	Rank                 int64            `json:"rank"`
	Score                *float64         `json:"score"`
	Rung                 int64            `json:"rung"`
	Terms                []string         `json:"terms"`
	ResetCredits         int64            `json:"reset_credits"`
	ResetCreditExpiresAt *int64           `json:"reset_credit_expires_at"`
	ExtraUsage           *navQuotaExtra   `json:"extra_usage"`
	PollError            string           `json:"poll_error"`
	ObservedAt           *int64           `json:"observed_at"`
	Windows              []navQuotaWindow `json:"windows"`
	Leases               []navQuotaLease  `json:"leases"`
}
type navQuotaLogin struct {
	HasCredential bool   `json:"has_credential"`
	CLI           string `json:"cli"` // in, out, unreadable or unknown
	CLIIdentity   string `json:"cli_identity"`
	CLIMismatch   bool   `json:"cli_mismatch"`
	CheckedAt     *int64 `json:"checked_at"`
	Problem       string `json:"problem"`
}
type navQuotaExtra struct {
	Currency     string `json:"currency"`
	BalanceMinor *int64 `json:"balance_minor"`
	UsedMinor    int64  `json:"used_minor"`
	LimitMinor   int64  `json:"limit_minor"` // 0 = no monthly limit
	LimitReached bool   `json:"limit_reached"`
	Note         string `json:"note"`
}
type navQuotaWindow struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	Kind          string   `json:"kind"`
	Scope         string   `json:"scope"`
	Binding       bool     `json:"binding"`
	UsedPct       *float64 `json:"used_pct"`
	ResetsAt      *int64   `json:"resets_at"`
	WindowSeconds int64    `json:"window_seconds"`
	Source        string   `json:"source"`
	ObservedAt    *int64   `json:"observed_at"`
}
type navQuotaLease struct {
	Mode      string `json:"mode"`
	Drain     string `json:"drain"`
	Depth     int64  `json:"depth"`
	Pane      string `json:"pane"`
	PID       int64  `json:"pid"`
	Workspace string `json:"workspace"`
	Cwd       string `json:"cwd"`
	Why       string `json:"why"`
	Machine   string `json:"machine"` // "" for this machine
}
type navQuotaRanking struct {
	Key     string         `json:"key"` // "<provider>/<mode>"
	Entries []navQuotaRank `json:"entries"`
}
type navQuotaRank struct {
	ID       string   `json:"id"`
	Score    *float64 `json:"score"`
	Rung     int64    `json:"rung"`
	Eligible bool     `json:"eligible"`
	Reason   string   `json:"reason"`
	Terms    []string `json:"terms"`
}

func navQuotasDecode(r *http.Request) bool {
	if r == nil || r.URL == nil || r.Method != "GET" {
		return false
	}
	u := r.URL
	return u.Path == "/api/nav/quotas" && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawFragment == "" && u.Opaque == "" &&
		(r.RequestURI == "" || r.RequestURI == u.RequestURI())
}

func (f *navFactory) quotas(w http.ResponseWriter, r *http.Request) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	result := f.prepareQuotas(r, &release)
	h := w.Header()
	if r.Context().Err() != nil {
		result = navListError(503, "request_cancelled")
	}
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	daemon.WriteNavigatorBody(w, r, h, result.status, result.body)
}

func (f *navFactory) prepareQuotas(r *http.Request, release *func()) (out navListResponse) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			diagnostic.Note(r.Context(), transcript.CapturePanic(v))
			out = navListError(500, "internal_error")
		}
	}()
	if !navQuotasDecode(r) {
		return navListError(400, "invalid_request")
	}
	ctx, cancel := context.WithTimeout(r.Context(), f.policy.timeout)
	defer cancel()
	// The slot is held until the body is built: nothing outlives this call.
	select {
	case f.quotaSlots <- struct{}{}:
		*release = func() { <-f.quotaSlots }
	case <-ctx.Done():
		return navListError(503, "quotas_busy")
	}
	d := f.input.quotas
	if d == nil || d.read == nil || d.host == nil || d.scope == nil || d.poll == nil || d.alive == nil || d.credential == nil || d.view == nil {
		return navListError(503, "quotas_unavailable")
	}
	body, err := navQuotasBuild(ctx, d, f.input.now(), state.QuotaSnapshotLimits{Rows: f.policy.sessions.arrayEntries, TextBytes: f.policy.sessions.rowBytes})
	if r.Context().Err() != nil {
		return navListError(503, "request_cancelled")
	}
	if errors.Is(err, state.ErrQuotaSnapshotTooLarge) {
		return navListError(503, "quotas_too_large")
	}
	if err != nil || ctx.Err() != nil {
		return navListError(503, "quotas_unavailable")
	}
	size, ok := navJSONSize(*body, f.policy.sessions.bodyBytes)
	if !ok {
		return navListError(503, "quotas_too_large")
	}
	encoded, err := json.Marshal(*body)
	if err != nil || int64(len(encoded)) != size {
		return navListError(500, "internal_error")
	}
	return navListResponse{status: 200, body: encoded}
}

var errNavQuotasUnavailable = errors.New("navigator quotas unavailable")

func navQuotasBuild(ctx context.Context, d *navQuotaDeps, now time.Time, limits state.QuotaSnapshotLimits) (*navQuotasBody, error) {
	host, err := d.host(ctx)
	if err != nil || host == "" {
		return nil, errNavQuotasUnavailable
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	snap, err := d.read(ctx, limits)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errNavQuotasUnavailable
	}
	facts := &pool.Facts{Now: now, Hostname: host, Accounts: snap.Accounts, Usage: snap.Usage, Windows: snap.Windows, Whys: snap.Whys,
		Scope: map[string]string{}, HasCredential: d.credential}
	// What PruneLeases would keep, without releasing what it would not.
	cutoff := now.Add(-state.MaxLeaseAge).Unix()
	for _, l := range snap.Leases {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if !state.LeaseGone(l, host, d.alive, cutoff) {
			facts.Leases = append(facts.Leases, l)
		}
	}
	for _, provider := range pool.Providers {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		facts.Scope[provider] = d.scope(ctx, provider)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	v := d.view(facts)
	if v == nil {
		return nil, errNavQuotasUnavailable
	}
	poll := d.poll()
	body := &navQuotasBody{Schema: 2, GeneratedAt: v.GeneratedAt, Hostname: v.Hostname,
		Poll:       navQuotaPoll{IntervalS: poll.IntervalSeconds, Polling: poll.Polling, Error: poll.LastError},
		ModelScope: []navQuotaScope{}, Accounts: make([]navQuotaAccount, 0, len(v.Accounts)), Rankings: []navQuotaRanking{}}
	if !poll.LastPoll.IsZero() {
		body.Poll.LastAt = navQuotaTime(poll.LastPoll.Unix())
	}
	for _, provider := range pool.Providers {
		body.ModelScope = append(body.ModelScope, navQuotaScope{Provider: provider, Scope: v.ModelScope[provider]})
	}
	for _, a := range v.Accounts {
		body.Accounts = append(body.Accounts, navQuotaAccountOf(a))
	}
	keys := make([]string, 0, len(v.Rankings))
	for key := range v.Rankings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ranking := navQuotaRanking{Key: key, Entries: []navQuotaRank{}}
		for _, r := range v.Rankings[key] {
			ranking.Entries = append(ranking.Entries, navQuotaRank{ID: r.ID, Score: navQuotaFinite(r.Score), Rung: int64(r.Rung), Eligible: r.Eligible, Reason: r.Reason, Terms: navQuotaTerms(r.Terms)})
		}
		body.Rankings = append(body.Rankings, ranking)
	}
	here := navQuotaMachine{Here: true, Status: fleet.OK, SeenAt: navQuotaTime(v.GeneratedAt)}
	for _, a := range body.Accounts {
		here.Sessions += int64(len(a.Leases))
	}
	body.Machines = []navQuotaMachine{here}
	if d.fleet != nil {
		hosts := d.fleet(ctx)
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		for _, h := range hosts {
			body.Machines = append(body.Machines, navQuotaAttach(body.Accounts, h))
		}
	}
	return body, nil
}

// navQuotaAttach adds a host's sessions to the accounts they run on, and
// returns the host's line. The machines share subscriptions, not account ids:
// a session belongs to the account of the same provider and identity (the
// email the provider reports), when exactly one has it; otherwise to the
// account of the same provider and id, unless both identities are known and
// differ.
func navQuotaAttach(accounts []navQuotaAccount, h fleet.Host) navQuotaMachine {
	m := navQuotaMachine{Name: h.Name, Status: h.Status, SeenAt: navQuotaTime(h.SeenAt), Error: h.Error}
	for _, l := range h.Leases {
		i := navQuotaMatch(accounts, l)
		if i < 0 {
			m.Unmatched++
			continue
		}
		accounts[i].Leases = append(accounts[i].Leases, navQuotaLease{Mode: l.Mode, Drain: l.Drain, Depth: int64(l.Depth), Pane: l.Pane,
			PID: int64(l.PID), Workspace: l.Workspace, Cwd: l.Cwd, Why: l.Why, Machine: h.Name})
		m.Sessions++
	}
	return m
}

func navQuotaMatch(accounts []navQuotaAccount, l fleet.Lease) int {
	if l.Identity != "" {
		found := -1
		for i, a := range accounts {
			if a.Provider == l.Provider && strings.EqualFold(a.Identity, l.Identity) {
				if found >= 0 {
					found = -2
					break
				}
				found = i
			}
		}
		if found >= 0 {
			return found
		}
	}
	for i, a := range accounts {
		if a.Provider == l.Provider && a.ID == l.Account && (a.Identity == "" || l.Identity == "" || strings.EqualFold(a.Identity, l.Identity)) {
			return i
		}
	}
	return -1
}

func navQuotaAccountOf(a pool.AccountView) navQuotaAccount {
	cli := a.CLILogin
	if cli != state.LoginIn && cli != state.LoginOut && cli != state.LoginUnreadable {
		cli = "unknown"
	}
	out := navQuotaAccount{ID: a.ID, Provider: a.Provider, Plan: a.Plan, Label: a.Label, Identity: a.Identity, Enabled: a.Enabled,
		Login: navQuotaLogin{HasCredential: a.HasCredential, CLI: cli, CLIIdentity: a.CLIIdentity, CLIMismatch: a.CLIMismatch,
			CheckedAt: navQuotaTime(a.CLICheckedAt), Problem: a.CLILoginProblem},
		Eligible: a.Eligible, Ineligible: a.Ineligible, Exhausted: a.Exhausted, Reason: a.Reason, CooldownUntil: navQuotaTime(a.CooldownUntil),
		Rank: int64(a.Rank), Score: navQuotaFinite(a.Score), Rung: int64(a.Rung), Terms: navQuotaTerms(a.Terms),
		ResetCredits: int64(a.ResetCredits), ResetCreditExpiresAt: navQuotaTime(a.CreditExpiry),
		PollError: a.PollError, ObservedAt: navQuotaTime(a.ObservedAt),
		Windows: make([]navQuotaWindow, 0, len(a.Windows)), Leases: make([]navQuotaLease, 0, len(a.Leases))}
	if e := a.ExtraUsage; e != nil {
		out.ExtraUsage = &navQuotaExtra{Currency: e.Currency, UsedMinor: e.UsedMinor, LimitMinor: e.LimitMinor, LimitReached: e.LimitReached, Note: a.ExtraUsageNote}
		if e.BalanceMinor >= 0 {
			balance := e.BalanceMinor
			out.ExtraUsage.BalanceMinor = &balance
		}
	}
	for _, w := range a.Windows {
		win := navQuotaWindow{Key: w.Key, Label: w.Label, Kind: w.Kind, Scope: w.Scope, Binding: w.Binding,
			ResetsAt: navQuotaTime(w.ResetsAt), WindowSeconds: w.WindowSeconds, Source: w.Source, ObservedAt: navQuotaTime(w.ObservedAt)}
		if w.UsedPct >= 0 && !math.IsNaN(w.UsedPct) && !math.IsInf(w.UsedPct, 0) {
			used := w.UsedPct
			win.UsedPct = &used
		}
		out.Windows = append(out.Windows, win)
	}
	for _, l := range a.Leases {
		out.Leases = append(out.Leases, navQuotaLease{Mode: l.Mode, Drain: l.Drain, Depth: int64(l.Depth), Pane: l.Pane, PID: int64(l.PID), Workspace: l.Workspace, Cwd: l.Cwd, Why: l.Why})
	}
	return out
}

// navQuotaTime is a unix time, or nil for the store's 0 (never, or unknown).
func navQuotaTime(unix int64) *int64 {
	if unix <= 0 {
		return nil
	}
	return &unix
}

// navQuotaFinite is a score, or nil when it is not a finite number.
func navQuotaFinite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func navQuotaTerms(terms []string) []string {
	if terms == nil {
		return []string{}
	}
	return terms
}
