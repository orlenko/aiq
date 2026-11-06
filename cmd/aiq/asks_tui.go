package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

var asksSections = []string{"Asks", "Warnings", "Other open", "Probably done", "Closed and dismissed"}

type asksSnapshot struct {
	items, moved []state.Ask
	status       state.AsksStatus
	leases       []state.Lease
	workspaces   []string
	off          map[string]bool
	keyOK        bool
}

type asksRequest struct {
	action string
	id     int64
	to     state.AskState
	ws     string
	off    bool
	closed bool
	next   string // the selection once the request is taken
}

type asksResult struct {
	snapshot  asksSnapshot
	err       error
	message   string
	undo      int64
	clearUndo bool
}

func asksWorkspace(l state.Lease) string {
	if l.Workspace != "" {
		return l.Workspace
	}
	return l.Cwd
}

// No transcript path or quoted text participates in navigation or session switches.
func asksLiveLeases(all []state.Lease) []state.Lease {
	var out []state.Lease
	for _, l := range all {
		if l.Mode == state.ModeLong && l.Hostname == hostname() && proc.Alive(l.PID) {
			out = append(out, l)
		}
	}
	return out
}

func asksLoad(reg, leases *state.Store, closed bool, now time.Time) (s asksSnapshot, err error) {
	states := []state.AskState{state.AskOpen, state.AskProbablyDone}
	if closed {
		states = append(states, state.AskClosed, state.AskDismissed)
	}
	if s.items, err = reg.ListAsks("", states...); err != nil {
		return s, err
	}
	if s.moved, err = reg.ListAsksMoved(asksDayStart(now)); err != nil {
		return s, err
	}
	if s.status, err = reg.GetAsksStatus(now); err != nil {
		return s, err
	}
	if s.off, err = reg.AskWorkspacesOff(); err != nil {
		return s, err
	}
	all, err := leases.ListLeases()
	if err != nil {
		return s, err
	}
	s.leases, s.keyOK = asksLiveLeases(all), asksKey() != ""
	s.workspaces, err = asksWorkspaceNames(leases)
	if err != nil {
		return s, err
	}
	return s, nil
}

func asksApply(reg, leases *state.Store, r asksRequest, now time.Time) asksResult {
	var out asksResult
	switch r.action {
	case "state":
		out.err = reg.SetAskByPerson(r.id, r.to, now.Unix())
		if out.err == nil {
			verb := "Dismissed"
			if r.to == state.AskOpen {
				verb = "Restored"
			}
			out.message, out.undo = fmt.Sprintf("%s #%d. u undo", verb, r.id), r.id
		}
	case "undo":
		_, out.err = reg.UndoAsk(r.id, now.Unix())
		if out.err == nil {
			out.message = fmt.Sprintf("Undid #%d", r.id)
			out.clearUndo = true
		}
	case "workspace":
		out.err = reg.SetAskWorkspaceOff(r.ws, r.off, now)
	case "reset":
		var n int
		n, out.err = asksReset(reg, r.ws, now)
		if out.err == nil {
			out.message = fmt.Sprintf("Erased %d asks. New asks start from now", n)
			out.clearUndo = true
		}
	}
	if out.err == nil {
		out.snapshot, out.err = asksLoad(reg, leases, r.closed, now)
	}
	return out
}

// asksWorker owns all store calls while the screen is up. A slow read cannot hold
// terminal input or signal restoration. Requests are serialized, including writes.
func asksWorker(reg, leases *state.Store, requests <-chan asksRequest, results chan<- asksResult, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case r := <-requests:
			result := asksApply(reg, leases, r, time.Now())
			select {
			case results <- result:
			case <-stop:
				return
			}
		}
	}
}

// asksDrain waits, up to limit, for the result of a request the worker was given.
func asksDrain(busy bool, results <-chan asksResult, limit time.Duration) {
	if busy {
		select {
		case <-results:
		case <-time.After(limit):
		}
	}
}

type asksRow struct {
	key, text, section, workspace string
	id                            int64
	action                        string
	segs                          []kitSeg
	bg                            string
	hits                          []asksHit
}

type asksScreen struct {
	snapshot   asksSnapshot
	rows       []asksRow
	expanded   map[int64]bool
	sections   map[string]bool
	selected   string
	top        int
	freeScroll bool
	w, h       int
	sessions   bool
	moved      bool
	loaded     bool
	busy       bool
	stale      bool
	message    string
	undo       int64
	noColor    bool
	// here is the directory to filter on, and filtered says the filter is on.
	here     string
	filtered bool
	// confirmReset says X was pressed, and the next key decides. confirmShown says the
	// question has been drawn since, so a y typed or pasted with the X does not answer it.
	// confirmClipped says it was drawn but did not fit.
	confirmReset   bool
	confirmShown   bool
	confirmClipped bool
}

// shownItems and shownMoved are what the filter leaves.
func (b *asksScreen) shownItems() []state.Ask {
	if !b.filtered {
		return b.snapshot.items
	}
	return asksFilter(b.snapshot.items, b.here)
}

func (b *asksScreen) shownMoved() []state.Ask {
	if !b.filtered {
		return b.snapshot.moved
	}
	return asksFilter(b.snapshot.moved, b.here)
}

// asksStartFiltered opens the app on this directory's items when it holds a long session,
// because the list across every session is too much to read beside one of them.
func asksStartFiltered(leases *state.Store, dir string, all bool) bool {
	if all {
		return false
	}
	names, err := asksWorkspaceNames(leases)
	if err != nil {
		return false
	}
	for _, name := range names {
		if asksInDir(name, dir) {
			return true
		}
	}
	return false
}

func newAsksScreen() *asksScreen {
	return &asksScreen{expanded: map[int64]bool{}, sections: map[string]bool{
		"Asks": true, "Warnings": true,
	}, w: 80, h: 24, noColor: os.Getenv("NO_COLOR") != ""}
}

func asksText(s string) string {
	return transcript.Clean(strings.ReplaceAll(sanitize(s), "**", ""))
}

