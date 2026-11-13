package main

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

var (
	errNavAsksWordingInvalid  = errors.New("invalid asks wording request")
	errNavAsksWordingTooLarge = errors.New("asks wording too large")
)

type navAsksWording struct {
	Lines []navConversationLine `json:"lines"`
	Copy  string                `json:"copy"`
}

type navAsksWordingPair struct {
	First  navAsksWording `json:"first"`
	Latest navAsksWording `json:"latest"`
}

type navAsksWordingOptions struct {
	width                                              int
	inputBytes, renderCells, arrayEntries, stringUnits uint64
	faults                                             *diagnostic.Latch // One serial operation only.
}

type navAsksWordingDraft struct {
	lines []webMarkdownLine
	copy  string
}

type navAsksWordingRender func(string, int, bool) ([]webMarkdownLine, bool)
type navAsksWordingConvert func([2]navAsksWordingDraft) *navAsksWordingPair

// Private proof of selected-pair work admission; raw input is charged by the
// standalone wrapper or the whole-response capture before creating these.
type navAsksAdmittedWording struct {
	raw   [2]string
	width int
}

func navAsksAdmitWording(raw [2]string, width int, work *uint64) (navAsksAdmittedWording, error) {
	if width < 10 || work == nil {
		return navAsksAdmittedWording{}, errNavAsksWordingInvalid
	}
	for _, text := range raw {
		c := uint64(len(text)) + 1
		if c == 0 || !navAsksProduct(work, c, c) || !navAsksProduct(work, c, uint64(width)) {
			return navAsksAdmittedWording{}, errNavAsksWordingTooLarge
		}
	}
	return navAsksAdmittedWording{raw: raw, width: width}, nil
}

// Pure paired projection. Callers supply limits and a coherent Ask value;
// readers, production policy and registered route diagnostics are separate.
func projectNavAsksWording(ctx context.Context, ask state.Ask, o navAsksWordingOptions) (*navAsksWordingPair, error) {
	return projectNavAsksWordingWith(ctx, ask, o, webMarkdownLinesChecked, navAsksWordingValue)
}

