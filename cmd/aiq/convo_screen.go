package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

// The conversation on a terminal: a full screen in the style of aiq asks, with one message
// selected. The printer draws each message as the stream and the pager showed it; the screen
// only cuts its output into messages, so how a message looks is the printer's alone.

// convoGutter is the cells left of each row, where the selection mark goes.
const convoGutter = 2

// convoStep is how many rows j/k, Up/Down and the wheel scroll through a message taller than
// the screen, before moving to the next one.
const convoStep = 3

// convoPart is one conversation the screen shows: the first, or one the pane moved to.
type convoPart struct {
	s     *transcript.Session
	t     convoTarget
	from  int       // the first turn shown (--last)
	moved time.Time // when the pane moved to it; zero for the first
	hold  bool      // the last reply is held back
}

// convoRow is one row of the conversation: rendered text, and the message it belongs to.
type convoRow struct {
	text string
	msg  int    // -1: the space between messages
	wrap bool   // continues the preceding row's rendered line, rather than a hard newline
	gap  string // text removed at that display wrap (a space, or nothing for a glyph split)
}

// convoMsg is one message: its rows, from inclusive to to exclusive, and the Markdown y copies.
type convoMsg struct {
	id       string
	kind     string // as convoCut, with more and none folded into the message before
	raw      string
	from, to int
	body     int // first body row, after a separately rendered message header
}

type convoScreen struct {
	parts []convoPart
	live  bool // --follow: an open turn may still answer
	head  []string
	rows  []convoRow
	msgs  []convoMsg
	sel   int
	top   int
	w, h  int
	last  int // --last: how many of the person's prompts to show, 0 for all
	// ctx ends when the screen is told to stop; a copy in progress gives up then.
	ctx context.Context
	// known is how many messages there were when the newest was last selected; the rest are
	// new below.
	known   int
	status  string // what --follow says: the turn working, or what went wrong
	message string // what the last key did, such as a copy
	search  *convoSearch
}

func newConvoScreen(live bool) *convoScreen {
	return &convoScreen{live: live, w: 80, h: 24}
}

// layout renders every part at the screen's width and cuts it into rows and messages.
func (c *convoScreen) layout() {
	c.rows, c.msgs, c.head = nil, nil, nil
	if len(c.parts) == 0 {
		return
	}
	width := max(c.w-convoGutter, convoMinWidth-convoGutter)
	for i, pt := range c.parts {
		p := newConvoPrinter(pt.s.Provider, true, false)
		p.width, p.live, p.hold, p.track = width, c.live, pt.hold, true
		if !pt.moved.IsZero() {
			c.rows = append(c.rows, convoRow{msg: -1})
			for _, l := range strings.Split(convoMovedLine(p, pt.t, pt.s, pt.moved, pt.from, width), "\n") {
				c.rows = append(c.rows, convoRow{text: convoExpandTabs(l), msg: -1})
			}
		}
		p.skip(pt.s.Turns[:min(pt.from, len(pt.s.Turns))])
		c.cut(i, p.emit(pt.s), p.cuts, width)
	}
	// The header names the conversation shown now.
	last := c.parts[len(c.parts)-1]
	c.head = strings.Split(strings.TrimSuffix(convoHeader(last.s, last.t, last.from, true), "\n"), "\n")
}

// convoWrap breaks a rendered line into rows of at most w cells, where the stream left the
// wrapping to the terminal. It breaks at the last space that fits, else between glyphs. Each
// row ends its styles, and the next row opens again with those still in force.
func convoWrap(line string, w int) []string {
	parts := convoWrapParts(line, w)
	rows := make([]string, len(parts))
	for i, row := range parts {
		rows[i] = row.text
	}
	return rows
}

