package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

// appTerminal is a verified terminal. Before focusing, the app asks for a
// fresh target and checks the address, root pid and child label or legacy
// creation time on that Herdr machine.
type appTerminal struct {
	Relation string     `json:"relation"` // origin or workspace_current
	Address  string     `json:"address"`
	CoreID   string     `json:"core_id"`
	Witness  appWitness `json:"witness"`
}
type appWitness struct {
	TerminalPID       int    `json:"terminal_pid"`
	Label             string `json:"label"`
	TerminalCreatedMS int64  `json:"terminal_created_ms,omitempty"`
}

// Reasons a row has no terminal.
const (
	appNoLease     = "no_lease"
	appAmbiguous   = "ambiguous"
	appNotHerdr    = "not_herdr"
	appEnded       = "ended"
	appUnknownTerm = "unknown"
)

type appTargetResult struct {
	terminal *appTerminal
	reason   string
}

// appTargets resolves terminals from one launch-metadata snapshot, checking
// each lease at most once per run.
type appTargets struct {
	ctx      context.Context
	env      appEnv
	f        *navFactory
	snapshot *state.NavigatorSnapshot
	failed   bool
	done     map[int64]appTargetResult
	inter    *appInteractive
}

func (env appEnv) targets(ctx context.Context) *appTargets {
	t := &appTargets{ctx: ctx, env: env, done: map[int64]appTargetResult{}}
	f, err := newNavFactory(env.input, navProductionPolicy())
	if err != nil || env.input.metadata == nil {
		t.failed = true
		return t
	}
	t.f = f
	limits := navProductionPolicy().sessions
	snapshot, err := env.input.metadata(ctx, state.NavigatorSnapshotLimits{Rows: limits.arrayEntries, TextBytes: limits.rowBytes})
	if err != nil || snapshot == nil {
		t.failed = true
		return t
	}
	t.snapshot = snapshot
	return t
}

func appLongTop(l *state.NavigatorLease) bool { return l.Mode == state.ModeLong && l.Depth == 0 }

// forConversation is the terminal running exactly that conversation.
func (t *appTargets) forConversation(provider, id string) (*appTerminal, string) {
	if t.failed {
		return nil, appUnknownTerm
	}
	if !transcript.IsFullID(provider, id) {
		return nil, appNoLease
	}
	var match *state.NavigatorLease
	for i := range t.snapshot.Leases {
		l := &t.snapshot.Leases[i]
		if appLongTop(l) && l.Provider == provider && l.SessionID == id {
			if match != nil {
				return nil, appAmbiguous
			}
			match = l
		}
	}
	if provider == "claude" {
		in := t.interactive()
		for i := range t.snapshot.Leases {
			l := &t.snapshot.Leases[i]
			if l.Mode == state.ModeInteractive && in.links[l.ID] == id {
				if match != nil {
					return nil, appAmbiguous
				}
				match = l
			}
		}
		if match == nil && in.failed {
			return nil, appUnknownTerm // an interactive session may hold it
		}
	}
	if match == nil {
		return nil, appNoLease
	}
	return t.resolve(match, "origin")
}

// forAsk is the terminal of the ask's own conversation; failing any lease
// for it, the one running long session in the ask's workspace.
func (t *appTargets) forAsk(a state.Ask) (*appTerminal, string) {
	if t.failed {
		return nil, appUnknownTerm
	}
	term, reason := t.forConversation(a.Provider, a.SessionID)
	if reason != appNoLease || a.Workspace == "" {
		return term, reason
	}
	want := canonicalDir(a.Workspace)
	var match *state.NavigatorLease
	for i := range t.snapshot.Leases {
		l := &t.snapshot.Leases[i]
		if appLongTop(l) && l.Workspace != "" && canonicalDir(l.Workspace) == want {
			if match != nil {
				return nil, appAmbiguous
			}
			match = l
		}
	}
	if match == nil {
		return nil, appNoLease
	}
	return t.resolve(match, "workspace_current")
}

func (t *appTargets) resolve(l *state.NavigatorLease, relation string) (*appTerminal, string) {
	r, seen := t.done[l.ID]
	if !seen {
		r = t.check(l)
		t.done[l.ID] = r
	}
	if r.terminal == nil {
		return nil, r.reason
	}
	out := *r.terminal
	out.Relation = relation
	if out.Witness.TerminalCreatedMS > 0 {
		out.Relation = "workspace_current"
	}
	return &out, ""
}