func asksGroups(items []state.Ask, now time.Time) map[string][]state.Ask {
	groups := map[string][]state.Ask{}
	for _, a := range items {
		name := "Closed and dismissed"
		switch a.State {
		case state.AskOpen:
			name = asksGroup(a, now.Unix())
		case state.AskProbablyDone:
			name = "Probably done"
		}
		groups[name] = append(groups[name], a)
	}
	return groups
}

func (b *asksScreen) current() asksRow {
	if b.selected == "moved-header" {
		return asksRow{key: "moved-header"}
	}
	for _, r := range b.rows {
		if r.key == b.selected {
			return r
		}
	}
	return asksRow{}
}

func (b *asksScreen) item(id int64) (state.Ask, bool) {
	for _, list := range [][]state.Ask{b.shownItems(), b.shownMoved()} {
		for _, a := range list {
			if a.ID == id {
				return a, true
			}
		}
	}
	return state.Ask{}, false
}

func (b *asksScreen) workspaceName(ws string) string {
	base := filepath.Base(ws)
	for _, list := range [][]state.Ask{b.shownItems(), b.shownMoved()} {
		for _, a := range list {
			if a.Workspace != ws && filepath.Base(a.Workspace) == base {
				return asksText(filepath.Join(filepath.Base(filepath.Dir(ws)), base))
			}
		}
	}
	for _, l := range b.snapshot.leases {
		other := asksWorkspace(l)
		if other != ws && filepath.Base(other) == base {
			return asksText(filepath.Join(filepath.Base(filepath.Dir(ws)), base))
		}
	}
	return asksText(base)
}

// asksLinks finds [text](http...) in a quote, so a collapsed quote shows only the text.
var asksLinks = regexp.MustCompile(`\[([^\]\n]+)\]\(https?://[^)\s]+\)`)

// asksQuoteRows renders a quote as Markdown, wrapped in full. The text is cleaned first, so
// the only escapes in the result are the renderer's own. base opens every row, which keeps a
// band unbroken through the renderer's resets.
func (b *asksScreen) asksQuoteRows(quote string, expanded bool, width int, base string) []string {
	text := strings.TrimSpace(sanitize(quote))
	if text == "" {
		return nil
	}
	if !expanded {
		text = asksLinks.ReplaceAllString(text, "$1")
	}
	saved := pal
	if b.noColor {
		pal = kitMonoPalette
	}
	defer func() { pal = saved }()
	rows := renderMarkdownRows(text, max(width, 10), base)
	for len(rows) > 0 && strings.TrimSpace(kitPlain(rows[len(rows)-1])) == "" {
		rows = rows[:len(rows)-1]
	}
	return rows
}

// asksWideFrom is the width where an item's details become a column beside its quote.
const asksWideFrom = 96

func (b *asksScreen) itemRows(a state.Ask, prefix string, now time.Time) []asksRow {
	key := fmt.Sprintf("%s:%d", prefix, a.ID)
	th, mk := kitThemeFor(b.noColor), b.marks()
	selected := key == b.selected
	body := ""
	if selected {
		body = th.selBand
	}
	first := th.itemBand
	if selected {
		first = th.selBand
	}
	add := func(out []asksRow, bg string, segs ...kitSeg) []asksRow {
		return append(out, asksRow{key: key, id: a.ID, segs: segs, bg: bg, text: kitText(segs)})
	}
	mark := kitSeg{style: th.dim, text: mk.ended}
	for _, l := range b.snapshot.leases {
		if asksWorkspace(l) == a.Workspace {
			mark = kitSeg{style: th.agent, text: mk.live}
			break
		}
	}
	pre := kitSeg{text: "  "}
	if selected {
		pre = kitSeg{style: sgrBold + th.ink, text: mk.selected + " "}
	}
	warning := a.State == state.AskOpen && asksGroup(a, now.Unix()) == "Warnings"
	idStyle := ""
	if warning {
		idStyle = sgrBold + th.warn
	} else if selected {
		idStyle = sgrBold
	}
	id := fmt.Sprintf("#%d", a.ID)
	age := fmtAge(max(now.Sub(time.Unix(a.RaisedAt, 0)), 0))
	if a.Repeats > 0 {
		age += fmt.Sprintf(" ×%d", a.Repeats+1)
	}
	word, wordStyle := "", th.dim
	if warning {
		word, wordStyle = "risky", sgrBold+th.warn
	} else if a.State != state.AskOpen {
		word = string(a.State)
	}
	dot := " " + mk.dot + " "
	var out []asksRow
	quoteRow := func(indent int, row string) []kitSeg {
		return []kitSeg{{text: strings.Repeat(" ", indent)}, {text: row, raw: true}}
	}
	if b.w >= asksWideFrom {
		meta := []kitSeg{pre, mark, {text: " "}, {style: idStyle, text: pad(id, 7)}}
		if !b.filtered {
			meta = append(meta, kitSeg{text: pad(b.workspaceName(a.Workspace), 13)})
		}
		meta = append(meta, kitSeg{style: th.dim, text: pad(asksText(a.Provider), 7)}, kitSeg{style: th.dim, text: pad(age, 11)})
		if word != "" {
			meta = append(meta, kitSeg{style: wordStyle, text: pad(word, 11)})
		}
		col := cells(kitText(meta))
		width := b.w - col - 1
		quote := a.FirstQuote
		if b.expanded[a.ID] {
			quote = a.LastQuote
		}
		rows := b.asksQuoteRows(quote, b.expanded[a.ID], width, first)
		if len(rows) == 0 {
			out = add(out, first, meta...)
		} else {
			out = add(out, first, append(meta, kitSeg{text: rows[0], raw: true})...)
		}
		if len(rows) > 1 {
			rows = b.asksQuoteRows(quote, b.expanded[a.ID], width, body)
			for _, row := range rows[1:] {
				out = add(out, body, quoteRow(col, row)...)
			}
		}
		out = b.itemFooter(out, a, key, col, body, width, th, add)
		return out
	}
	meta := []kitSeg{pre, mark, {text: " "}, {style: idStyle, text: id}, {text: "  "}}
	if !b.filtered {
		meta = append(meta, kitSeg{text: b.workspaceName(a.Workspace)}, kitSeg{style: th.dim, text: dot})
	}
	meta = append(meta, kitSeg{style: th.dim, text: asksText(a.Provider) + dot + age})
	if word != "" {
		meta = append(meta, kitSeg{style: th.dim, text: dot}, kitSeg{style: wordStyle, text: word})
	}
	out = add(out, first, meta...)
	const indent = 4
	width := b.w - indent - 1
	quote := a.FirstQuote
	if b.expanded[a.ID] {
		quote = a.LastQuote
	}
	for _, row := range b.asksQuoteRows(quote, b.expanded[a.ID], width, body) {
		out = add(out, body, quoteRow(indent, row)...)
	}
	return b.itemFooter(out, a, key, indent, body, width, th, add)
}