// convoWrapParts retains the boundary that the terminal display adds. Searching can
// put these rows back together without inventing matches across actual line breaks.
func convoWrapParts(line string, w int) []convoRow {
	if w < 1 || cells(kitPlain(line)) <= w {
		return []convoRow{{text: line}}
	}
	type token struct {
		text  string
		cells int
		sgr   bool
	}
	var toks []token
	glyphs := func(s string) {
		eachGlyph(s, func(g string, c int) bool {
			toks = append(toks, token{text: g, cells: c})
			return true
		})
	}
	at := 0
	for _, m := range kitSGR.FindAllStringIndex(line, -1) {
		glyphs(line[at:m[0]])
		toks = append(toks, token{text: line[m[0]:m[1]], sgr: true})
		at = m[1]
	}
	glyphs(line[at:])
	var rows []convoRow
	open := "" // the styles in force where the row starts
	gap := ""
	for start := 0; start < len(toks); {
		used, end, space := 0, start, -1
		for ; end < len(toks); end++ {
			t := toks[end]
			if t.sgr {
				continue
			}
			if used > 0 && used+t.cells > w {
				break
			}
			if t.text == " " {
				space = end
			}
			used += t.cells
		}
		cut, next := end, end
		if end < len(toks) && space > start {
			cut, next = space, space+1 // the space the row breaks at goes
		}
		var b strings.Builder
		b.WriteString(open)
		for _, t := range toks[start:cut] {
			b.WriteString(t.text)
			if t.sgr {
				if t.text == sgrReset || t.text == "\x1b[m" {
					open = ""
				} else {
					open += t.text
				}
			}
		}
		rows = append(rows, convoRow{text: b.String() + sgrReset, wrap: start > 0, gap: gap})
		gap = ""
		if next > cut {
			gap = " "
		}
		start = next
	}
	return rows
}

// convoTabStop is where a terminal puts its tab stops: every eighth column.
const convoTabStop = 8

// convoExpandTabs turns the tabs in a rendered line into the spaces a terminal would move
// over, counting columns from the line's start and skipping its styles, so every row is as
// wide as it is counted. The stream left tabs to the terminal; the screen lays out its rows.
func convoExpandTabs(line string) string {
	if !strings.Contains(line, "\t") {
		return line
	}
	var b strings.Builder
	col, at := 0, 0
	text := func(s string) {
		eachGlyph(s, func(g string, c int) bool {
			if g == "\t" {
				n := convoTabStop - col%convoTabStop
				b.WriteString(strings.Repeat(" ", n))
				col += n
				return true
			}
			b.WriteString(g)
			col += c
			return true
		})
	}
	for _, m := range kitSGR.FindAllStringIndex(line, -1) {
		text(line[at:m[0]])
		b.WriteString(line[m[0]:m[1]])
		at = m[1]
	}
	text(line[at:])
	return b.String()
}

// cut splits what a printer emitted into messages, in rows width cells wide at most. The
// blank lines before a message are the space between messages; a later part of an answer and
// a "(no reply)" go on the message before them.
func (c *convoScreen) cut(part int, text string, cuts []convoCut, width int) {
	for _, folded := range foldConvoCuts(text, cuts) {
		ct := folded.first
		m := convoMsg{id: fmt.Sprintf("%d\x00%s\x00%s\x00%x\x00%d", part, ct.key, ct.kind, folded.firstHash, folded.occurrence), kind: ct.kind, raw: folded.raw}
		for segment, paint := range folded.segments {
			var lines []convoRow
			headerRows := 0
			for _, l := range strings.Split(paint, "\n") {
				wrapped := convoWrapParts(convoExpandTabs(l), width)
				if segment == 0 && headerRows == 0 && kitPlain(l) != "" && (ct.kind == "you" || ct.kind == "steer" || ct.kind == "answer") {
					headerRows = len(wrapped)
				}
				lines = append(lines, wrapped...)
			}
			start := 0
			if segment == 0 {
				for start < len(lines)-1 && lines[start].text == "" {
					c.rows = append(c.rows, convoRow{msg: -1})
					start++
				}
				m.from = len(c.rows)
				m.body = m.from + headerRows
			}
			for _, line := range lines[start:] {
				line.msg = len(c.msgs)
				c.rows = append(c.rows, line)
			}
		}
		m.to = len(c.rows)
		c.msgs = append(c.msgs, m)
	}
}

// height is how many rows of the conversation fit between the header and the two lines at
// the bottom: the status and the keys.
func (c *convoScreen) height() int { return max(c.h-len(c.head)-2, 1) }

func (c *convoScreen) clampTop() { c.top = clamp(c.top, 0, max(len(c.rows)-c.height(), 0)) }

// following says the newest message is selected, so the next one will be.
func (c *convoScreen) following() bool {
	return c.search == nil && (len(c.msgs) == 0 || c.sel == len(c.msgs)-1)
}

// visible says some row of message i is on screen.
func (c *convoScreen) visible(i int) bool {
	m := c.msgs[i]
	return m.to > c.top && m.from < c.top+c.height()
}