func projectNavAsksWordingWith(ctx context.Context, ask state.Ask, o navAsksWordingOptions,
	render navAsksWordingRender, convert navAsksWordingConvert) (pair *navAsksWordingPair, err error) {
	faults := o.faults
	if faults == nil {
		faults = &diagnostic.Latch{}
	}
	defer func() {
		if value := recover(); value != nil {
			faults.Note(diagnostic.Capture(value))
			pair, err = nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}()
	if ctx == nil || render == nil || convert == nil || o.width < 1 || o.inputBytes == 0 ||
		o.renderCells == 0 || o.arrayEntries == 0 || o.stringUnits == 0 {
		return nil, errNavAsksWordingInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := [2]string{ask.FirstQuote, ask.LastQuote}
	bytes, work := o.inputBytes, o.renderCells
	for _, text := range raw {
		if !navAsksTake(&bytes, uint64(len(text))) {
			return nil, errNavAsksWordingTooLarge
		}
	}
	admitted, err := navAsksAdmitWording(raw, max(o.width, 10), &work)
	if err != nil {
		return nil, err
	}
	return navAsksRenderAdmitted(ctx, admitted, &o.arrayEntries, &o.stringUnits, faults, nil, render, convert)
}

// Internal zero remaining credits mean exhaustion, not invalid configuration.
func navAsksRenderAdmitted(ctx context.Context, admitted navAsksAdmittedWording, entries, units *uint64,
	faults *diagnostic.Latch, observed *bool, render navAsksWordingRender, convert navAsksWordingConvert) (pair *navAsksWordingPair, err error) {
	defer func() {
		if value := recover(); value != nil {
			origin := diagnostic.Capture(value)
			if observed != nil {
				*observed = true
			}
			faults.Note(origin)
			pair, err = nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}()
	if ctx == nil || admitted.width < 10 || entries == nil || units == nil || faults == nil || render == nil || convert == nil {
		return nil, errNavAsksWordingInvalid
	}
	var draft [2]navAsksWordingDraft
	for i, text := range admitted.raw {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		paint := asksQuotePaintText(text, i == 1)
		draft[i].copy = quoteForReply(sanitize(text))
		covered := true
		if paint != "" {
			draft[i].lines, covered = render(paint, admitted.width, false)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !covered {
			return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
		}
	}
	for _, value := range draft {
		if err := navAsksWordingCharge(value, entries, units); err != nil {
			if err == diagnostic.Base(diagnostic.Unshowable) {
				return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
			}
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pair = convert(draft)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pair == nil {
		return nil, diagnostic.Failure(diagnostic.Unshowable, faults.Origin())
	}
	return pair, nil
}

func navAsksTake(remaining *uint64, n uint64) bool {
	if n > *remaining {
		return false
	}
	*remaining -= n
	return true
}

func navAsksProduct(remaining *uint64, a, b uint64) bool {
	if b == 0 || a > *remaining/b {
		return false
	}
	*remaining -= a * b
	return true
}

func navAsksText(remaining *uint64, text string) bool {
	for _, r := range text {
		n := uint64(1)
		if r > 0xffff {
			n = 2
		}
		if !navAsksTake(remaining, n) {
			return false
		}
	}
	return true
}

// Validate and charge even discarded whitespace tails before owned cloning.
// Markdown/copy intermediates already exist: this is not their allocation bound.
func navAsksWordingCharge(v navAsksWordingDraft, entries, units *uint64) error {
	bad := diagnostic.Base(diagnostic.Unshowable)
	if !utf8.ValidString(v.copy) {
		return bad
	}
	if !navAsksText(units, v.copy) || !navAsksTake(entries, uint64(len(v.lines))) {
		return errNavAsksWordingTooLarge
	}
	for _, line := range v.lines {
		if !navConversationWord(line.kind, "text", "table", "rule", "quote", "code") ||
			!navConversationNumber(line.tableRows) || line.kind != "table" && line.tableRows != 0 {
			return bad
		}
		if !navAsksText(units, line.kind) || !navAsksTake(entries, uint64(len(line.spans))) {
			return errNavAsksWordingTooLarge
		}
		for _, span := range line.spans {
			if !utf8.ValidString(span.text) {
				return bad
			}
			if !navAsksText(units, span.text) || !navAsksTake(entries, uint64(len(span.classes))) {
				return errNavAsksWordingTooLarge
			}
			for _, class := range span.classes {
				if !navConversationWord(class, "b", "dim", "i", "u", "inverse", "band", "you", "agent", "muted", "accent", "code") {
					return bad
				}
				if !navAsksText(units, class) {
					return errNavAsksWordingTooLarge
				}
			}
		}
	}
	return nil
}

func navAsksWordingValue(in [2]navAsksWordingDraft) *navAsksWordingPair {
	var out [2]navAsksWording
	for i, value := range in {
		end := len(value.lines)
		for end > 0 && navAsksBlankLine(value.lines[end-1]) {
			end--
		}
		out[i] = navAsksWording{Copy: strings.Clone(value.copy), Lines: make([]navConversationLine, 0, end)}
		for _, line := range value.lines[:end] {
			row := navConversationLine{Kind: strings.Clone(line.kind), TableRows: line.tableRows,
				Spans: make([]navConversationSpan, 0, len(line.spans))}
			for _, span := range line.spans {
				classes, _ := navConversationClasses(span.classes) // Already validated and charged.
				row.Spans = append(row.Spans, navConversationSpan{Classes: classes,
					Decorative: span.decorative, Text: strings.Clone(span.text)})
			}
			out[i].Lines = append(out[i].Lines, row)
		}
	}
	return &navAsksWordingPair{First: out[0], Latest: out[1]}
}

func navAsksBlankLine(line webMarkdownLine) bool {
	for _, span := range line.spans {
		for _, r := range span.text {
			if !unicode.IsSpace(r) {
				return false
			}
		}
	}
	return true
}