// itemFooter adds what comes under a quote: why it moved, its path when open, and its keys.
func (b *asksScreen) itemFooter(out []asksRow, a state.Ask, key string, col int, body string, width int, th kitTheme,
	add func([]asksRow, string, ...kitSeg) []asksRow) []asksRow {
	margin := kitSeg{text: strings.Repeat(" ", col)}
	if a.State != state.AskOpen && (a.StateReason != "" || a.StateText != "") {
		for _, text := range wrap("By "+asksText(a.StateReason)+": "+asksText(a.StateText), max(width, 10)) {
			out = add(out, body, margin, kitSeg{style: th.dim, text: text})
		}
	}
	if b.expanded[a.ID] {
		for _, text := range wrap(asksText(a.Workspace)+" "+b.marks().dot+" "+asksText(a.SessionID), max(width, 10)) {
			out = add(out, body, margin, kitSeg{style: th.dim, text: text})
		}
	}
	if key == b.selected {
		dismiss, restore := state.AskAction(a.State)
		var keys []kitKey
		if dismiss {
			keys = append(keys, kitKey{"d", "dismiss", 'd'})
		}
		if restore {
			keys = append(keys, kitKey{"r", "restore", 'r'})
		}
		more := "more"
		if b.expanded[a.ID] {
			more = "less"
		}
		keys = append(keys, kitKey{"y", "copy", 'y'}, kitKey{"Enter", more, 0})
		segs, hits := kitKeys(keys, th.dim, col)
		out = add(out, body, append([]kitSeg{margin}, segs...)...)
		out[len(out)-1].action, out[len(out)-1].hits = "keys", hits
	}
	return out
}

func (b *asksScreen) marks() kitMarks { return kitMarksFor(os.Getenv) }

// plainRow is a row of the sessions list, which has one style and may wrap.
func (b *asksScreen) plainRow(key, workspace, action, style, text string) asksRow {
	th, mk := kitThemeFor(b.noColor), b.marks()
	pre, bg := kitSeg{text: "  "}, ""
	if key == b.selected {
		pre, bg = kitSeg{style: sgrBold + th.ink, text: mk.selected + " "}, th.selBand
	}
	segs := []kitSeg{pre, {style: style, text: text}}
	return asksRow{key: key, workspace: workspace, action: action, text: text, segs: segs, bg: bg}
}

// sectionRow is a section's bar. Sections are the strong lines, items the faint ones.
func (b *asksScreen) sectionRow(key, section, name, count string, open, warn bool) asksRow {
	th, mk := kitThemeFor(b.noColor), b.marks()
	style := sgrBold + th.ink
	if warn {
		style = sgrBold + th.warn
	}
	label := " " + strings.ToUpper(name)
	if count != "" {
		label += " " + count
	}
	label += " "
	if !open {
		label = " " + mk.closed + label
	}
	bg := ""
	if key == b.selected {
		bg = th.selBand
		label = mk.selected + label
	}
	segs := kitBar(label, style, mk.bar, b.w)
	return asksRow{key: key, section: section, text: label, segs: segs, bg: bg}
}

func (b *asksScreen) layout(now time.Time) {
	oldIndex := 0
	for i, r := range b.rows {
		if r.key == b.selected {
			oldIndex = i
			break
		}
	}
	b.rows = nil
	th := kitThemeFor(b.noColor)
	if b.sessions {
		b.rows = append(b.rows, b.sectionRow("sessions", "Sessions", "Sessions", "", true, false))
		workspaces := append([]string(nil), b.snapshot.workspaces...)
		sort.Strings(workspaces)
		for _, ws := range workspaces {
			mode := "on"
			if b.snapshot.off[ws] {
				mode = "off"
			}
			for _, text := range wrap(mode+" "+b.marks().dot+" "+asksText(ws), max(b.w-3, 10)) {
				b.rows = append(b.rows, b.plainRow("ws:"+ws, ws, "workspace", "", text))
			}
		}
		for i, why := range b.snapshot.status.NotWatched {
			for _, text := range wrap("Not watched: "+asksText(why), max(b.w-3, 10)) {
				b.rows = append(b.rows, b.plainRow(fmt.Sprintf("nw:%d", i), "", "", th.dim, text))
			}
		}
		if b.snapshot.status.LastError != "" {
			for _, text := range wrap("Last error: "+asksText(b.snapshot.status.LastError), max(b.w-3, 10)) {
				b.rows = append(b.rows, b.plainRow("last-error", "", "", th.warn, text))
			}
		}
	} else {
		if b.moved {
			moved := b.shownMoved()
			b.rows = append(b.rows, b.sectionRow("moved-header", "", "Moved today", strconv.Itoa(len(moved)), true, false))
			for _, a := range moved {
				b.rows = append(b.rows, b.itemRows(a, "moved", now)...)
			}
		}
		groups := asksGroups(b.shownItems(), now)
		for _, name := range asksSections {
			count := strconv.Itoa(len(groups[name]))
			if name == "Closed and dismissed" && !b.sections[name] {
				count = "" // not loaded until it is opened
			}
			b.rows = append(b.rows, b.sectionRow("section:"+name, name, name, count, b.sections[name], name == "Warnings" && len(groups[name]) > 0))
			if b.sections[name] {
				for _, a := range groups[name] {
					b.rows = append(b.rows, b.itemRows(a, "ask", now)...)
				}
			}
		}
	}
	for i, r := range b.rows {
		if r.key == b.selected {
			b.top += i - oldIndex
			return
		}
	}
	if b.selected == "moved-header" {
		return
	}
	if len(b.rows) > 0 {
		// The selection moved, and how a row looks depends on it.
		b.selected = b.rows[min(oldIndex, len(b.rows)-1)].key
		b.layout(now)
	}
}

