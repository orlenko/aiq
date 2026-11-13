package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

var (
	errNavAsksResponseInvalid  = errors.New("invalid asks response request")
	errNavAsksResponseTooLarge = errors.New("asks response too large")
)

type navAsksScope struct {
	Mode string `json:"mode"`
	Dir  string `json:"dir"`
}
type navAsksTotals struct {
	Day     string `json:"utc_day"`
	Skipped int64  `json:"skipped"`
	Calls   int64  `json:"calls_today"`
}
type navAsksMoved struct {
	Day   string      `json:"local_day"`
	Start navWireTime `json:"start_at"`
	IDs   []string    `json:"ids"`
}
type navAsksItem struct {
	ID        string         `json:"id"`
	Provider  string         `json:"provider"`
	SessionID string         `json:"session_id"`
	Workspace string         `json:"workspace"`
	State     string         `json:"state"`
	Raised    navWireTime    `json:"raised_at"`
	StateAt   navWireTime    `json:"state_at"`
	Repeats   int64          `json:"repeats"`
	Reason    string         `json:"state_reason"`
	Text      string         `json:"state_text"`
	Refs      []string       `json:"refs"`
	First     navAsksWording `json:"first"`
	Latest    navAsksWording `json:"latest"`
}
type navAsksSection struct {
	Key   string        `json:"key"`
	Items []navAsksItem `json:"items"`
}
type navAsksUnknownReport struct {
	Known bool `json:"known"`
}
type navAsksKnownReport struct {
	Known      bool        `json:"known"`
	Pass       navWireTime `json:"pass_at"`
	OK         navWireTime `json:"ok_at"`
	Error      navWireTime `json:"error_at"`
	Next       navWireTime `json:"next_try_at"`
	Watched    int64       `json:"watched"`
	Waiting    int64       `json:"waiting"`
	Skipped    int64       `json:"skipped"`
	Calls      int64       `json:"calls_today"`
	LastError  string      `json:"last_error"`
	NotWatched []string    `json:"not_watched"`
}

// Two concrete closed wire shapes; neither contains an interface or pointer.
type navAsksUnknownBody struct {
	Scope    navAsksScope         `json:"scope"`
	Sections []navAsksSection     `json:"sections"`
	Report   navAsksUnknownReport `json:"report"`
	Totals   navAsksTotals        `json:"totals"`
	Moved    navAsksMoved         `json:"moved_today"`
}
type navAsksKnownBody struct {
	Scope    navAsksScope       `json:"scope"`
	Sections []navAsksSection   `json:"sections"`
	Report   navAsksKnownReport `json:"report"`
	Totals   navAsksTotals      `json:"totals"`
	Moved    navAsksMoved       `json:"moved_today"`
}
type navAsksResponseOptions struct {
	scope                                                    navAsksScope
	now                                                      time.Time
	aliases                                                  map[string]string // Supplied completed facts; no resolver in this component.
	width                                                    int
	rows, inputBytes, renderCells, arrayEntries, stringUnits uint64
	bodyBytes                                                int64
	faults                                                   *diagnostic.Latch // One serial operation.
}
type navAsksCredits struct {
	input, work, entries, units uint64
	err                         error
	ctx                         context.Context
}

func (c *navAsksCredits) require(ok bool) {
	if !ok && c.err == nil {
		c.err = diagnostic.Base(diagnostic.Unshowable)
	}
}
func (c *navAsksCredits) raw(s string, metadata bool) {
	if c.err != nil {
		return
	}
	if err := c.ctx.Err(); err != nil {
		c.err = err
		return
	}
	if !navAsksTake(&c.input, uint64(len(s))) {
		c.err = errNavAsksResponseTooLarge
		return
	}
	c.require(!metadata || utf8.ValidString(s))
}
func (c *navAsksCredits) array(n int) {
	if c.err == nil && !navAsksTake(&c.entries, uint64(n)) {
		c.err = errNavAsksResponseTooLarge
	}
}
func (c *navAsksCredits) text(s string) string {
	if c.err != nil {
		return ""
	}
	if !navAsksText(&c.units, s) {
		c.err = errNavAsksResponseTooLarge
		return ""
	}
	return strings.Clone(s)
}
func (c *navAsksCredits) list(in []string) []string {
	c.array(len(in))
	if c.err != nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, c.text(s))
		if c.err != nil {
			return nil
		}
	}
	return out
}
func navAsksSafeCount(n int64) bool { return n >= 0 && n <= navConversationSafeInteger }
func navAsksSafeSeconds(n int64) bool {
	return n >= -navConversationSafeInteger/1000 && n <= navConversationSafeInteger/1000
}
func navAsksSecond(n int64, sentinel bool) navWireTime {
	if sentinel && n == 0 {
		return navWireTime{}
	}
	return navWireTime{MS: n * 1000, Known: true} // Checked before exclusion and conversion.
}