// show scrolls the selected message into view: all of it when it fits, else its start.
func (c *convoScreen) show() {
	if len(c.msgs) == 0 {
		return
	}
	m, h := c.msgs[c.sel], c.height()
	switch {
	case m.to-m.from > h || m.from < c.top:
		c.top = m.from
	case m.to > c.top+h:
		c.top = m.to - h
	}
	c.clampTop()
}

// apply takes what --follow found. A newest message selected moves on to the next one;
// otherwise the selection and its place on screen stay.
func (c *convoScreen) apply(u convoUpdate) {
	if u.s == nil {
		c.status = u.status
		return
	}
	pt := convoPart{s: u.s, t: u.t, from: u.from, moved: u.moved, hold: u.hold}
	if u.begin || len(c.parts) == 0 {
		c.parts = append(c.parts, pt)
	} else {
		last := &c.parts[len(c.parts)-1]
		last.s, last.hold = u.s, u.hold
		if last.from >= len(u.s.Turns) {
			// No turn is left after the cutoff: the file was rewritten (a cutoff is always at
			// a turn there was), so --last counts again from what it holds now.
			last.from = convoStart(u.s.Turns, c.last)
		}
	}
	c.relayout()
}

// resize re-wraps for a new size and keeps the selection.
func (c *convoScreen) resize(w, h int) {
	c.w, c.h = w, h
	c.relayout()
}

// relayout lays the screen out again, keeping the selected message and where it sits on
// screen.
func (c *convoScreen) relayout() {
	defer c.refreshSearch()
	following := c.following()
	id, off, old := "", 0, c.sel
	if len(c.msgs) > 0 {
		id, off = c.msgs[c.sel].id, c.msgs[c.sel].from-c.top
	}
	c.layout()
	if len(c.msgs) == 0 {
		c.sel, c.top = 0, 0
		return
	}
	idx := -1
	for i, m := range c.msgs {
		if m.id == id {
			idx = i
			break
		}
	}
	if idx < 0 || following && idx != len(c.msgs)-1 {
		// A new message to follow, or the one selected is gone.
		c.sel = min(old, len(c.msgs)-1)
		if following {
			c.sel = len(c.msgs) - 1
		}
		c.show()
		c.settle()
		return
	}
	c.sel = idx
	c.top = c.msgs[idx].from - off
	c.clampTop()
	m := c.msgs[idx]
	if !c.visible(idx) || following && m.to-m.from <= c.height() {
		c.show()
	}
	c.settle()
}

// settle notes that the newest message is selected: nothing is new below it.
func (c *convoScreen) settle() {
	if c.following() {
		c.known = len(c.msgs)
	}
}

// down moves on through the selected message while more of it is below the screen, then to
// the next message.
func (c *convoScreen) down() {
	if len(c.msgs) == 0 {
		return
	}
	m, bottom := c.msgs[c.sel], c.top+c.height()
	if m.to > bottom && m.from < bottom {
		c.top = min(c.top+convoStep, m.to-c.height())
		c.clampTop()
		return
	}
	if c.sel < len(c.msgs)-1 {
		c.sel++
	}
	c.show()
}

// up moves back through the selected message while its start is above the screen, then to
// the message before.
func (c *convoScreen) up() {
	if len(c.msgs) == 0 {
		return
	}
	m := c.msgs[c.sel]
	if m.from < c.top && m.to > c.top {
		c.top = max(c.top-convoStep, m.from)
		c.clampTop()
		return
	}
	if c.sel > 0 {
		c.sel--
	}
	c.show()
}

// prompt says message i is something the person typed.
func (c *convoScreen) prompt(i int) bool { return c.msgs[i].kind == "you" || c.msgs[i].kind == "steer" }

// jump selects the next prompt the person typed (d 1) or the one before (d -1).
func (c *convoScreen) jump(d int) {
	for i := c.sel + d; i >= 0 && i < len(c.msgs); i += d {
		if c.prompt(i) {
			c.sel = i
			c.show()
			return
		}
	}
}

// scroll moves the view by d rows. The selection stays on screen, so y copies what can be
// seen: scrolled past it, the nearest message on screen is selected.
func (c *convoScreen) scroll(d int) {
	c.top += d
	c.clampTop()
	if len(c.msgs) == 0 || c.visible(c.sel) {
		return
	}
	if c.msgs[c.sel].to <= c.top {
		for i := range c.msgs {
			if c.visible(i) {
				c.sel = i
				return
			}
		}
		return
	}
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if c.visible(i) {
			c.sel = i
			return
		}
	}
}