// asksStatusProblem says the status line means the list cannot be trusted right now.
func asksStatusProblem(status string) bool {
	for _, p := range []string{"Off:", "Not watching", "Failing", "Stale:"} {
		if strings.HasPrefix(status, p) {
			return true
		}
	}
	return false
}

// Header rows are fixed, so status and rule-driven moves remain visible when scrolled.
func (b *asksScreen) header(now time.Time) []asksRow {
	th, mk := kitThemeFor(b.noColor), b.marks()
	open, total, hidden := 0, 0, 0
	for _, a := range b.shownItems() {
		if a.State == state.AskOpen {
			open++
		}
	}
	for _, a := range b.snapshot.items {
		if a.State == state.AskOpen {
			total++
			if !asksInDir(a.Workspace, b.here) {
				hidden++
			}
		}
	}
	segs := func(s ...kitSeg) asksRow { return asksRow{segs: s, text: kitText(s)} }
	// Line 1: the tabs. The screen and the scope in use are the only reverse video here.
	tabs := []kitSeg{{style: th.tab, text: " aiq asks "}, {text: "  "}}
	here := asksText(filepath.Base(b.here))
	switch {
	case b.filtered:
		// Hidden items must show, or an empty filtered list reads as nothing waiting anywhere.
		tabs = append(tabs, kitSeg{style: th.tab, text: " " + here + " "}, kitSeg{text: " "}, kitSeg{style: th.dim, text: fmt.Sprintf("%d elsewhere", hidden)})
	case b.here != "":
		tabs = append(tabs, kitSeg{style: th.dim, text: here}, kitSeg{text: " "}, kitSeg{style: th.tab, text: fmt.Sprintf(" all %d ", total)})
	default:
		tabs = append(tabs, kitSeg{style: th.tab, text: fmt.Sprintf(" all %d ", total)})
	}
	head := []asksRow{segs(tabs...)}
	// Line 2: the status. A problem takes the warning style, and starts with what is wrong.
	status := asksStatusLine(b.snapshot.status, b.snapshot.keyOK, open, now)
	if !b.loaded {
		status = "Loading asks…"
	} else if b.stale {
		status = "Stale: " + status
	}
	statusSeg := kitSeg{style: th.dim, text: " " + status}
	if asksStatusProblem(status) {
		statusSeg = kitSeg{style: sgrBold + th.warn, text: " ! " + status}
	} else if before, _, ok := strings.Cut(status, " ("); ok {
		statusSeg.text = " " + before
	}
	closed, probably := 0, 0
	for _, a := range b.shownMoved() {
		if a.State == state.AskClosed {
			closed++
		} else if a.State == state.AskProbablyDone {
			probably++
		}
	}
	moved := fmt.Sprintf(" %s moved today %d closed, %d probably done", mk.dot, closed, probably)
	if b.w < 70 {
		moved = fmt.Sprintf(" %s moved %d/%d", mk.dot, closed, probably)
	}
	line2 := segs(statusSeg, kitSeg{style: th.dim, text: moved})
	if b.selected == "moved-header" {
		line2 = segs(append([]kitSeg{{style: sgrBold + th.ink, text: mk.selected}}, line2.segs...)...)
		line2.bg = th.selBand
	}
	head = append(head, line2)
	if b.confirmReset {
		scope := "every workspace"
		if b.filtered {
			scope = filepath.Base(b.here)
		}
		confirm := segs(kitSeg{style: sgrBold + th.warn, text: fmt.Sprintf(" ! Erase all asks for %s, for good?", asksText(scope))},
			kitSeg{text: "  "}, kitSeg{style: sgrBold, text: "y"}, kitSeg{text: " erases, other keys keep"})
		if segsCells(confirm.segs) > b.w {
			confirm = segs(kitSeg{style: sgrBold + th.warn, text: fmt.Sprintf(" ! Erase asks for %s?", asksText(scope))},
				kitSeg{text: " "}, kitSeg{style: sgrBold, text: "y"}, kitSeg{text: " erases"})
		}
		confirm.bg, confirm.key = th.warnBand, "confirm"
		head = append(head, confirm)
	}
	if b.message != "" {
		head = append(head, segs(kitSeg{style: sgrBold + th.accent, text: " " + asksText(b.message)}))
	}
	if n := len(b.snapshot.status.NotWatched); n > 0 {
		head = append(head, segs(kitSeg{style: th.dim, text: fmt.Sprintf(" %d not watched: %s, s sessions", n, asksText(b.snapshot.status.NotWatched[0]))}))
	}
	if n := b.snapshot.status.Skipped; n > 0 {
		head = append(head, segs(kitSeg{style: th.dim, text: fmt.Sprintf(" %d texts could not be read", n)}))
	}
	return head
}