type navAsksResponseSeed struct {
	group, index int
	raw          [2]string
	admitted     navAsksAdmittedWording
}
type navAsksResponseCapture struct {
	scope    navAsksScope
	sections []navAsksSection
	report   navAsksKnownReport
	known    bool
	totals   navAsksTotals
	moved    navAsksMoved
	seeds    []navAsksResponseSeed
}

func prepareNavAsksRaw(s state.AsksSnapshot, o navAsksResponseOptions, c *navAsksCredits) (state.AsksSnapshot, error) {
	c.require(navAsksSafeCount(s.Skipped) && navAsksSafeCount(s.CallsToday) &&
		int64(s.Status.Skipped) == s.Skipped && int64(s.Status.CallsToday) == s.CallsToday && s.UTCDay == state.AsksDay(o.now))
	if uint64(len(s.Items)) > o.rows {
		return state.AsksSnapshot{}, errNavAsksResponseTooLarge
	}
	structural := o.arrayEntries
	for _, a := range s.Items {
		if !navAsksTake(&structural, uint64(len(a.Refs))) {
			return state.AsksSnapshot{}, errNavAsksResponseTooLarge
		}
	}
	if s.ReportPresent && !navAsksTake(&structural, uint64(len(s.Status.NotWatched))) {
		return state.AsksSnapshot{}, errNavAsksResponseTooLarge
	}
	c.raw(o.scope.Mode, true)
	c.raw(o.scope.Dir, true)
	c.raw(s.UTCDay, true)
	seen := make(map[int64]bool)
	for _, a := range s.Items {
		c.require(a.ID > 0 && !seen[a.ID] && navConversationWord(string(a.State), "open", "probably-done", "closed", "dismissed") &&
			navAsksSafeCount(int64(a.Repeats)) && navAsksSafeSeconds(a.RaisedAt) && navAsksSafeSeconds(a.StateAt) &&
			!math.IsNaN(a.Risk) && !math.IsInf(a.Risk, 0) && !math.IsNaN(a.AsksReader) && !math.IsInf(a.AsksReader, 0))
		seen[a.ID] = true
		for _, text := range []string{a.Chain, a.Provider, a.SessionID, a.Workspace, string(a.State), a.StateReason, a.StateText} {
			c.raw(text, true)
		}
		for _, text := range a.Refs {
			c.raw(text, true)
		}
		c.raw(a.FirstQuote, false)
		c.raw(a.LastQuote, false)
		if c.err != nil {
			return state.AsksSnapshot{}, c.err
		}
	}
	if s.ReportPresent {
		st := s.Status
		c.require(navAsksSafeCount(int64(st.Watched)) && navAsksSafeCount(int64(st.Waiting)))
		for _, at := range []int64{st.PassAt, st.OkAt, st.ErrorAt, st.NextTryAt} {
			c.require(navAsksSafeSeconds(at))
		}
		c.raw(st.LastError, true)
		for _, text := range st.NotWatched {
			c.raw(text, true)
		}
	}
	if c.err != nil {
		return state.AsksSnapshot{}, c.err
	}
	owned := state.AsksSnapshot{Items: make([]state.Ask, len(s.Items)), ReportPresent: s.ReportPresent,
		Skipped: s.Skipped, CallsToday: s.CallsToday, UTCDay: strings.Clone(s.UTCDay)}
	owned.Status.Skipped, owned.Status.CallsToday = s.Status.Skipped, s.Status.CallsToday
	cloneList := func(in []string) []string {
		out := make([]string, len(in))
		for i, v := range in {
			out[i] = strings.Clone(v)
		}
		return out
	}
	if s.ReportPresent {
		owned.Status = s.Status
		owned.Status.LastError = strings.Clone(s.Status.LastError)
		owned.Status.NotWatched = cloneList(s.Status.NotWatched)
	}
	for i, a := range s.Items {
		a.Chain, a.Workspace, a.Provider, a.SessionID = strings.Clone(a.Chain), strings.Clone(a.Workspace), strings.Clone(a.Provider), strings.Clone(a.SessionID)
		a.State = state.AskState(strings.Clone(string(a.State)))
		a.StateReason, a.StateText = strings.Clone(a.StateReason), strings.Clone(a.StateText)
		a.FirstQuote, a.LastQuote = strings.Clone(a.FirstQuote), strings.Clone(a.LastQuote)
		a.Refs = cloneList(a.Refs)
		owned.Items[i] = a
	}
	return owned, nil
}