// copy puts the selected message's Markdown on the clipboard and says how that went.
func (c *convoScreen) copy() {
	if len(c.msgs) == 0 {
		return
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	where, err := asksCopier(ctx, c.msgs[c.sel].raw)
	if err != nil {
		c.message = fmt.Sprintf("Could not copy: %v", err)
		return
	}
	c.message = "Copied the message to the " + where
}

// convoQuits says the event quits the screen: q, Escape, Ctrl-C or Ctrl-D. Text inside a
// paste is no event, so a q pasted does not quit.
func convoQuits(e asksInput) bool {
	k := e.key
	return !e.paste && !e.mouse && (k.code == keyQuit || k.code == keyBack || k.r == 'q')
}

// input handles one event and says whether to quit.
func (c *convoScreen) input(e asksInput) bool {
	c.message = ""
	if c.search != nil {
		return c.searchInput(e)
	}
	switch {
	case e.paste:
		c.message = "Pasted text does nothing here"
	case e.mouse:
		c.click(e)
	default:
		k := e.key
		switch {
		case convoQuits(e):
			return true
		case k.r == '/':
			c.search = &convoSearch{}
		case k.code == keyDown:
			c.down()
		case k.code == keyUp:
			c.up()
		case k.r == '\t':
			c.jump(1)
		case k.code == keyBackTab:
			c.jump(-1)
		case k.code == keyHome || k.r == 'g':
			c.sel, c.top = 0, 0
		case k.code == keyEnd:
			c.sel = max(len(c.msgs)-1, 0)
			c.show()
		case k.code == keyPgDn:
			c.scroll(max(c.height()-1, 1))
		case k.code == keyPgUp:
			c.scroll(-max(c.height()-1, 1))
		case k.r == 'y' || k.r == 'c':
			c.copy()
		}
	}
	c.settle()
	return false
}

// click selects the message under a left click; the wheel scrolls.
func (c *convoScreen) click(e asksInput) {
	switch e.button {
	case 64:
		c.scroll(-convoStep)
	case 65:
		c.scroll(convoStep)
	case 0:
		y := e.y - 1 - len(c.head)
		if c.narrow() || y < 0 || y >= c.height() || c.top+y >= len(c.rows) {
			return
		}
		if r := c.rows[c.top+y]; r.msg >= 0 {
			c.sel = r.msg
		}
	}
}

// newBelow is how many messages came since the newest was last selected.
func (c *convoScreen) newBelow() int {
	if !c.live || c.following() {
		return 0
	}
	return max(len(c.msgs)-c.known, 0)
}

// footer is one dim line of keys, cut down by width, as in aiq asks.
func (c *convoScreen) footer(th kitTheme) []kitSeg {
	keys := []kitKey{{"j/k", "move", 0}, {"/", "search", 0}}
	switch {
	case c.w < 50:
		keys = append(keys, kitKey{"y", "copy", 0}, kitKey{"q", "quit", 0})
	case c.w < 80:
		keys = append(keys, kitKey{"Tab", "prompt", 0}, kitKey{"y", "copy", 0}, kitKey{"q", "quit", 0})
	default:
		keys = append(keys, kitKey{"Tab", "prompt", 0}, kitKey{"g/G", "first/last", 0},
			kitKey{"y", "copy", 0}, kitKey{"q", "quit", 0})
	}
	segs, _ := kitKeys(keys, th.dim, 1)
	return append([]kitSeg{{text: " "}}, segs...)
}

// statusLine says what the last key did, what is new below, and what --follow reports.
func (c *convoScreen) statusLine(th kitTheme) []kitSeg {
	var segs []kitSeg
	if c.message != "" {
		segs = append(segs, kitSeg{style: sgrBold + th.accent, text: " " + c.message})
	}
	if n := c.newBelow(); n > 0 {
		segs = append(segs, kitSeg{style: sgrBold + th.accent, text: fmt.Sprintf(" %d new below, G follows", n)})
	}
	if c.status != "" {
		segs = append(segs, kitSeg{style: th.dim, text: " " + c.status})
	}
	return segs
}

// convoMinWidth is the narrowest screen the conversation is shown on. Below it the screen
// says so, rather than cut every row short.
const convoMinWidth = 12

// narrow says the screen is too narrow to show the conversation.
func (c *convoScreen) narrow() bool { return c.w < convoMinWidth }

// frame is the screen, one string a row, each exactly w cells.
func (c *convoScreen) frame() []string {
	th, mk := kitThemeFor(false), kitMarksFor(os.Getenv)
	rail := "│"
	if mk.selected != kitUnicode.selected {
		rail = "|"
	}
	c.clampTop()
	var out []string
	if c.narrow() {
		label := "Too narrow; q quits"
		if c.search != nil {
			label = "Too narrow; Ctrl-C quits"
		}
		out = append(out, kitLine([]kitSeg{{style: sgrBold + th.accent, text: label}}, "", c.w))
		for len(out) < c.h {
			out = append(out, kitLine(nil, "", c.w))
		}
		return out
	}
	if c.search != nil {
		return c.searchFrame(th, mk)
	}
	for _, l := range c.head {
		out = append(out, kitLine([]kitSeg{{text: convoExpandTabs(l), raw: true}}, "", c.w))
	}
	for i := c.top; i < c.top+c.height(); i++ {
		if i >= len(c.rows) {
			out = append(out, kitLine(nil, "", c.w))
			continue
		}
		r := c.rows[i]
		gutter := kitSeg{text: strings.Repeat(" ", convoGutter)}
		if r.msg >= 0 && r.msg == c.sel {
			gutter = kitSeg{style: th.ink, text: rail + " "}
			if i == c.msgs[r.msg].from {
				gutter = kitSeg{style: sgrBold + th.ink, text: mk.selected + " "}
			}
		}
		out = append(out, kitLine([]kitSeg{gutter, {text: r.text, raw: true}}, "", c.w))
	}
	out = append(out, kitLine(c.statusLine(th), "", c.w), kitLine(c.footer(th), "", c.w))
	return out[:min(len(out), c.h)]
}

func (c *convoScreen) draw(out io.Writer) error {
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, row := range c.frame() {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(sgrReset + "\x1b[2K" + row)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// showConvo shows the conversation as it is now.
func showConvo(in *devTTY, tgt convoTarget, last int, stop <-chan struct{}) error {
	_, mod := statFile(tgt.path)
	s, err := loadConvo(tgt)
	if err != nil {
		return err
	}
	if !tgt.stale && s.Unfinished && time.Since(mod) < convoQuiet {
		holdLastTurn(s) // see convoFollow.quiet
	}
	c := newConvoScreen(false)
	c.last = last
	c.apply(convoUpdate{s: s, t: tgt, begin: true, from: convoStart(s.Turns, last)})
	c.status = workingLabel(s, time.Now())
	return runConvoScreen(c, newConvoTTY(in, os.Stdout), nil, nil, stop)
}

// convoPanic is a panic in the --follow loop, carried to the screen so the terminal is put
// back before it goes on.
type convoPanic struct {
	value any
	stack []byte
}

func (p convoPanic) Error() string { return fmt.Sprintf("%v\n\n%s", p.value, p.stack) }

// repanic goes on with a panic the --follow loop had, now that the terminal is back.
func repanic(err error) error {
	if p, ok := err.(convoPanic); ok {
		panic(p)
	}
	return err
}

// screen runs --follow on the screen: the loop finds what changed and the screen shows it.
func (f *convoFollow) screen(in *devTTY, stop <-chan struct{}) error {
	if f.tgt.global {
		select {
		case <-stop:
			return nil
		default:
		}
	}
	updates := make(chan convoUpdate)
	quit := make(chan struct{})
	defer close(quit)
	f.send = func(u convoUpdate) {
		select {
		case updates <- u:
		case <-quit:
		}
	}
	failed := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				failed <- convoPanic{r, debug.Stack()}
			}
		}()
		failed <- f.loop(quit)
	}()
	// The conversation is read before the screen takes the terminal, so an error shows as one.
	c := newConvoScreen(true)
	c.last = f.last
	select {
	case u := <-updates:
		c.apply(u)
	case err := <-failed:
		return repanic(err)
	case <-stop:
		return nil
	}
	return repanic(runConvoScreen(c, newConvoTTY(in, os.Stdout), updates, failed, stop))
}