// footer is one dim line of keys, cut down by width. tmux already has a bar at the bottom of
// the window, and two bars would read as two apps.
func (b *asksScreen) footer(th kitTheme) []kitSeg {
	scope := "all"
	if !b.filtered {
		scope = "here"
	}
	keys := []kitKey{{"j/k", "move", 0}, {"Tab", "section", 0}}
	switch {
	case b.w < 60:
		keys = append(keys, kitKey{"f", scope, 0}, kitKey{"q", "quit", 0})
	case b.w < asksWideFrom:
		keys = append(keys, kitKey{"Enter", "open", 0}, kitKey{"y", "copy", 0}, kitKey{"d", "dismiss", 0}, kitKey{"f", scope, 0}, kitKey{"q", "quit", 0})
	default:
		keys = append(keys, kitKey{"Enter", "open", 0}, kitKey{"y", "copy", 0}, kitKey{"d", "dismiss", 0}, kitKey{"r", "restore", 0}, kitKey{"u", "undo", 0},
			kitKey{"f", scope, 0}, kitKey{"s", "sessions", 0}, kitKey{"m", "moved", 0}, kitKey{"X", "erase", 0}, kitKey{"q", "quit", 0})
	}
	segs, _ := kitKeys(keys, th.dim, 1)
	return append([]kitSeg{{text: " "}}, segs...)
}

func (b *asksScreen) draw(out *os.File, now time.Time) error {
	b.layout(now)
	th := kitThemeFor(b.noColor)
	head := b.header(now)
	// The question counts as asked only when all of it is on screen.
	b.confirmShown, b.confirmClipped = false, b.confirmReset
	for i, r := range head {
		if r.key == "confirm" && i < b.h-1 && segsCells(r.segs) <= b.w {
			b.confirmShown, b.confirmClipped = true, false
		}
	}
	height := max(b.h-len(head)-1, 1)
	sel, end := 0, 0
	found := false
	for i, r := range b.rows {
		if r.key == b.selected && b.selected != "moved-header" {
			if !found {
				sel, found = i, true
			}
			end = i
		}
	}
	b.top = clamp(b.top, 0, max(len(b.rows)-height, 0))
	if !b.freeScroll && found {
		// Show the whole item when it fits, and its start when it does not.
		if sel < b.top {
			b.top = sel
		} else if end >= b.top+height {
			b.top = min(sel, end-height+1)
		}
		b.top = clamp(b.top, 0, max(len(b.rows)-height, 0))
	}
	rows := append([]asksRow{}, head...)
	for i := b.top; i < len(b.rows) && len(rows) < b.h-1; i++ {
		rows = append(rows, b.rows[i])
	}
	for len(rows) < b.h-1 {
		rows = append(rows, asksRow{})
	}
	rows = append(rows, asksRow{segs: b.footer(th)})
	var text strings.Builder
	text.WriteString("\x1b[H")
	for i, r := range rows[:min(len(rows), b.h)] {
		if i > 0 {
			text.WriteString("\r\n")
		}
		// Clear the row first, so a line a terminal draws narrower than counted leaves nothing
		// of the frame before. Clearing after a full row could take its last cell with it.
		text.WriteString(sgrReset + "\x1b[2K" + kitLine(r.segs, r.bg, b.w))
	}
	_, err := out.WriteString(text.String())
	return err
}

func (b *asksScreen) move(delta int) {
	b.freeScroll = false
	keys := []string{"moved-header"}
	if b.sessions {
		keys = nil
	}
	for _, r := range b.rows {
		if len(keys) == 0 || keys[len(keys)-1] != r.key {
			keys = append(keys, r.key)
		}
	}
	for i, key := range keys {
		if key == b.selected {
			b.selected = keys[clamp(i+delta, 0, len(keys)-1)]
			return
		}
	}
}

// asksCopier is copyText; tests swap it. aiq convo passes its own lifetime as ctx, so a copy
// gives up when the screen is told to stop.
var asksCopier = func(ctx context.Context, text string) (string, error) { return copyText(ctx, text, os.Stdout) }

// copyAsk copies the quote the item shows, as a quote block to answer under, and says how
// that went.
func (b *asksScreen) copyAsk(a state.Ask) string {
	quote := a.FirstQuote
	if b.expanded[a.ID] {
		quote = a.LastQuote
	}
	where, err := asksCopier(context.Background(), quoteForReply(sanitize(quote)))
	if err != nil {
		return fmt.Sprintf("Could not copy #%d: %v", a.ID, err)
	}
	return fmt.Sprintf("Copied #%d to the %s", a.ID, where)
}

// neighbour is the item after the one with this key, else the one before it, else the
// key itself.
func (b *asksScreen) neighbour(key string) string {
	var items []string
	for _, r := range b.rows {
		if r.id != 0 && (len(items) == 0 || items[len(items)-1] != r.key) {
			items = append(items, r.key)
		}
	}
	for i, k := range items {
		if k != key {
			continue
		}
		if i+1 < len(items) {
			return items[i+1]
		}
		if i > 0 {
			return items[i-1]
		}
	}
	return key
}

