package main

import (
	"context"
	"errors"
	"strconv"

	"github.com/orlenko/aiq/internal/herdr"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

// appTerminal is a verified exact terminal: the app focuses only a terminal
// on that Herdr machine with this live address, this root pid and this label,
// after asking for a fresh target. Nothing here is a directory, a name or
// a focus guess.
type appTerminal struct {
	Relation string     `json:"relation"` // origin or workspace_current
	Address  string     `json:"address"`
	CoreID   string     `json:"core_id"`
	Witness  appWitness `json:"witness"`
}
type appWitness struct {
	TerminalPID int    `json:"terminal_pid"`
	Label       string `json:"label"`
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
	return &out, ""
}

func (t *appTargets) check(l *state.NavigatorLease) appTargetResult {
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
	return appTargetResult{terminal: &appTerminal{Address: target.Terminal, CoreID: target.CoreID, Witness: appWitness{TerminalPID: target.PID, Label: target.Label}}}
}

type appTargetReply struct {
	Terminal *appTerminal `json:"terminal"`
	Reason   string       `json:"terminal_reason"`
}

// target is a fresh resolution for one ask (by id and revision) or one
// conversation, read when the person clicks; the app never navigates from a
// cached target.
func (env appEnv) target(ctx context.Context, f map[string]string) appResult {
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