func (t *appTargets) check(l *state.NavigatorLease) appTargetResult {
	if l.Mode == state.ModeInteractive {
		return t.checkInteractive(l)
	}
	if !herdr.IsRef(l.Pane) {
		return appTargetResult{reason: appNotHerdr}
	}
	if t.ctx.Err() != nil {
		return appTargetResult{reason: appUnknownTerm}
	}
	observed, err := t.f.observeNavLease(t.ctx, t.snapshot, l, navLeaseLaunchID(t.ctx, t.snapshot, l, true))
	if err != nil {
		return appTargetResult{reason: appUnknownTerm}
	}
	switch observed {
	case terminal.Ended:
		return appTargetResult{reason: appEnded}
	case terminal.Running:
	default:
		return appTargetResult{reason: appUnknownTerm}
	}
	target, reason := t.env.resolve(l.Pane)
	if reason != "" {
		return appTargetResult{reason: reason}
	}
	return appTargetResult{terminal: &appTerminal{Address: target.Terminal, CoreID: target.CoreID, Witness: appWitness{TerminalPID: target.PID, Label: target.Label, TerminalCreatedMS: target.CreatedMS}}}
}

type appTargetReply struct {
	Terminal *appTerminal `json:"terminal"`
	Reason   string       `json:"terminal_reason"`
}

// target is a fresh resolution for one ask (by id and revision) or one
// conversation, read when the person clicks; the app never navigates from a
// cached target.
func (env appEnv) target(ctx context.Context, f map[string]string) appResult {
	if f["lease"] != "" {
		if len(f) != 1 {
			return appFailure(appInvalid)
		}
		id, err := strconv.ParseInt(f["lease"], 10, 64)
		if err != nil {
			return appFailure(appInvalid)
		}
		term, reason := env.targets(ctx).forLease(id)
		return appResult{data: appTargetReply{term, reason}}
	}
	byAsk := f["ask"] != "" || f["revision"] != ""
	byConversation := f["provider"] != "" || f["conversation"] != ""
	if byAsk == byConversation || byAsk && (f["ask"] == "" || f["revision"] == "") || byConversation && (f["provider"] == "" || f["conversation"] == "") {
		return appFailure(appInvalid)
	}
	if byConversation {
		term, reason := env.targets(ctx).forConversation(f["provider"], f["conversation"])
		return appResult{data: appTargetReply{term, reason}}
	}
	id, err := strconv.ParseInt(f["ask"], 10, 64)
	if err != nil {
		return appFailure(appInvalid)
	}
	a, err := env.readAsk(ctx, id, f["revision"])
	var code appCodeError
	if errors.As(err, &code) {
		return appFailure(string(code))
	} else if err != nil {
		return appFailure(appInternal)
	}
	term, reason := env.targets(ctx).forAsk(a)
	return appResult{data: appTargetReply{term, reason}}
}

// linkSessionTargets annotates the bounded history window from one fresh launch
// snapshot. The app still resolves again on a click; these are display hints.
func (env appEnv) linkSessionTargets(ctx context.Context, data any) {
	d, ok := data.(map[string]any)
	if !ok {
		return
	}
	rows, ok := d["rows"].([]map[string]any)
	if !ok {
		return
	}
	t := env.targets(ctx)
	represented := map[string]bool{}
	for _, row := range rows {
		var term *appTerminal
		var reason string
		if id, _ := row["id"].(string); id != "" {
			provider, _ := row["provider"].(string)
			term, reason = t.forConversation(provider, id)
		} else if lease, _ := row["lease_id"].(string); lease != "" {
			id, err := strconv.ParseInt(lease, 10, 64)
			if err == nil {
				term, reason = t.forLease(id)
			} else {
				reason = appUnknownTerm
			}
		} else {
			reason = appNoLease
		}
		row["terminal"], row["terminal_reason"] = term, reason
		if id, _ := row["lease_id"].(string); id != "" {
			represented[id] = true
		}
		if term != nil {
			row["live"] = "live"
			represented[term.CoreID+"/"+term.Address] = true
		}
	}
	// Interactive providers without an exact registered conversation still have
	// a lease row. Do not guess a transcript from its directory or title.
	if t.failed {
		return
	}
	for i := range t.snapshot.Leases {
		l := &t.snapshot.Leases[i]
		provider := appLeaseProvider(l)
		if !appProviderPattern.MatchString(provider) {
			continue
		}
		id := strconv.FormatInt(l.ID, 10)
		if l.Mode != state.ModeInteractive || l.Depth != 0 || represented[id] {
			continue
		}
		term, reason := t.forLease(l.ID)
		if term != nil && represented[term.CoreID+"/"+term.Address] {
			continue
		}
		live := "unknown"
		if term != nil {
			live = "live"
		} else if reason == appEnded {
			live = "ended"
		}
		rows = append(rows, map[string]any{"kind": "workspace", "provider": appLeaseProvider(l), "id": "", "lease_id": id, "cwd": l.Cwd, "workspace": l.Cwd, "workspace_known": false, "name": "", "title": "", "model": "", "branch": "", "started_ms": time.Unix(l.StartedAt, 0).UnixMilli(), "updated_ms": time.Unix(l.StartedAt, 0).UnixMilli(), "turns": 0, "tools": 0, "worker": false, "pending_asks": 0, "pending_asks_known": false, "live": live, "account": l.AccountID, "launcher": l.Launcher, "terminal": term, "terminal_reason": reason})
	}
	d["rows"] = rows
}