func (b *asksScreen) action(k keypress) (asksRequest, bool, bool) {
	r := b.current()
	request := asksRequest{closed: b.sections["Closed and dismissed"]}
	if b.confirmReset {
		b.confirmReset = false
		if k.r == 'y' {
			request.action = "reset"
			if b.filtered {
				request.ws = b.here
			}
			return request, true, false
		}
		b.message = "Kept"
		return request, false, false
	}
	switch {
	case k.r == 'X':
		b.confirmReset, b.confirmShown = true, false
	case k.code == keyQuit || k.r == 'q':
		return request, false, true
	case k.code == keyUp:
		b.move(-1)
	case k.code == keyDown:
		b.move(1)
	case k.code == keyPgUp:
		b.top -= max(b.h-4, 1)
		b.freeScroll = true
	case k.code == keyPgDn:
		b.top += max(b.h-4, 1)
		b.freeScroll = true
	case k.r == '\t':
		b.freeScroll = false
		section := r.section
		if a, ok := b.item(r.id); ok {
			for name, list := range asksGroups([]state.Ask{a}, time.Now()) {
				if len(list) > 0 {
					section = name
				}
			}
		}
		for i, name := range asksSections {
			if section == name {
				b.selected = "section:" + asksSections[(i+1)%len(asksSections)]
				return request, false, false
			}
		}
		for _, row := range b.rows {
			if row.section != "" && row.key != b.selected {
				b.selected = row.key
				break
			}
		}
	case k.r == 's':
		b.sessions, b.selected, b.top = !b.sessions, "", 0
		b.freeScroll = false
	case k.r == 'f' && b.here != "":
		b.filtered, b.selected, b.top = !b.filtered, "", 0
		b.freeScroll = false
	case k.code == keyEnter:
		if r.key == "moved-header" {
			b.moved = !b.moved
			return request, false, false
		}
		if r.workspace != "" {
			request.action, request.ws, request.off = "workspace", r.workspace, !b.snapshot.off[r.workspace]
			return request, true, false
		}
		if r.section != "" {
			b.sections[r.section] = !b.sections[r.section]
			request.closed = b.sections["Closed and dismissed"]
			return request, true, false
		}
		if r.id != 0 {
			b.expanded[r.id] = !b.expanded[r.id]
		}
	case k.r == 'u' && b.undo != 0:
		request.action, request.id = "undo", b.undo
		return request, true, false
	case k.r == 'y' || k.r == 'c':
		if a, ok := b.item(r.id); ok {
			b.message = b.copyAsk(a)
		}
	case k.r == 'd' || k.r == 'r':
		if a, ok := b.item(r.id); ok {
			request.id = a.ID
			dismiss, restore := state.AskAction(a.State)
			if k.r == 'd' && dismiss {
				request.action, request.to = "state", state.AskDismissed
			} else if k.r == 'r' && restore {
				request.action, request.to = "state", state.AskOpen
			}
			if request.action == "state" {
				// The item leaves its place; the selection stays where it was, once the
				// request is taken.
				request.next = b.neighbour(r.key)
			}
			return request, request.action != "", false
		}
	}
	return request, false, false
}

type asksInput struct {
	key          keypress
	x, y, button int
	mouse        bool
	paste        bool // the start of pasted text, which is dropped
	alone        bool // the key was all its read held, as a typed key is
}

func segsCells(ss []kitSeg) int {
	n := 0
	for _, sg := range ss {
		n += cells(sg.text)
	}
	return n
}

// input handles one event. While a reset waits for its answer, only a y typed on its own
// after the question was drawn erases; any other key, a y that came with other input, a click
// or a paste keeps everything.
func (b *asksScreen) input(e asksInput, now time.Time) (asksRequest, bool, bool) {
	if b.confirmReset && (e.mouse || e.paste || e.key.r != 'y' || !e.alone || !b.confirmShown) {
		b.message = "Kept"
		if b.confirmClipped {
			b.message = "Kept: the pane is too small to ask. Make it larger to erase"
		}
		b.confirmReset, b.confirmShown, b.confirmClipped = false, false, false
		return asksRequest{}, false, false
	}
	switch {
	case e.paste:
		b.message = "Pasted text does nothing here"
		return asksRequest{}, false, false
	case e.mouse:
		r, send := b.click(e, now)
		return r, send, false
	case e.key.r == 'm':
		b.moved = !b.moved
		return asksRequest{}, false, false
	case e.key.r == 'X' && !e.alone:
		// Like its answer, the question is raised only by a key typed on its own.
		b.message = "Type X by itself to erase"
		return asksRequest{}, false, false
	}
	return b.action(e.key)
}

// take hands a request to the worker, or says why it cannot go now. A request that does not
// go changes nothing, so trying again acts on the same item.
func (b *asksScreen) take(r asksRequest) bool {
	if b.busy {
		b.message = "Refreshing; try again"
		return false
	}
	if b.stale && r.action != "" {
		b.message = "Store unavailable; waiting for refresh"
		return false
	}
	b.busy = true
	if r.next != "" {
		b.selected = r.next
	}
	return true
}

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// asksTokenizer turns input bytes into events. Pasted text, between the bracketed-paste
// marks, is dropped whole, so it can never act as keys.
type asksTokenizer struct {
	pending []byte
	pasting bool
	// afterEscape says a lone Escape was just flushed as the key. What follows it may still
	// be the rest of its sequence, such as a paste mark that came late.
	afterEscape bool
	// typed holds the last bytes taken as typed keys. A paste mark that arrives in pieces,
	// each read on its own long after its Escape, still spells itself out here.
	typed []byte
	// cutPaste says Ctrl-C or Ctrl-D quit in the middle of a paste whose end had not come, or
	// of a key sequence not yet finished (which may be a paste mark), so the rest may still be
	// on its way.
	cutPaste bool
}

// escapeWait is how long a lone Escape waits for the rest of a sequence, as tmux's
// escape-time does, before it counts as the Escape key. For orphanWait after that, input
// that reads as the rest of a sequence is still taken as one, so a paste mark split after
// its Escape and delayed is still a paste.
const (
	escapeWait = 50 * time.Millisecond
	orphanWait = time.Second
)

// forget ends the time after a flushed Escape in which a sequence can still finish.
func (p *asksTokenizer) forget() {
	p.afterEscape = false
}

// waiting says a lone Escape is held for what may follow.
func (p *asksTokenizer) waiting() bool {
	return !p.pasting && len(p.pending) == 1 && p.pending[0] == 0x1b
}

// flush turns a held lone Escape into the Escape key.
func (p *asksTokenizer) flush() []asksInput {
	if !p.waiting() {
		return nil
	}
	p.pending, p.afterEscape = nil, true
	return []asksInput{{key: keypress{code: keyBack}}}
}

// Buffer partial CSI/SGR sequences. Legacy mouse reports are consumed and ignored,
// including their payload bytes, which otherwise could become dismiss/quit keys.
func (p *asksTokenizer) feed(buf []byte) []asksInput {
	// A key typed reaches the screen as a read of its own; pasted or replayed text does not.
	alone := len(p.pending) == 0 && !p.pasting && !p.afterEscape && len(buf) == 1
	out := p.tokens(buf)
	if alone && len(out) == 1 {
		out[0].alone = true
	}
	return out
}

