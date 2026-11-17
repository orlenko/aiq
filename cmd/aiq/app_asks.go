package main

import (
	"context"
	"errors"
	"os"
	"strconv"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/state"
)

var appSections = []string{"asks", "warnings", "other_open", "probably_done", "closed_dismissed"}

type appText struct {
	Preview   string `json:"preview,omitempty"`
	Text      string `json:"text,omitempty"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}
type appSessionName struct {
	Name   string `json:"name"`
	Source string `json:"name_source"`
}
type appAskRow struct {
	ID             string         `json:"id"`
	Revision       string         `json:"revision"`
	State          string         `json:"state"`
	Section        string         `json:"section"`
	Provider       string         `json:"provider"`
	ConversationID string         `json:"conversation_id"`
	Workspace      string         `json:"workspace"`
	RaisedAt       int64          `json:"raised_at_ms"`
	StateAt        int64          `json:"state_at_ms"`
	StateReason    string         `json:"state_reason"`
	Repeats        int            `json:"repeats"`
	First          appText        `json:"first"`
	Latest         appText        `json:"latest"`
	Session        appSessionName `json:"session"`
	Terminal       *appTerminal   `json:"terminal"`
	TerminalReason string         `json:"terminal_reason"`
}
type appAskCounts struct {
	Known        bool  `json:"known"`
	Asks         int64 `json:"asks"`
	Warnings     int64 `json:"warnings"`
	OtherOpen    int64 `json:"other_open"`
	ProbablyDone int64 `json:"probably_done"`
	ClosedOrGone int64 `json:"closed_dismissed"`
	Open         int64 `json:"open"`
}
type appWatcher struct {
	Known     bool   `json:"known"`
	PassAt    int64  `json:"pass_at_ms"`
	ErrorAt   int64  `json:"error_at_ms"`
	LastError string `json:"last_error"`
	Watched   int    `json:"watched"`
	Waiting   int    `json:"waiting"`
}
type appAsks struct {
	Setup   string       `json:"setup"`
	Counts  appAskCounts `json:"counts"`
	Watcher appWatcher   `json:"watcher"`
	Rows    []appAskRow  `json:"rows"`
}

// appPreview is the first appPreviewRunes characters of s, with its full size.
func appPreview(s string) appText {
	out := appText{Preview: s, Bytes: len(s)}
	if utf8.RuneCountInString(s) > appPreviewRunes {
		n := 0
		for i := range s {
			if n == appPreviewRunes {
				out.Preview, out.Truncated = s[:i], true
				break
			}
			n++
		}
	}
	return out
}

// appFull is s up to appTextBytes, cut on a character boundary.
func appFull(s string) appText {
	out := appText{Text: s, Bytes: len(s)}
	if len(s) > appTextBytes {
		end := appTextBytes
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		out.Text, out.Truncated = s[:end], true
	}
	return out
}

// appSection is an ask's section index in appSections.
func appSection(a state.Ask, now int64) int {
	switch a.State {
	case state.AskOpen:
		switch asksGroup(a, now) {
		case "Asks":
			return 0
		case "Warnings":
			return 1
		}
		return 2
	case state.AskProbablyDone:
		return 3
	}
	return 4
}

// appAskRowsMax bounds the full rows one asks reply reads; far more than a
// 4 MiB reply of previews can hold.
const appAskRowsMax = 6000

// appReadAsks takes one read-only window of the asks store: every ask's index
// entry, and full rows only for the asks choose picks. A missing store is not
// set up, never zero asks.
func (env appEnv) appReadAsks(ctx context.Context, choose func([]state.AskIndex) []int64) (*state.AsksWindow, string) {
	if _, err := os.Lstat(env.asksPath); os.IsNotExist(err) {
		return nil, appNotSetUp
	}
	store, err := state.OpenReadOnly(env.asksPath)
	if err != nil {
		return nil, appUnavailable
	}
	defer store.Close()
	reader := navProductionPolicy().asks.reader
	w, err := store.ReadAsksWindow(ctx, state.AsksWindowLimits{Rows: reader.Rows, TextBytes: reader.TextBytes}, choose)
	if err != nil || w == nil {
		return nil, appErrorFor(ctx, err)
	}
	return w, ""
}

// appOne picks the ask with this decimal id, if the index has it.
func appOne(id string) func([]state.AskIndex) []int64 {
	return func(index []state.AskIndex) []int64 {
		for _, v := range index {
			if appIDString(v.ID) == id {
				return []int64{v.ID}
			}
		}
		return nil
	}
}

// appCodeError is a handled failure carrying its fixed error code.
type appCodeError string

func (e appCodeError) Error() string { return string(e) }

// readAsk is the bounded point read history commands use: the ask with this
// id at exactly this revision. Errors are appCodeError: not_set_up,
// not_found, conflict (another revision), busy, too_large or unavailable.
func (env appEnv) readAsk(ctx context.Context, id int64, revision string) (state.Ask, error) {
	key := appIDString(id)
	w, code := env.appReadAsks(ctx, appOne(key))
	if code != "" {
		return state.Ask{}, appCodeError(code)
	}
	for _, a := range w.Items {
		if a.ID != id {
			continue
		}
		if state.AskActionRevision(a) != revision {
			return state.Ask{}, appCodeError(appConflict)
		}
		return a, nil
	}
	return state.Ask{}, appCodeError(appNotFound)
}

func appIndexSection(v state.AskIndex, now int64) int {
	return appSection(state.Ask{State: v.State, RaisedAt: v.RaisedAt, AsksReader: v.AsksReader, Risk: v.Risk}, now)
}

func (env appEnv) asks(ctx context.Context) appResult {
	now := env.now().Unix()
	out := appAsks{Setup: "ready", Counts: appAskCounts{Known: true}, Rows: []appAskRow{}}
	counts := []*int64{&out.Counts.Asks, &out.Counts.Warnings, &out.Counts.OtherOpen, &out.Counts.ProbablyDone, &out.Counts.ClosedOrGone}
	total := int64(0)
	w, code := env.appReadAsks(ctx, func(index []state.AskIndex) []int64 {
		total = int64(len(index))
		bySection := make([][]int64, len(appSections))
		for _, v := range index { // newest raised first
			i := appIndexSection(v, now)
			*counts[i]++
			if v.State == state.AskOpen {
				out.Counts.Open++
			}
			bySection[i] = append(bySection[i], v.ID)
		}
		ids := []int64{}
		for _, section := range bySection {
			for _, id := range section {
				if len(ids) == appAskRowsMax {
					return ids
				}
				ids = append(ids, id)
			}
		}
		return ids
	})
	if code == appNotSetUp {
		return appResult{data: appAsks{Setup: "not_set_up", Rows: []appAskRow{}}}
	}
	if code != "" {
		return appFailure(code)
	}
	if w.ReportPresent {
		st := w.Status
		out.Watcher = appWatcher{Known: true, PassAt: appMillis(st.PassAt), ErrorAt: appMillis(st.ErrorAt), LastError: st.LastError, Watched: st.Watched, Waiting: st.Waiting}
	}
	targets := env.targets(ctx)
	size, err := appJSONSize(out)
	if err != nil {
		return appFailure(appInternal)
	}
	budget := appMaxBytes - appEnvelopeRoom
	for _, a := range w.Items {
		row := appAskRow{ID: appIDString(a.ID), Revision: state.AskActionRevision(a), State: string(a.State), Section: appSections[appSection(a, now)],
			Provider: a.Provider, ConversationID: a.SessionID, Workspace: a.Workspace, RaisedAt: appMillis(a.RaisedAt), StateAt: appMillis(a.StateAt),
			StateReason: a.StateReason, Repeats: a.Repeats, First: appPreview(a.FirstQuote), Latest: appPreview(a.LastQuote),
			Session: appSessionName{Source: "none"}}
		row.Terminal, row.TerminalReason = targets.forAsk(a)
		n, err := appJSONSize(row)
		if err != nil {
			return appFailure(appInternal)
		}
		if size+n+1 > budget {
			break // rows stay a prefix of the order
		}
		size += n + 1
		out.Rows = append(out.Rows, row)
	}
	r := appResult{data: out}
	if omitted := total - int64(len(out.Rows)); omitted > 0 {
		r.partial, r.omitted = true, map[string]int64{"rows": omitted}
	}
	return r
}

type appAskDetail struct {
	ID             string  `json:"id"`
	Revision       string  `json:"revision"`
	State          string  `json:"state"`
	Provider       string  `json:"provider"`
	ConversationID string  `json:"conversation_id"`
	Workspace      string  `json:"workspace"`
	First          appText `json:"first"`
	Latest         appText `json:"latest"`
}

// askDetail is one ask's full question text from the asks store alone: no
// transcript is read, so selecting an ask stays cheap.
func (env appEnv) askDetail(ctx context.Context, id string) appResult {
	w, code := env.appReadAsks(ctx, appOne(id))
	if code != "" {
		return appFailure(code)
	}
	for _, a := range w.Items {
		if appIDString(a.ID) == id {
			return appResult{data: appAskDetail{ID: id, Revision: state.AskActionRevision(a), State: string(a.State), Provider: a.Provider,
				ConversationID: a.SessionID, Workspace: a.Workspace, First: appFull(a.FirstQuote), Latest: appFull(a.LastQuote)}}
		}
	}
	return appFailure(appNotFound)
}

type appActionResult struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Revision string `json:"revision"`
}

// askAction dismisses or restores one ask only if it is still at revision.
// conflict means it changed (a replay of an applied action included). Any
// other failure may have happened after the write: unknown_outcome, which the
// app resolves with one read, never a retry.
func (env appEnv) askAction(ctx context.Context, id, revision, action string) appResult {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return appFailure(appInvalid)
	}
	if _, err := os.Lstat(env.asksPath); os.IsNotExist(err) {
		return appFailure(appNotSetUp)
	}
	to := state.AskDismissed
	if action == "restore" {
		to = state.AskOpen
	}
	store, err := state.OpenHook(env.asksPath)
	if err != nil {
		return appFailure(appUnavailable)
	}
	defer store.Close()
	item, err := store.SetAskByPersonRevision(ctx, n, revision, to, env.now().Unix(), navProductionPolicy().asks.reader.TextBytes)
	if errors.Is(err, state.ErrAskActionChanged) {
		return appFailure(appConflict)
	}
	if err != nil {
		return appFailure(appUnknownOutcome)
	}
	return appResult{data: appActionResult{ID: appIDString(item.ID), State: string(item.State), Revision: state.AskActionRevision(item)}}
}
