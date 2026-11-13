package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
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
}
type navAsksKnownBody struct {
	Scope    navAsksScope       `json:"scope"`
	Sections []navAsksSection   `json:"sections"`
	Report   navAsksKnownReport `json:"report"`
	Totals   navAsksTotals      `json:"totals"`
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
	seeds    []navAsksResponseSeed
}

func captureNavAsksResponse(s state.AsksSnapshot, o navAsksResponseOptions, c *navAsksCredits) (*navAsksResponseCapture, error) {
	c.require(navAsksSafeCount(s.Skipped) && navAsksSafeCount(s.CallsToday) &&
		int64(s.Status.Skipped) == s.Skipped && int64(s.Status.CallsToday) == s.CallsToday && s.UTCDay == state.AsksDay(o.now))
	if uint64(len(s.Items)) > o.rows {
		return nil, errNavAsksResponseTooLarge
	}
	c.raw(o.scope.Mode, true)
	c.raw(o.scope.Dir, true)
	c.raw(s.UTCDay, true)
	seen, allowed := make(map[int64]bool), make(map[string]bool)
	allowed[o.scope.Dir] = true
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
			return nil, c.err
		}
		if a.Workspace != "" {
			allowed[a.Workspace] = true
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
	c.require(uint64(len(o.aliases)) <= uint64(len(s.Items))+1 && (o.scope.Mode != "all" || len(o.aliases) == 0))
	for key, value := range o.aliases {
		c.require(key != "" && value != "" && allowed[key])
		c.raw(key, true)
		c.raw(value, true)
	}
	if c.err != nil {
		return nil, c.err
	}
	aliases := make(map[string]string, len(o.aliases))
	for key, value := range o.aliases {
		aliases[strings.Clone(key)] = strings.Clone(value)
	}
	out := &navAsksResponseCapture{scope: navAsksScope{c.text(o.scope.Mode), c.text(o.scope.Dir)},
		sections: make([]navAsksSection, 5), known: s.ReportPresent, totals: navAsksTotals{c.text(s.UTCDay), s.Skipped, s.CallsToday}}
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
		out.seeds = append(out.seeds, navAsksResponseSeed{group: group, index: len(out.sections[group].Items), raw: [2]string{a.FirstQuote, a.LastQuote}})
		out.sections[group].Items = append(out.sections[group].Items, item)
	}
	if c.err != nil {
		return nil, c.err
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
	faults := o.faults
	if faults == nil {
		faults = &diagnostic.Latch{}
	}
	defer func() {
		if value := recover(); value != nil {
			faults.Note(diagnostic.Capture(value))
			body, err = nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}()
	if ctx == nil || render == nil || marshal == nil || o.width < 1 || o.rows == 0 || o.inputBytes == 0 || o.renderCells == 0 || o.arrayEntries == 0 || o.stringUnits == 0 || o.bodyBytes <= 0 ||
		!(o.scope.Mode == "all" && o.scope.Dir == "" || o.scope.Mode == "directory" && o.scope.Dir != "" && filepath.IsAbs(o.scope.Dir)) {
		return nil, errNavAsksResponseInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const seconds = navConversationSafeInteger / 1000
	if o.now.Before(time.Unix(-seconds, 0)) || !o.now.Before(time.Unix(seconds, 0).Add(time.Second)) {
		return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
	}
	c := &navAsksCredits{ctx: ctx, input: o.inputBytes, work: o.renderCells, entries: o.arrayEntries, units: o.stringUnits}
	data, err := captureNavAsksResponse(s, o, c)
	if err != nil {
		return nil, navAsksResponseError(err, faults)
	}
	s = state.AsksSnapshot{}
	o.aliases = nil // No caller graph is consulted after capture.
	for _, seed := range data.seeds {
		pair, err := navAsksRenderAdmitted(ctx, seed.admitted, &c.entries, &c.units, faults, render, navAsksWordingValue)
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
		payload = navAsksKnownBody{data.scope, data.sections, data.report, data.totals}
	} else {
		payload = navAsksUnknownBody{data.scope, data.sections, navAsksUnknownReport{Known: false}, data.totals}
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