func (p *asksTokenizer) tokens(buf []byte) []asksInput {
	if p.afterEscape && len(p.pending) == 0 && len(buf) > 0 && (buf[0] == '[' || buf[0] == 'O') {
		p.pending = []byte{0x1b}
	}
	p.afterEscape = false
	p.pending = append(p.pending, buf...)
	var out []asksInput
	for len(p.pending) > 0 {
		b := p.pending
		if p.pasting {
			end := bytes.Index(b, pasteEnd)
			// Ctrl-C still quits, so a paste whose end mark never comes cannot hold the screen.
			if c := bytes.IndexByte(b, 0x03); c >= 0 && (end < 0 || c < end) {
				p.pending, p.pasting, p.cutPaste = nil, false, end < 0
				return append(out, asksInput{key: keypress{code: keyQuit}})
			}
			if end < 0 {
				// Keep only what could be the start of the end mark.
				keep := min(len(b), len(pasteEnd)-1)
				p.pending = append([]byte(nil), b[len(b)-keep:]...)
				break
			}
			p.pending, p.pasting = b[end+len(pasteEnd):], false
			continue
		}
		if bytes.HasPrefix(b, pasteStart) {
			p.pending, p.pasting = b[len(pasteStart):], true
			out = append(out, asksInput{paste: true})
			continue
		}
		// A paste mark that lost its Escape, as when the Escape came long before, still
		// starts a paste: no one types it.
		if bytes.HasPrefix(b, pasteStart[1:]) {
			p.pending, p.pasting = b[len(pasteStart)-1:], true
			out = append(out, asksInput{paste: true})
			continue
		}
		if bytes.HasPrefix(b, pasteEnd) {
			p.pending = b[len(pasteEnd):]
			continue
		}
		if b[0] == 0x1b {
			if len(b) < 2 {
				// It may start a sequence split across reads, such as a paste mark, so it waits:
				// flush makes it the Escape key once nothing has followed for escapeWait.
				break
			}
			if b[1] == '[' || b[1] == 'O' {
				if len(b) >= 3 && b[1] == '[' && b[2] == 'M' {
					if len(b) < 6 {
						break
					}
					p.pending = b[6:]
					continue
				}
				i := 2
				sgrMouse := len(b) >= 3 && b[1] == '[' && b[2] == '<'
				if sgrMouse {
					for i < len(b) && b[i] != 'M' && b[i] != 'm' {
						if b[i] == 3 || b[i] == 4 {
							p.pending = nil
							return append(out, asksInput{key: keypress{code: keyQuit}})
						}
						i++
					}
				} else {
					for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
						// Ctrl-C or Ctrl-D in a sequence that never finished still quits, as
						// in a mouse report; nothing of the sequence becomes a key. It may
						// have been a paste mark, whose rest may still come.
						if b[i] == 3 || b[i] == 4 {
							p.pending, p.cutPaste = nil, true
							return append(out, asksInput{key: keypress{code: keyQuit}})
						}
						i++
					}
				}
				if i == len(b) {
					if len(b) > 128 {
						p.pending = nil
					}
					break
				}
				seq := b[:i+1]
				if sgrMouse {
					parts := strings.Split(string(b[3:i]), ";")
					if len(parts) == 3 && b[i] == 'M' {
						button, e1 := strconv.Atoi(parts[0])
						x, e2 := strconv.Atoi(parts[1])
						y, e3 := strconv.Atoi(parts[2])
						// Every press counts, so any click can cancel a question; only a left
						// click and the wheel act on the screen. Motion reports (32) are not presses.
						if e1 == nil && e2 == nil && e3 == nil && x > 0 && y > 0 && button&32 == 0 {
							out = append(out, asksInput{mouse: true, button: button, x: x, y: y})
						}
					}
				} else {
					keys := parseKeys(seq)
					for _, k := range keys {
						out = append(out, asksInput{key: k})
					}
					// A key this screen does not know, such as Delete or F1, is still a key: it
					// does nothing, and keeps a pending reset.
					if len(keys) == 0 {
						out = append(out, asksInput{key: keypress{}})
					}
				}
				p.pending = b[i+1:]
				continue
			}
			// Escape and then another key, such as Escape q typed quickly: both count.
			out = append(out, asksInput{key: keypress{code: keyBack}})
			p.pending = b[1:]
			continue
		}
		if !utf8.FullRune(b) {
			break
		}
		_, n := utf8.DecodeRune(b)
		for _, k := range parseKeys(b[:n]) {
			// The resume browser uses g for Home. Here it is a plain key, which does nothing.
			if b[0] == 'g' {
				k = keypress{r: 'g'}
			}
			out = append(out, asksInput{key: k})
		}
		p.pending = b[n:]
		p.typed = append(p.typed, b[:n]...)
		if len(p.typed) > len(pasteStart)-1 {
			p.typed = p.typed[len(p.typed)-(len(pasteStart)-1):]
		}
		if bytes.Equal(p.typed, pasteStart[1:]) {
			p.typed, p.pasting = nil, true
			out = append(out, asksInput{paste: true})
		}
	}
	return out
}

func (b *asksScreen) click(e asksInput, now time.Time) (asksRequest, bool) {
	if e.button != 0 && e.button != 64 && e.button != 65 {
		return asksRequest{}, false
	}
	if e.button == 64 || e.button == 65 {
		b.top += map[bool]int{true: -3, false: 3}[e.button == 64]
		b.freeScroll = true
		return asksRequest{}, false
	}
	if e.x > b.w || e.y > b.h || e.y < 1 {
		return asksRequest{}, false
	}
	if e.y == 1 {
		b.action(keypress{r: 'f'})
		return asksRequest{}, false
	}
	if e.y == 2 {
		b.moved = !b.moved
		return asksRequest{}, false
	}
	idx := b.top + e.y - len(b.header(now)) - 1
	if idx < 0 || idx >= len(b.rows) || e.y == b.h {
		return asksRequest{}, false
	}
	r := b.rows[idx]
	if b.selected != r.key {
		b.selected = r.key
		b.freeScroll = false
		return asksRequest{}, false
	}
	k := keypress{code: keyEnter}
	if r.action == "keys" {
		// On the keys row only a key does anything, so a stray click cannot close the quote.
		hit := false
		for _, h := range r.hits {
			if e.x-1 >= h.from && e.x-1 < h.to {
				hit = true
				if h.r != 0 {
					k = keypress{r: h.r}
				}
			}
		}
		if !hit {
			return asksRequest{}, false
		}
	}
	request, submit, _ := b.action(k)
	return request, submit
}

