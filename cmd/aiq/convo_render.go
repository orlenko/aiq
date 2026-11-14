package main

import (
	"os"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

type convoMarkdownRenderer func(string, int, string, bool) string

// convoRenderContext supplies trusted display inputs. Its renderer must use
// the supplied styles without consulting mutable process settings. A context
// may be reused by independent printers; each printer's emit state is its own.
// An empty palette/label or zero now is deliberate, and nil location means UTC.
type convoRenderContext struct {
	palette     convoPalette
	markup      bool
	width       int
	liveMark    string
	itemBand    string
	skippedNote string
	now         time.Time
	location    *time.Location
	renderer    convoMarkdownRenderer
}

func newConvoPrinterWithContext(provider string, color, marks bool, ctx convoRenderContext) *convoPrinter {
	location := ctx.location
	if location == nil {
		location = time.UTC
	}
	return &convoPrinter{
		provider: provider, color: color, marks: marks,
		width: ctx.width, markup: ctx.markup, palette: ctx.palette,
		liveMark: ctx.liveMark, itemBand: ctx.itemBand, skippedNote: ctx.skippedNote,
		renderer: ctx.renderer, seen: map[string]*turnSeen{},
		promptHead: func(t time.Time) string {
			if t.IsZero() {
				return "▌ you"
			}
			return "▌ you · " + whenLabelIn(t, ctx.now, location)
		},
		clock: func(t time.Time) string { return clockLabelIn(t, location) },
	}
}

// newConvoPrinter is the CLI adapter. It resolves defaults once, while prompt
// dates remain live so a long-running follow can cross midnight.
func newConvoPrinter(provider string, color, marks bool) *convoPrinter {
	palette := pal
	renderer := markdownRenderer
	if renderer == nil {
		renderer = (markdownContext{palette: palette}).renderMarkdown
	}
	p := newConvoPrinterWithContext(provider, color, marks, convoRenderContext{
		palette: palette, markup: convoMarkup, renderer: renderer,
		liveMark: kitMarksFor(os.Getenv).live, itemBand: kitThemeFor(false).itemBand,
		skippedNote: transcript.SkippedNote(),
	})
	p.promptHead, p.clock = youHeader, clockLabel
	return p
}
