package main

import (
	"errors"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

type convoStyledOptions struct {
	width                 int
	markup                bool
	now                   time.Time
	location              *time.Location
	liveMark, skippedNote string
	start                 int
	live, hold            bool
	accounts              []convoAccountFact
}

// Private presentation, not a transport. Never serialize message.rawKey.
// Consumers must render groups separately, with base inheritance and the
// captured break/empty-body recipe; invisible groups contribute no paint.
type convoStyledMessage struct {
	message convoProjectionMessage
	groups  []convoStyledGroup
}

type convoStyledGroup struct {
	ordinal                              int
	head                                 convoHead
	base                                 []string
	lines                                []webMarkdownLine
	visible, leadingBreak, emptyBodyLine bool
}

type webLineRenderer func(string, int, bool) ([]webMarkdownLine, bool)

func renderConvoStyled(s *transcript.Session, opts convoStyledOptions) ([]convoStyledMessage, error) {
	return renderConvoStyledWith(s, opts, webMarkdownPalette(), webMarkdownLinesChecked)
}

// Explicit trusted dependencies permit fault/ownership tests. The ordinary
// entry always binds the fixed web palette and checked metadata producer.
func renderConvoStyledWith(s *transcript.Session, opts convoStyledOptions, palette convoPalette, render webLineRenderer) (out []convoStyledMessage, err error) {
	defer func() {
		if recover() != nil {
			out, err = nil, transcript.ErrUnshowable
		}
	}()
	capture := &convoPaintCapture{}
	ctx := convoRenderContext{
		palette: palette, width: opts.width, markup: opts.markup,
		now: opts.now, location: opts.location, liveMark: opts.liveMark,
		itemBand: palette.band, skippedNote: opts.skippedNote,
		renderer: func(text string, width int, base string, markup bool) string {
			capture.markdown(text, width, base, markup, render)
			return "" // offsets only; captured metadata owns the real body
		},
	}
	messages, err := renderConvoProjection(s, convoProjectionOptions{
		context: ctx, start: opts.start, live: opts.live, hold: opts.hold,
		accounts: opts.accounts, paintCapture: capture,
	})
	if err != nil {
		if errors.Is(err, errConvoProjectionRender) {
			return nil, transcript.ErrUnshowable
		}
		return nil, err
	}
	return capture.project(messages)
}

type convoPaintRecord struct {
	group convoStyledGroup
	kind  string
	facts convoFacts
}

// Each invocation owns its collector. No state is installed in reusable
// caller options, and ordinary printers leave their capture pointer nil.
type convoPaintCapture struct {
	records         []convoPaintRecord
	pending         convoStyledGroup
	markdownPending bool
	lastEnd         int
	failed          bool
}

func (c *convoPaintCapture) reset() { *c = convoPaintCapture{} }

func (c *convoPaintCapture) literal(style, text string) {
	if c == nil {
		return
	}
	if c.markdownPending {
		c.failed = true
	}
	classes, covered := webStyleClasses(style)
	if !covered {
		c.failed = true
	}
	// Match paint's final cleaning, after the emitter chose its branch and
	// did its own trimming/wrapping. In particular, do not trim here again.
	for _, row := range strings.Split(sanitize(text), "\n") {
		owned, ok := ownedWebClasses(classes)
		if !ok {
			c.failed = true
		}
		c.pending.lines = append(c.pending.lines, webMarkdownLine{kind: "text", spans: []webMarkdownSpan{{text: row, classes: owned}}})
	}
}

func (c *convoPaintCapture) markdown(text string, width int, base string, markup bool, render webLineRenderer) {
	if c.markdownPending || c.pending.lines != nil {
		c.failed = true
	}
	classes, coveredBase := webStyleClasses(base)
	lines, covered := render(text, width, markup)
	owned, valid := ownedWebLines(lines)
	if !coveredBase || !covered || !valid {
		c.failed = true
	}
	c.pending.base, c.pending.lines = classes, owned
	c.pending.emptyBodyLine = len(lines) == 0
	c.markdownPending = true
}

func (c *convoPaintCapture) cut(b *strings.Builder, cut convoCut, ordinal int) {
	defer func() { c.pending, c.markdownPending = convoStyledGroup{}, false }()
	if ordinal != len(c.records) || cut.end < c.lastEnd || cut.end > b.Len() {
		c.failed = true
		return
	}
	group := c.pending
	group.ordinal, group.head = ordinal, normalizedConvoFacts(cut.facts).head
	group.visible = cut.kind != "part"
	group.leadingBreak = cut.end > c.lastEnd && b.String()[c.lastEnd] == '\n'
	switch cut.kind {
	case "answer", "more":
		if !c.markdownPending {
			c.failed = true
		}
	case "part":
		if c.markdownPending || group.lines != nil || cut.end != c.lastEnd {
			c.failed = true
		}
	case "you", "steer", "notice", "line", "none", "skipped":
		if c.markdownPending || group.lines == nil {
			c.failed = true
		}
	default:
		c.failed = true
	}
	c.lastEnd = cut.end
	c.records = append(c.records, convoPaintRecord{group: group, kind: cut.kind, facts: normalizedConvoFacts(cut.facts)})
}

func (c *convoPaintCapture) project(messages []convoProjectionMessage) ([]convoStyledMessage, error) {
	if c.failed || c.markdownPending || c.pending.lines != nil {
		return nil, transcript.ErrUnshowable
	}
	var out []convoStyledMessage
	ordinal := 0
	for _, message := range messages {
		m := convoStyledMessage{message: message, groups: make([]convoStyledGroup, 0, len(message.parts))}
		for _, part := range message.parts {
			if ordinal >= len(c.records) {
				return nil, transcript.ErrUnshowable
			}
			record := c.records[ordinal]
			if record.group.ordinal != ordinal || record.kind != part.kind || record.facts != part.convoFacts {
				return nil, transcript.ErrUnshowable
			}
			m.groups = append(m.groups, record.group)
			ordinal++
		}
		out = append(out, m)
	}
	if ordinal != len(c.records) {
		return nil, transcript.ErrUnshowable
	}
	return out, nil
}

func ownedWebClasses(classes []string) ([]string, bool) {
	if classes == nil {
		return nil, true
	}
	out := make([]string, len(classes))
	valid := true
	for i, class := range classes {
		switch class {
		case "b", "dim", "i", "u", "inverse", "band", "you", "agent", "muted", "accent", "code":
		default:
			valid = false
		}
		out[i] = class
	}
	return out, valid
}

func ownedWebLines(lines []webMarkdownLine) ([]webMarkdownLine, bool) {
	if lines == nil {
		return nil, true
	}
	out := make([]webMarkdownLine, len(lines))
	valid := true
	for i, line := range lines {
		switch line.kind {
		case "text", "table", "rule", "quote", "code":
		default:
			valid = false
		}
		out[i] = webMarkdownLine{kind: line.kind, tableRows: line.tableRows}
		if line.spans == nil {
			continue
		}
		out[i].spans = make([]webMarkdownSpan, len(line.spans))
		for j, span := range line.spans {
			classes, ok := ownedWebClasses(span.classes)
			if !ok {
				valid = false
			}
			out[i].spans[j] = webMarkdownSpan{text: span.text, classes: classes, decorative: span.decorative}
		}
	}
	return out, valid
}