const asksTerminalEnter = "\x1b[?1049h\x1b[?25l\x1b[?2004h"
const asksMouseEnter = "\x1b[?1000h\x1b[?1006h"
const asksTerminalLeave = "\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l"

func runAsksTUI(reg, leases *state.Store) error {
	// Before the screen takes the terminal, because the answer comes back over it. A signal
	// during the query stops it, so the query puts the terminal back before aiq exits.
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done, gone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		select {
		case <-sig:
			close(stop)
		case <-done:
		}
	}()
	pal = choosePalette(themeChoice("", os.Getenv("AIQ_THEME"), os.Stderr), os.Getenv("TMUX_PANE"), stop)
	close(done)
	<-gone
	signal.Stop(sig)
	select {
	case <-stop:
		return nil
	default:
	}
	return runAsksScreen(reg, leases, os.Stdin, os.Stdout, !hasFlag(os.Args[1:], "--no-mouse"))
}

func runAsksScreen(reg, leases *state.Store, in, out *os.File, mouse bool) error {
	here, err := asksHereDir()
	explicit := hasFlag(os.Args[1:], "--here")
	if err != nil && explicit {
		return err
	}
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return err
	}
	var once sync.Once
	restored := make(chan struct{})
	restore := func() {
		once.Do(func() {
			go func() {
				defer close(restored)
				// Restore input mode first, even if a blocked output cannot accept escapes.
				term.Restore(int(in.Fd()), old)
				out.WriteString(asksTerminalLeave)
			}()
		})
		select {
		case <-restored:
		case <-time.After(time.Second):
		}
	}
	defer restore()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	shutdown, signalDone := make(chan struct{}), make(chan struct{})
	defer close(signalDone)
	go func() {
		select {
		case <-signals:
			close(shutdown)
			restore()
		case <-signalDone:
		}
	}()
	if _, err := out.WriteString(asksTerminalEnter); err != nil {
		return err
	}
	if mouse {
		if _, err := out.WriteString(asksMouseEnter); err != nil {
			return err
		}
	}
	b := newAsksScreen()
	// With no directory to go by, the app shows everything and f does nothing.
	b.here = here
	b.filtered = here != "" && (explicit || asksStartFiltered(leases, here, hasFlag(os.Args[1:], "--all")))
	resize := func() {
		b.w, b.h, err = term.GetSize(int(out.Fd()))
		if err != nil || b.w < 1 || b.h < 1 {
			b.w, b.h = 80, 24
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	keys, stopKeys := readKeys(int(in.Fd()))
	defer stopKeys()
	requests, results := make(chan asksRequest, 1), make(chan asksResult, 1)
	workerStop := make(chan struct{})
	defer close(workerStop)
	go asksWorker(reg, leases, requests, results, workerStop)
	submit := func(r asksRequest) {
		if b.take(r) {
			requests <- r
		}
	}
	// finish waits for a request already taken, such as a dismiss just before q, to land.
	finish := func() { asksDrain(b.busy, results, 5*time.Second) }
	var tokenizer asksTokenizer
	dirty := true
	// handle acts on input events and says whether to quit.
	handle := func(events []asksInput) bool {
		for _, e := range events {
			dirty = true
			if e.mouse && !mouse {
				continue
			}
			r, send, quit := b.input(e, time.Now())
			if quit {
				return true
			}
			if send {
				submit(r)
			}
			b.layout(time.Now())
		}
		return false
	}
	var escape, orphan <-chan time.Time
	submit(asksRequest{})
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	ageStamp := ""
	for {
		if dirty {
			if err := b.draw(out, time.Now()); err != nil {
				return err
			}
			dirty = false
		}
		select {
		case <-shutdown:
			finish()
			return nil
		case <-winch:
			resize()
			dirty = true
		case <-tick.C:
			now := time.Now()
			stamp := asksStatusLine(b.snapshot.status, b.snapshot.keyOK, len(b.shownItems()), now)
			for _, a := range b.shownItems() {
				stamp += fmtAge(max(now.Sub(time.Unix(a.RaisedAt, 0)), 0)) + ";"
			}
			if stamp != ageStamp {
				ageStamp, dirty = stamp, true
			}
			if !b.busy {
				submit(asksRequest{closed: b.sections["Closed and dismissed"]})
			}
		case result := <-results:
			wasLoaded := b.loaded
			b.busy, b.loaded = false, true
			if !wasLoaded {
				dirty = true
			}
			if result.message != "" {
				b.message, dirty = result.message, true
			}
			if result.undo != 0 || result.clearUndo {
				b.undo = result.undo
			}
			if result.err != nil {
				b.message, b.stale, dirty = "Store error: "+asksText(result.err.Error())+" "+result.message, true, true
				continue
			}
			b.stale = false
			if !reflect.DeepEqual(b.snapshot, result.snapshot) {
				b.snapshot = result.snapshot
				dirty = true
			}
			if result.message == "" && (strings.HasPrefix(b.message, "Store error:") || strings.HasPrefix(b.message, "Store unavailable;")) {
				b.message = ""
				dirty = true
			}
		case <-escape:
			escape = nil
			if handle(tokenizer.flush()) {
				finish()
				return nil
			}
			orphan = time.After(orphanWait)
		case <-orphan:
			orphan = nil
			tokenizer.forget()
		case data, ok := <-keys:
			if !ok {
				finish()
				return nil
			}
			if handle(tokenizer.feed(data)) {
				finish()
				return nil
			}
			escape, orphan = nil, nil
			if tokenizer.waiting() {
				escape = time.After(escapeWait)
			}
		}
	}
}
