package main

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

type navLiveFacts struct {
	observation           terminal.Observation
	context               string
	workingKnown, working bool
	elapsedMS             int64
	accounts              []convoAccountFact
}

var errNavAuxTooLarge = errors.New("navigator metadata too large")

func (f *navFactory) liveFacts(ctx context.Context, s *transcript.Session, includeAccounts bool) (navLiveFacts, error) {
	var out navLiveFacts
	if f.input.metadata == nil {
		return out, nil
	}
	snapshot, err := f.input.metadata(ctx, state.NavigatorSnapshotLimits{Rows: f.policy.sessions.arrayEntries, TextBytes: f.policy.sessions.rowBytes})
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil || snapshot == nil {
		if errors.Is(err, state.ErrNavigatorSnapshotTooLarge) {
			return out, errNavAuxTooLarge
		}
		out.context = "metadata-unavailable"
		return out, nil
	}
	if includeAccounts {
		out.accounts, err = navAccountsFromSnapshot(ctx, s, snapshot, f.input.now(), f.policy.detail.arrayEntries, f.policy.detail.stringUnits, f.policy.sessions.rowBytes)
		if err != nil {
			return navLiveFacts{}, err
		}
	}
	var lease *state.NavigatorLease
	for i := range snapshot.Leases {
		if err := ctx.Err(); err != nil {
			return navLiveFacts{}, err
		}
		v := &snapshot.Leases[i]
		if v.Mode == state.ModeLong && v.Provider == s.Provider && v.SessionID == s.ID && v.Depth == 0 && navSameDirectory(v.Cwd, s.Cwd) {
			if lease != nil {
				out.context = "ambiguous-leases"
				return out, nil
			}
			lease = v
		}
	}
	var latest *state.NavigatorLaunch
	for i := range snapshot.Launches {
		if err := ctx.Err(); err != nil {
			return navLiveFacts{}, err
		}
		v := &snapshot.Launches[i]
		if v.Provider == s.Provider && v.SessionID == s.ID && (v.Mode == state.ModeInteractive || v.Mode == state.ModeLong) && navSameDirectory(v.Cwd, s.Cwd) {
			if latest == nil || v.StartedMS > latest.StartedMS || v.StartedMS == latest.StartedMS && v.ID > latest.ID {
				latest = v
			}
		}
	}
	latestID := ""
	if latest != nil {
		latestID = strconv.FormatInt(latest.ID, 10)
	}
	if lease == nil {
		out.context = navConversationKey("nav-aux-context-v1", "no-managed-lease", latestID)
		return out, nil
	}
	launchID := navLeaseLaunchID(ctx, snapshot, lease, false)
	out.context = navConversationKey("nav-aux-context-v1", strconv.FormatInt(lease.ID, 10), strconv.FormatInt(lease.StartedAt, 10), strconv.FormatInt(lease.PID, 10), lease.Pane, lease.AccountID, lease.Provider, lease.SessionID, lease.Workspace, strconv.FormatInt(launchID, 10), latestID)
	observed, err := f.observeNavLease(ctx, snapshot, lease, launchID)
	if err != nil {
		return navLiveFacts{}, err
	}
	out.observation = observed
	if s.Provider != "agy" && observed == terminal.Running && lease.TurnStartedAt > 0 && lease.TurnStartedAt <= navConversationSafeInteger/1000 && lease.TurnEndedAt >= 0 && lease.TurnEndedAt <= navConversationSafeInteger/1000 {
		now := f.input.now().UnixMilli()
		start := lease.TurnStartedAt * 1000
		if now >= start && now <= navConversationSafeInteger {
			out.workingKnown = true
			out.working = lease.TurnStartedAt > lease.TurnEndedAt
			if out.working {
				out.elapsedMS = now - start
			}
		}
	}
	return out, nil
}
func navSameDirectory(a, b string) bool {
	return a != "" && b != "" && filepath.IsAbs(a) && filepath.IsAbs(b) && filepath.Clean(a) == filepath.Clean(b)
}

type navAccountSpan struct {
	at         int64
	account    string
	ambiguous  bool
	nextChange int
}

func navAccountsFromSnapshot(ctx context.Context, s *transcript.Session, snapshot *state.NavigatorSnapshot, now time.Time, entries, units, bytes int64) ([]convoAccountFact, error) {
	var spans []navAccountSpan
	for _, v := range snapshot.Launches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if v.Provider == s.Provider && v.SessionID == s.ID && (v.Mode == state.ModeInteractive || v.Mode == state.ModeLong) && navSameDirectory(v.Cwd, s.Cwd) {
			if int64(len(spans)) >= entries {
				return nil, errNavAuxTooLarge
			}
			spans = append(spans, navAccountSpan{at: v.StartedMS, account: v.AccountID})
		}
	}
	if len(spans) == 0 {
		return nil, nil
	}
	if int64(len(s.Turns)) > entries {
		return nil, errNavAuxTooLarge
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].at < spans[j].at })
	groups := spans[:0]
	for _, v := range spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(groups) > 0 && groups[len(groups)-1].at == v.at {
			g := &groups[len(groups)-1]
			if g.account != v.account {
				g.ambiguous = true
			}
			continue
		}
		groups = append(groups, v)
	}
	for i := len(groups) - 1; i >= 0; i-- {
		groups[i].nextChange = len(groups)
		if i+1 < len(groups) {
			if groups[i+1].ambiguous || groups[i].account != groups[i+1].account {
				groups[i].nextChange = i + 1
			} else {
				groups[i].nextChange = groups[i+1].nextChange
			}
		}
	}
	out := make([]convoAccountFact, len(s.Turns))
	known := false
	for i, turn := range s.Turns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if turn.Started.IsZero() {
			continue
		}
		start := turn.Started.UnixMilli()
		if start < 0 || start > navConversationSafeInteger {
			continue
		}
		at := sort.Search(len(groups), func(j int) bool { return groups[j].at > start }) - 1
		if at < 0 || groups[at].ambiguous || groups[at].account == "" || groups[at].at <= 0 {
			continue
		}
		end := turn.Ended
		if end.IsZero() {
			end = turn.ReplyAt
		}
		if turn.Open {
			end = now
		}
		if end.IsZero() || end.Before(turn.Started) {
			continue
		}
		complete := true
		for _, item := range turn.Items {
			if item.At.IsZero() {
				complete = false
				break
			}
			if item.At.After(end) {
				end = item.At
			}
		}
		if !complete {
			continue
		}
		if next := groups[at].nextChange; next < len(groups) && groups[next].at <= end.UnixMilli() {
			continue
		}
		label := oneLine(groups[at].account)
		count := int64(0)
		for _, r := range label {
			count++
			if r > 0xffff {
				count++
			}
		}
		if count > units || int64(len(label)) > bytes {
			return nil, errNavAuxTooLarge
		}
		units -= count
		bytes -= int64(len(label))
		out[i] = convoAccountFact{known: true, label: label}
		known = true
	}
	if !known {
		return nil, nil
	}
	return out, nil
}