// convoTerm is the terminal the screen runs on; tests fake it.
type convoTerm interface {
	raw() error                    // takes the keyboard
	write(s string) error          // draws on the screen
	size() (w, h int, err error)   // the screen's size
	keys() (<-chan []byte, func()) // what is typed; the func stops the reading and waits for it
	discard(pasting bool)          // drops what was typed and not read, and the rest of a paste
	cooked()                       // puts back the mode raw found
	abandon()                      // drops output the terminal has not taken
}

// A paste that may still be coming in when the screen ends (one cut short, or a sequence only
// begun, which may be the start of a paste mark) is read and dropped up to its end mark, or
// until nothing more has come for convoPasteQuiet, and for convoPasteLimit at most: a paste
// that never ends cannot hold the way out. What comes after that is the shell's.
const (
	convoPasteQuiet = 100 * time.Millisecond
	convoPasteLimit = 500 * time.Millisecond
)

// drainPaste reads and drops the rest of a paste, as above.
func drainPaste(read func(time.Duration) []byte, quiet, limit time.Duration) {
	deadline := time.Now().Add(limit)
	var tail []byte
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		b := read(min(quiet, left))
		if len(b) == 0 {
			return
		}
		tail = append(tail, b...)
		if bytes.Contains(tail, pasteEnd) {
			return
		}
		tail = tail[max(len(tail)-len(pasteEnd)+1, 0):]
	}
}