// First-origin provenance and a new fault in this run are independent facts.
type navAsksFaultState struct {
	latch   *diagnostic.Latch
	current bool
}

func (f *navAsksFaultState) external(err error) {
	origin := diagnostic.FromError(err)
	f.latch.Note(origin)
	if origin.Present() {
		f.current = true
	}
}

type navAsksRun struct {
	o        navAsksResponseOptions
	c        *navAsksCredits
	raw      state.AsksSnapshot
	faults   *navAsksFaultState
	aliases  map[string]string
	allowed  map[string]bool
	pending  string
	used     bool
	dayStart int64
}

func newNavAsksRun(ctx context.Context, s state.AsksSnapshot, o navAsksResponseOptions, f *navAsksFaultState) (*navAsksRun, error) {
	if ctx == nil || f == nil || f.latch == nil || o.width < 1 || o.rows == 0 || o.inputBytes == 0 || o.renderCells == 0 || o.arrayEntries == 0 || o.stringUnits == 0 || o.bodyBytes <= 0 ||
		!(o.scope.Mode == "all" && o.scope.Dir == "" || o.scope.Mode == "directory" && o.scope.Dir != "" && filepath.IsAbs(o.scope.Dir)) {
		return nil, errNavAsksResponseInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const seconds = navConversationSafeInteger / 1000
	if o.now.Before(time.Unix(-seconds, 0)) || !o.now.Before(time.Unix(seconds, 0).Add(time.Second)) {
		return nil, diagnostic.Base(diagnostic.Unshowable)
	}
	start := asksDayStart(o.now) // Same producer-local boundary as the CLI.
	if !navAsksSafeSeconds(start) {
		return nil, diagnostic.Base(diagnostic.Unshowable)
	}
	c := &navAsksCredits{ctx: ctx, input: o.inputBytes, work: o.renderCells, entries: o.arrayEntries, units: o.stringUnits}
	raw, err := prepareNavAsksRaw(s, o, c)
	if err != nil {
		return nil, err
	}
	o.scope = navAsksScope{strings.Clone(o.scope.Mode), strings.Clone(o.scope.Dir)}
	o.aliases = nil
	r := &navAsksRun{o: o, c: c, raw: raw, faults: f, dayStart: start, aliases: make(map[string]string), allowed: map[string]bool{o.scope.Dir: true}}
	for _, a := range raw.Items {
		if a.Workspace != "" {
			r.allowed[a.Workspace] = true
		}
	}
	return r, nil
}
func (r *navAsksRun) reserveAlias(key string) error {
	if r.used || r.pending != "" || r.o.scope.Mode == "all" || key == "" || !r.allowed[key] || r.aliases[key] != "" || uint64(len(r.aliases)) >= uint64(len(r.raw.Items))+1 {
		return diagnostic.Base(diagnostic.Unshowable)
	}
	r.c.raw(key, true)
	if r.c.err != nil {
		return r.c.err
	}
	r.pending = key
	return nil
}
func (r *navAsksRun) commitAlias(key, value string) error {
	if r.used || r.pending != key || value == "" {
		return diagnostic.Base(diagnostic.Unshowable)
	}
	r.c.raw(value, true)
	if r.c.err != nil {
		return r.c.err
	}
	r.aliases[strings.Clone(key)] = strings.Clone(value)
	r.pending = ""
	return nil
}
func (r *navAsksRun) captureSelected() (*navAsksResponseCapture, error) {
	s, o, c, aliases := r.raw, r.o, r.c, r.aliases
	out := &navAsksResponseCapture{scope: navAsksScope{c.text(o.scope.Mode), c.text(o.scope.Dir)},
		sections: make([]navAsksSection, 5), known: s.ReportPresent, totals: navAsksTotals{c.text(s.UTCDay), s.Skipped, s.CallsToday},
		moved: navAsksMoved{Day: c.text(o.now.Format("2006-01-02")), Start: navAsksSecond(r.dayStart, false), IDs: make([]string, 0)}}
	type moveRef struct {
		at, id int64
		text   string
	}
	var moved []moveRef
	c.array(5)
	for i, key := range []string{"asks", "warnings", "other_open", "probably_done", "closed_dismissed"} {
		out.sections[i] = navAsksSection{Key: c.text(key), Items: make([]navAsksItem, 0)}
	}
	if s.ReportPresent {
		st := s.Status
		out.report = navAsksKnownReport{Known: true, Pass: navAsksSecond(st.PassAt, true), OK: navAsksSecond(st.OkAt, true),
			Error: navAsksSecond(st.ErrorAt, true), Next: navAsksSecond(st.NextTryAt, true), Watched: int64(st.Watched), Waiting: int64(st.Waiting),
			Skipped: s.Skipped, Calls: s.CallsToday, LastError: c.text(st.LastError), NotWatched: c.list(st.NotWatched)}
	}
	for _, a := range s.Items {
		selected := true
		if o.scope.Mode == "directory" {
			selected = asksInDir(a.Workspace, o.scope.Dir, func(p string) string { return p })
			if !selected && a.Workspace != "" {
				_, wsOK := aliases[a.Workspace]
				_, dirOK := aliases[o.scope.Dir]
				c.require(wsOK && dirOK)
				if c.err != nil {
					return nil, c.err
				}
				selected = asksInDir(a.Workspace, o.scope.Dir, func(p string) string { return aliases[p] })
			}
		}
		if !selected {
			continue
		}
		group := 4
		switch a.State {
		case state.AskProbablyDone:
			group = 3
		case state.AskOpen:
			group = 2
			switch asksGroup(a, o.now.Unix()) {
			case "Asks":
				group = 0
			case "Warnings":
				group = 1
			}
		}
		c.array(1)
		item := navAsksItem{ID: c.text(strconv.FormatInt(a.ID, 10)), Provider: c.text(a.Provider), SessionID: c.text(a.SessionID),
			Workspace: c.text(a.Workspace), State: c.text(string(a.State)), Raised: navAsksSecond(a.RaisedAt, false), StateAt: navAsksSecond(a.StateAt, false),
			Repeats: int64(a.Repeats), Reason: c.text(a.StateReason), Text: c.text(a.StateText), Refs: c.list(a.Refs)}
		if c.err != nil {
			return nil, c.err
		}
		if (a.State == state.AskClosed || a.State == state.AskProbablyDone) && a.StateReason != state.AskPerson && a.StateAt >= r.dayStart {
			c.array(1)
			text := c.text(item.ID) // A second wire occurrence, on the same ledger.
			if c.err != nil {
				return nil, c.err
			}
			moved = append(moved, moveRef{a.StateAt, a.ID, text})
		}
		out.seeds = append(out.seeds, navAsksResponseSeed{group: group, index: len(out.sections[group].Items), raw: [2]string{a.FirstQuote, a.LastQuote}})
		out.sections[group].Items = append(out.sections[group].Items, item)
	}
	if c.err != nil {
		return nil, c.err
	}
	sort.Slice(moved, func(i, j int) bool {
		if moved[i].at != moved[j].at {
			return moved[i].at > moved[j].at
		}
		return moved[i].id > moved[j].id
	})
	for _, m := range moved {
		out.moved.IDs = append(out.moved.IDs, m.text)
	}
	// Every selected quote is frozen and admitted before any renderer callout.
	for i := range out.seeds {
		admitted, err := navAsksAdmitWording(out.seeds[i].raw, max(o.width, 10), &c.work)
		if err != nil {
			return nil, err
		}
		out.seeds[i].admitted = admitted
		out.seeds[i].raw = [2]string{}
	}
	return out, nil
}

func buildNavAsksResponse(ctx context.Context, s state.AsksSnapshot, o navAsksResponseOptions) ([]byte, error) {
	return buildNavAsksResponseWith(ctx, s, o, webMarkdownLinesChecked, json.Marshal)
}
func buildNavAsksResponseWith(ctx context.Context, s state.AsksSnapshot, o navAsksResponseOptions,
	render navAsksWordingRender, marshal func(any) ([]byte, error)) (body []byte, err error) {
	latch := o.faults
	if latch == nil {
		latch = &diagnostic.Latch{}
	}
	f := &navAsksFaultState{latch: latch}
	defer func() {
		if value := recover(); value != nil {
			origin := diagnostic.Capture(value)
			f.current = true
			latch.Note(origin)
			body, err = nil, diagnostic.Failure(diagnostic.Unshowable, latch.Origin())
		}
	}()
	if render == nil || marshal == nil {
		return nil, errNavAsksResponseInvalid
	}
	r, err := newNavAsksRun(ctx, s, o, f)
	if err != nil {
		if err == errNavAsksResponseInvalid {
			return nil, err
		}
		return nil, navAsksResponseError(err, latch)
	}
	for key, value := range o.aliases {
		if err = r.reserveAlias(key); err != nil {
			return nil, navAsksResponseError(err, latch)
		}
		if err = r.commitAlias(key, value); err != nil {
			return nil, navAsksResponseError(err, latch)
		}
	}
	return r.finish(render, marshal)
}
func (r *navAsksRun) finish(render navAsksWordingRender, marshal func(any) ([]byte, error)) (body []byte, err error) {
	ctx, o, c, faults := r.c.ctx, r.o, r.c, r.faults.latch
	defer func() {
		if value := recover(); value != nil {
			origin := diagnostic.Capture(value)
			r.faults.current = true
			faults.Note(origin)
			body, err = nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}()
	if r.used {
		return nil, errNavAsksResponseInvalid
	}
	r.used = true
	if r.pending != "" || render == nil || marshal == nil {
		return nil, errNavAsksResponseInvalid
	}
	data, err := r.captureSelected()
	if err != nil {
		return nil, navAsksResponseError(err, faults)
	}
	for _, seed := range data.seeds {
		pair, err := navAsksRenderAdmitted(ctx, seed.admitted, &c.entries, &c.units, faults, &r.faults.current, render, navAsksWordingValue)
		if err != nil {
			return nil, navAsksResponseError(err, faults)
		} // Note closed origins BEFORE context/rebasing.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := &data.sections[seed.group].Items[seed.index]
		item.First, item.Latest = pair.First, pair.Latest
	}
	var payload any // Always one of these two concrete, closed values.
	if data.known {
		payload = navAsksKnownBody{data.scope, data.sections, data.report, data.totals, data.moved}
	} else {
		payload = navAsksUnknownBody{data.scope, data.sections, navAsksUnknownReport{Known: false}, data.totals, data.moved}
	}
	size, fits := navJSONSize(payload, math.MaxInt64)
	if !fits {
		return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
	}
	if size > o.bodyBytes {
		return nil, errNavAsksResponseTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err = marshal(payload)
	if err != nil {
		r.faults.external(err)
		return nil, navAsksResponseError(err, faults)
	}
	if int64(len(body)) != size {
		return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}
func navAsksResponseError(err error, faults *diagnostic.Latch) error {
	faults.Note(diagnostic.FromError(err))
	if err == errNavAsksWordingTooLarge || err == errNavAsksResponseTooLarge {
		return errNavAsksResponseTooLarge
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
}