// convoLeaveWait is how long the way out waits on output that may be stuck, as when the
// terminal has paused it: for the escapes that leave the screen, then for the screen to end.
var convoLeaveWait = time.Second

// convoExit ends aiq when the screen cannot end after a stop, its output stuck; the keyboard
// is back by then. Tests swap it.
var convoExit = os.Exit

// convoPiece is the most of a frame one write puts out: less than a terminal's output queue
// holds, so a write held up by a stopped terminal finishes once the queue is dropped.
const convoPiece = 256

// convoPieceWait is how long the way out gives that write to finish before it drops the
// queue again.
const convoPieceWait = 50 * time.Millisecond

// convoOut is a writer through a func.
type convoOut func(string) error

func (o convoOut) Write(p []byte) (int, error) {
	if err := o(string(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// runConvoScreen shows c until q, Esc or Ctrl-C, a signal (stop), or the end of what feeds
// it (failed). updates brings what --follow finds; both are nil for a conversation as it is.
//
// However it ends, the terminal goes back the same way, once: the key reader stops, what was
// typed and not read is dropped, and the keyboard's mode is put back, none of which waits on
// the output; only then come the escapes that leave the screen, waited on for a time. A stop
// does this on its own, so output the terminal does not take, or a copy in progress, cannot
// hold the keyboard; if the screen still has not ended a while after, aiq exits.
func runConvoScreen(c *convoScreen, t convoTerm, updates <-chan convoUpdate, failed <-chan error, stop <-chan struct{}) error {
	if err := t.raw(); err != nil {
		return err
	}
	leaveWait, exit := convoLeaveWait, convoExit
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ctx = ctx
	// Frames and the leaving escapes take turns; once the screen is left, frames go nowhere.
	var outMu sync.Mutex
	left := false
	// abandoned says the output is stuck and aiq is about to exit: nothing more is written.
	var abandoned atomic.Bool
	write := func(s string) error {
		outMu.Lock()
		defer outMu.Unlock()
		// A frame goes out in pieces, so a write the terminal holds up holds only a piece
		// once the screen gives up on it.
		for len(s) > 0 && !left && !abandoned.Load() {
			n := min(len(s), convoPiece)
			if err := t.write(s[:n]); err != nil {
				return err
			}
			s = s[n:]
		}
		return nil
	}
	keys, stopKeys := t.keys()
	// pasting says a paste may still be coming in. convoInput alone sets it, and closes
	// settled once it has parsed the last of the keys, so the way out reads it after that.
	var pasting atomic.Bool
	settled := make(chan struct{})
	var once sync.Once
	modeBack, gone := make(chan struct{}), make(chan struct{})
	restore := func() {
		once.Do(func() {
			go func() {
				defer close(gone)
				stopKeys() // the keys end, and convoInput parses what came before that
				// Keys convoInput has not finished parsing in time may begin a paste.
				unsettled := false
				select {
				case <-settled:
				case <-time.After(convoPieceWait):
					unsettled = true
				}
				t.discard(unsettled || pasting.Load())
				t.cooked()
				close(modeBack)
				outMu.Lock()
				defer outMu.Unlock()
				left = true
				if !abandoned.Load() {
					t.write(asksTerminalLeave)
				}
			}()
		})
		// Each wait has its own timer, both ending leaveWait from now.
		end := time.Now().Add(leaveWait)
		select {
		case <-modeBack:
		case <-time.After(leaveWait):
			return
		}
		select {
		case <-gone:
		case <-time.After(time.Until(end)):
		}
	}
	finished := make(chan struct{})
	// quitting closes when a key quits. The keys are read and parsed apart from the screen, so
	// a quit key works as a stop does even while a frame or a copy holds the screen up.
	quitting := make(chan struct{})
	var quitOnce sync.Once
	quit := func() { quitOnce.Do(func() { close(quitting) }) }
	go func() {
		select {
		case <-stop:
		case <-quitting:
		case <-finished:
			return
		}
		cancel()
		restore()
		select {
		case <-finished:
		case <-time.After(leaveWait):
			// Output the terminal does not take holds a process even as it exits, where it
			// closes the terminal and waits for the output to go; so it is dropped, then the
			// piece being written when there was room again, and nothing more is written.
			abandoned.Store(true)
			t.abandon()
			time.Sleep(convoPieceWait)
			t.abandon()
			exit(0)
		}
	}()
	defer close(finished)
	defer restore()
	// The keys are read before anything is written, so a quit key works even if the terminal
	// takes no output from the start.
	events := make(chan asksInput)
	go convoInput(keys, events, quit, &pasting, settled, finished)
	if err := write(asksTerminalEnter + asksMouseEnter); err != nil {
		return err
	}
	size := func() {
		w, h, err := t.size()
		if err != nil || w < 1 || h < 1 {
			w, h = 80, 24
		}
		c.resize(w, h)
	}
	size()
	winch, stopResize := watchResize()
	defer stopResize()
	for {
		select {
		case <-stop:
			return nil
		case <-quitting:
			return nil
		default:
		}
		if err := c.draw(convoOut(write)); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-quitting:
			return nil
		case err := <-failed:
			return err
		case <-winch:
			size()
		case u := <-updates:
			c.apply(u)
		case e := <-events:
			if c.input(e) {
				return nil
			}
		}
	}
}

// convoInput turns what is typed into events for the screen, queued so the reading never
// waits on the screen. A quit key, or the end of the keys, calls quit as well. A lone Escape
// waits escapeWait for the rest of a sequence before it counts, as in aiq asks; pasting says
// a paste may still be coming in, for the way out to drop. It closes settled once the keys have
// ended and it has parsed all that came before, after which pasting no longer changes.
func convoInput(keys <-chan []byte, events chan<- asksInput, quit func(), pasting *atomic.Bool, settled chan<- struct{}, finished <-chan struct{}) {
	done := false // settled is closed
	defer func() {
		if !done {
			close(settled)
		}
	}()
	tokens := asksTokenizer{literal: true}
	searching := false // follows event order, even while output holds the screen up
	var escape, orphan <-chan time.Time
	var queue []asksInput
	take := func(evs []asksInput) {
		// A paste may still be coming: one cut short, a sequence begun and not finished
		// (it may be a paste mark), or an Escape just taken as the key, whose sequence may
		// yet follow. This is set before a quit is called, so the way out sees it.
		pasting.Store(tokens.pasting || tokens.cutPaste || len(tokens.pending) > 0 || tokens.afterEscape)
		for _, e := range evs {
			if !searching && convoQuits(e) || !e.paste && !e.mouse && e.key.code == keyQuit {
				quit()
			}
			searching = convoSearchMode(searching, e)
		}
		queue = append(queue, evs...)
	}
	for {
		var out chan<- asksInput
		var head asksInput
		if len(queue) > 0 {
			out, head = events, queue[0]
		}
		select {
		case <-finished:
			return
		case out <- head:
			queue = queue[1:]
		case <-escape:
			escape = nil
			take(tokens.flush())
			orphan = time.After(orphanWait)
		case <-orphan:
			orphan = nil
			tokens.forget()
			take(nil)
		case data, ok := <-keys:
			if !ok {
				take(nil)
				quit()
				keys, escape, orphan = nil, nil, nil
				close(settled)
				done = true
				continue
			}
			take(tokens.feed(data))
			escape, orphan = nil, nil
			if tokens.waiting() {
				escape = time.After(escapeWait)
			}
		}
	}
}
