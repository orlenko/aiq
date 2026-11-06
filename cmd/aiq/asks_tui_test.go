package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/orlenko/aiq/internal/state"
)

func TestAsksGroupingAndNoPreviewCap(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		a    state.Ask
		want string
	}{
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix() - 48*3600, AsksReader: .6}, "Asks"},
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix() - 48*3600 - 1, AsksReader: .6}, "Other open"},
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .599, Risk: .8}, "Warnings"},
		{state.Ask{State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .6, Risk: .8}, "Asks"},
		{state.Ask{State: state.AskProbablyDone}, "Probably done"},
		{state.Ask{State: state.AskClosed}, "Closed and dismissed"},
		{state.Ask{State: state.AskDismissed}, "Closed and dismissed"},
	} {
		if got := asksGroups([]state.Ask{tc.a}, now); len(got[tc.want]) != 1 {
			t.Errorf("%+v grouped as %v, want %s", tc.a, got, tc.want)
		}
	}
	b := newAsksScreen()
	for i := int64(1); i <= 12; i++ {
		b.snapshot.items = append(b.snapshot.items, state.Ask{ID: i, State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9})
	}
	b.layout(now)
	seen := map[int64]bool{}
	for _, r := range b.rows {
		if r.id != 0 {
			seen[r.id] = true
		}
	}
	if len(seen) != 12 || b.sections["Other open"] || b.sections["Probably done"] || b.sections["Closed and dismissed"] {
		t.Fatalf("visible items %d, section defaults %v", len(seen), b.sections)
	}
}

func TestAsksTokenizerFragmentedAndRejectedMouse(t *testing.T) {
	var p asksTokenizer
	if got := p.feed([]byte("\x1b[<0;7")); len(got) != 0 {
		t.Fatal(got)
	}
	got := p.feed([]byte(";6M\x1b[<0;7;6m\x1b[<32;7;6M\x1b[<64;1;1M\x1b[A"))
	want := []asksInput{{mouse: true, x: 7, y: 6}, {mouse: true, button: 64, x: 1, y: 1}, {key: keypress{code: keyUp}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// Legacy payload contains action keys; none may be interpreted as a key.
	if got := p.feed([]byte("\x1b[Mdrq")); len(got) != 0 {
		t.Fatalf("legacy mouse became input: %+v", got)
	}
	if got := p.feed([]byte("\x1b[<x;3;4M\x1b[<0;-1;2M")); len(got) != 0 {
		t.Fatal(got)
	}
	if got := p.feed([]byte{'\xc3'}); len(got) != 0 {
		t.Fatal(got)
	}
	got = p.feed([]byte{'\xa9', 'g', '\t'})
	if len(got) != 3 || got[0].key.r != 'é' || got[1].key.r != 'g' || got[2].key.r != '\t' {
		t.Fatal(got)
	}
	p.feed([]byte("\x1b[" + strings.Repeat("1", 200)))
	if len(p.pending) > 128 {
		t.Fatal("unbounded partial sequence")
	}
	p.feed([]byte("\x1b[<0;"))
	if got := p.feed([]byte{3}); len(got) != 1 || got[0].key.code != keyQuit {
		t.Fatal("partial mouse report swallowed Ctrl-C")
	}
}

func TestAsksRowsActionsAndMovedHeader(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.loaded = 50, 20, true
	now := time.Now()
	b.snapshot.items = []state.Ask{{ID: 12, Workspace: "/example/project", Provider: "claude", State: state.AskOpen,
		RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: strings.Repeat("some words ", 30), LastQuote: "latest quote"}}
	b.snapshot.moved = []state.Ask{{ID: 13, Workspace: "/another/project"}}
	if name := b.workspaceName("/example/project"); name != "example/project" {
		t.Fatalf("moved-item collision hidden: %q", name)
	}
	b.layout(now)
	for _, r := range b.rows {
		if r.action == "keys" {
			t.Fatal("keys shown on unselected item")
		}
	}
	b.selected = "ask:12"
	b.layout(now)
	quotes := 0
	for _, r := range b.rows {
		if strings.HasPrefix(r.text, "    ") && r.action != "keys" {
			quotes++
		}
	}
	if quotes < 8 {
		t.Fatalf("a quote must wrap in full, got %d rows", quotes)
	}
	if _, send, _ := b.action(keypress{r: 'r'}); send {
		t.Fatal("open item allowed restore")
	}
	if req, send, _ := b.action(keypress{r: 'd'}); !send || req.id != 12 || req.to != state.AskDismissed {
		t.Fatalf("dismiss: %+v, %v", req, send)
	}
	b.action(keypress{r: '\t'})
	if b.selected != "section:Warnings" {
		t.Fatal(b.selected)
	}
	b.selected = "moved-header"
	b.action(keypress{code: keyEnter})
	if !b.moved {
		t.Fatal("keyboard cannot open moved items")
	}
	b.message = "Dismissed #12. u undo"
	head := b.header(now)
	if !strings.Contains(head[0].text, "aiq asks") || !strings.Contains(head[1].text, "! Off:") || !strings.Contains(head[1].text, "moved") ||
		!strings.Contains(head[2].text, b.message) {
		t.Fatal(head)
	}
	if !strings.HasPrefix(head[1].text, b.marks().selected) {
		t.Fatalf("moved line not marked as selected: %q", head[1].text)
	}
}

func TestAsksStateChangesRefreshAndSessions(t *testing.T) {
	reg, leases := asksTestStores(t)
	now := time.Now()
	asksSeed(t, reg, state.Ask{Workspace: "/example/project", Provider: "claude", SessionID: "example-session", Chain: "example",
		RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Please check the example build"})
	first := asksApply(reg, leases, asksRequest{}, now)
	if first.err != nil || len(first.snapshot.items) != 1 {
		t.Fatalf("load: err=%v, items=%d", first.err, len(first.snapshot.items))
	}
	id := first.snapshot.items[0].ID
	result := asksApply(reg, leases, asksRequest{action: "state", id: id, to: state.AskDismissed}, now)
	if result.err != nil || result.message != fmt.Sprintf("Dismissed #%d. u undo", id) || len(result.snapshot.items) != 0 {
		t.Fatalf("dismiss: err=%v, message=%q, items=%d", result.err, result.message, len(result.snapshot.items))
	}
	result = asksApply(reg, leases, asksRequest{action: "undo", id: id}, now)
	if result.err != nil || len(result.snapshot.items) != 1 || result.snapshot.items[0].State != state.AskOpen {
		t.Fatalf("undo: err=%v, items=%d", result.err, len(result.snapshot.items))
	}
	if r := asksApply(reg, leases, asksRequest{action: "state", id: id, to: state.AskOpen}, now); r.err == nil {
		t.Fatal("invalid restore accepted")
	}
	// Another connection can update the registry between UI refreshes.
	other, err := state.Open(filepath.Join(os.Getenv("AIQ_DATA_DIR"), "asks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.SetAskByPerson(id, state.AskDismissed, now.Unix()); err != nil {
		t.Fatal(err)
	}
	result = asksApply(reg, leases, asksRequest{closed: true}, now)
	if result.err != nil || result.snapshot.items[0].State != state.AskDismissed {
		t.Fatalf("refresh: err=%v", result.err)
	}
	ws := "/example/project"
	result = asksApply(reg, leases, asksRequest{action: "workspace", ws: ws, off: true}, now)
	if result.err != nil || !result.snapshot.off[ws] {
		t.Fatalf("workspace: err=%v, off=%v", result.err, result.snapshot.off[ws])
	}
	b := newAsksScreen()
	b.snapshot, b.sessions = result.snapshot, true
	b.layout(now)
	if len(b.rows) < 2 || b.rows[1].workspace != ws || !strings.Contains(b.rows[1].text, "off") {
		t.Fatal(b.rows)
	}
}

func TestAsksRenderSanitizesAndAnchors(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.noColor, b.loaded = 40, 16, true, true
	now := time.Now()
	for i := int64(1); i <= 8; i++ {
		b.snapshot.items = append(b.snapshot.items, state.Ask{ID: i, State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9,
			Workspace: "/example/\x1b]52;clipboard\a", Provider: "claude\x1b[2J", FirstQuote: "please\rcheck\u202e now", LastQuote: "last\x1b[Hquote"})
	}
	b.selected = "ask:4"
	b.layout(now)
	b.top = 8
	oldOffset := 0
	for i, r := range b.rows {
		if r.key == b.selected {
			oldOffset = i - b.top
			break
		}
	}
	b.snapshot.items = append([]state.Ask{{ID: 99, State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9}}, b.snapshot.items...)
	b.layout(now)
	checked := false
	for i, r := range b.rows {
		if r.key == b.selected && !checked {
			if i-b.top != oldOffset {
				t.Fatal("refresh shifted the selected item")
			}
			checked = true
		}
		for _, c := range r.text {
			if unicode.IsControl(c) || c == '\u202e' {
				t.Fatalf("untrusted control rendered: %q", r.text)
			}
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "screen")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := b.draw(f, now); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(f.Name())
	if strings.Contains(string(raw), "38;5") || strings.Contains(string(raw), "48;5") {
		t.Fatal("NO_COLOR screen contains colour")
	}
	if !strings.Contains(string(raw), sgrBold) {
		t.Fatal("NO_COLOR keeps bold, so selection and titles still read")
	}
	plain := kitPlain(strings.NewReplacer("\x1b[H", "", "\x1b[2K", "", "\x1b[K", "", "\x1b[J", "").Replace(string(raw)))
	if strings.ContainsRune(plain, 0x1b) {
		t.Fatalf("an escape other than SGR reached the screen: %q", plain)
	}
	for _, row := range strings.Split(plain, "\r\n") {
		if cells(row) != 40 {
			t.Fatalf("row has %d cells, want every row to fill 40: %q", cells(row), row)
		}
	}
}

func TestAsksExpandedQuoteCanScroll(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.loaded = 40, 12, true
	b.snapshot.items = []state.Ask{{ID: 1, State: state.AskOpen, RaisedAt: time.Now().Unix(), AsksReader: .9,
		LastQuote: strings.Repeat("read this invented long quote ", 100)}}
	b.selected, b.expanded[1] = "ask:1", true
	b.layout(time.Now())
	b.action(keypress{code: keyPgDn})
	f, err := os.CreateTemp(t.TempDir(), "scrolled")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := b.draw(f, time.Now()); err != nil {
		t.Fatal(err)
	}
	if b.top < 4 || b.selected != "ask:1" {
		t.Fatal("long quote scrolled back to its first line")
	}
	top := b.top
	b.click(asksInput{mouse: true, button: 65}, time.Now())
	if b.top != top+3 || b.selected != "ask:1" {
		t.Fatal("mouse wheel changed the selected item instead of scrolling")
	}
}

// Run this opt-in helper under a disposable pseudo-terminal to exercise the real
// input/refresh/restore loop. It seeds invented rows and never starts a watcher.
func TestAsksTUIPreviewHelper(t *testing.T) {
	if os.Getenv("AIQ_ASKS_TUI_HELPER") != "1" {
		t.Skip("opt-in terminal preview helper")
	}
	reg, leases := asksTestStores(t)
	now := time.Now()
	asksSeed(t, reg,
		state.Ask{Chain: "example", Workspace: "/example/project", Provider: "claude", SessionID: "example-session", RaisedAt: now.Unix(), AsksReader: .9,
			FirstQuote: "Please check the example build before proceeding. This invented quote wraps in a narrow pane.", LastQuote: "The example build needs your check."},
		state.Ask{Chain: "example", Workspace: "/example/older", Provider: "codex", SessionID: "other-example", State: state.AskClosed, StateReason: "reply", StateText: "The invented reply closed this item", StateAt: now.Unix(), RaisedAt: now.Add(-time.Hour).Unix(), FirstQuote: "Is the example ready?"})
	if err := runAsksScreen(reg, leases, os.Stdin, os.Stdout, os.Getenv("AIQ_ASKS_TEST_NO_MOUSE") != "1"); err != nil {
		t.Fatal(err)
	}
}

func TestAsksFilterKeyAndHiddenCount(t *testing.T) {
	b := newAsksScreen()
	now := time.Now()
	b.loaded = true
	b.here, b.filtered = "/w/alpha", true
	b.snapshot.items = []state.Ask{
		{ID: 1, State: state.AskOpen, Workspace: "/w/alpha", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "mine"},
		{ID: 2, State: state.AskOpen, Workspace: "/w/beta", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "theirs"},
	}
	text := func() string {
		b.layout(now)
		out := ""
		for _, r := range b.rows {
			out += r.text + "\n"
		}
		for _, h := range b.header(now) {
			out += h.text + "\n"
		}
		return out
	}
	got := text()
	if !strings.Contains(got, "mine") || strings.Contains(got, "theirs") {
		t.Fatalf("filtered:\n%s", got)
	}
	if !strings.Contains(got, "1 elsewhere") {
		t.Fatalf("hidden asks must be counted:\n%s", got)
	}
	b.action(keypress{r: 'f'})
	got = text()
	if !strings.Contains(got, "mine") || !strings.Contains(got, "theirs") || strings.Contains(got, "elsewhere") {
		t.Fatalf("all:\n%s", got)
	}
}

func TestAsksResetNeedsConfirmation(t *testing.T) {
	b := newAsksScreen()
	b.here, b.filtered = "/w/alpha", true
	if _, send, _ := b.action(keypress{r: 'X'}); send || !b.confirmReset {
		t.Fatal("X must only ask")
	}
	if _, send, _ := b.action(keypress{r: 'n'}); send || b.confirmReset {
		t.Fatal("another key must keep")
	}
	b.action(keypress{r: 'X'})
	req, send, _ := b.action(keypress{r: 'y'})
	if !send || req.action != "reset" || req.ws != "/w/alpha" {
		t.Fatalf("%+v %v", req, send)
	}
	b.filtered = false
	b.action(keypress{r: 'X'})
	if req, _, _ := b.action(keypress{r: 'y'}); req.ws != "" {
		t.Fatalf("with the filter off a reset covers everything: %+v", req)
	}
}

func TestAsksDesignRules(t *testing.T) {
	saved := pal
	defer func() { pal = saved }()
	pal = lightPalette
	now := time.Now()
	b := newAsksScreen()
	b.noColor = false
	b.w, b.h, b.loaded, b.here, b.filtered = 50, 40, true, "/w/harbor", true
	b.snapshot.keyOK = true
	b.snapshot.status = state.AsksStatus{PassAt: now.Unix(), Watched: 3}
	long := strings.Repeat("pick the release window for this invented change ", 6)
	b.snapshot.items = []state.Ask{
		{ID: 1, State: state.AskOpen, Workspace: "/w/harbor", Provider: "claude", RaisedAt: now.Unix(), AsksReader: .9,
			FirstQuote: long + "see [the notes](https://example.com/n) and `make check` **now**"},
		{ID: 2, State: state.AskOpen, Workspace: "/w/lantern", Provider: "codex", RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "second"},
		{ID: 3, State: state.AskOpen, Workspace: "/w/harbor", Provider: "codex", RaisedAt: now.Unix(), Risk: .9, AsksReader: .1, FirstQuote: "careful"},
	}
	th := kitThemeFor(false)
	row := func(id int64) []asksRow {
		var out []asksRow
		for _, r := range b.rows {
			if r.id == id {
				out = append(out, r)
			}
		}
		return out
	}
	b.selected = "section:Asks"
	b.layout(now)

	// The workspace shows only when more than one can be on screen.
	all := ""
	for _, r := range row(1) {
		all += r.text + "\n"
	}
	first := row(1)[0]
	if strings.Contains(first.text, "harbor") || first.bg != th.itemBand {
		t.Fatalf("first row: %+v", first)
	}
	if row(1)[1].bg != "" {
		t.Fatal("only an item's first row sits on the band")
	}
	// Full text, with Markdown rendered and the link down to its words.
	for _, want := range []string{"the notes", "make check", "now"} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing %q in\n%s", want, all)
		}
	}
	for _, not := range []string{"**", "https://", "](", "…"} {
		if strings.Contains(all, not) {
			t.Fatalf("%q shown in\n%s", not, all)
		}
	}

	// Selection: a glyph, the band on every row, and the keys under it.
	b.selected = "ask:1"
	b.layout(now)
	rows := row(1)
	if !strings.HasPrefix(rows[0].text, "▶ ") {
		t.Fatalf("selected: %q", rows[0].text)
	}
	for _, r := range rows {
		if r.bg != th.selBand {
			t.Fatalf("selected row without the band: %q", r.text)
		}
	}
	last := rows[len(rows)-1]
	if last.action != "keys" || !strings.Contains(last.text, "d dismiss") {
		t.Fatalf("keys row: %+v", last)
	}

	// A click acts on the key under it, and only there.
	y := 0
	for i, r := range b.rows {
		if r.action == "keys" {
			y = i + len(b.header(now)) + 1
		}
	}
	b.top = 0
	if _, send := b.click(asksInput{mouse: true, x: 2, y: y}, now); send {
		t.Fatal("a click in the margin acted")
	}
	req, send := b.click(asksInput{mouse: true, x: last.hits[0].from + 1, y: y}, now)
	if !send || req.to != state.AskDismissed {
		t.Fatalf("click on d: %+v %v", req, send)
	}

	// A warning says so in words and keeps its live mark.
	b.selected = "section:Asks"
	b.layout(now)
	if w := row(3)[0]; !strings.Contains(w.text, "risky") || !strings.Contains(w.text, "○") {
		t.Fatalf("warning: %q", w.text)
	}

	// Section bars reach the edge, open or closed.
	for _, r := range b.rows {
		if r.section != "" && cells(kitText(r.segs)) != b.w {
			t.Fatalf("bar is %d cells: %q", cells(kitText(r.segs)), kitText(r.segs))
		}
	}

	// From 96 columns the details are a column beside the quote.
	b.w = 120
	b.layout(now)
	rows = row(1)
	col := cells(rows[1].text) - cells(strings.TrimLeft(rows[1].text, " "))
	if cut := cells(rows[0].text[:strings.Index(rows[0].text, "pick")]); col <= 4 || cut != col {
		t.Fatalf("quote columns differ: %d and %d\n%s\n%s", cut, col, rows[0].text, rows[1].text)
	}
}

func TestAsksDismissSelectsTheNextItem(t *testing.T) {
	b := newAsksScreen()
	b.w, b.h, b.loaded = 60, 40, true
	now := time.Now()
	for id := int64(1); id <= 3; id++ {
		b.snapshot.items = append(b.snapshot.items, state.Ask{ID: id, Workspace: "/example/project", Provider: "claude",
			State: state.AskOpen, RaisedAt: now.Unix() - id, AsksReader: .9, FirstQuote: fmt.Sprintf("question %d", id)})
	}
	b.layout(now)
	var order []string
	for _, r := range b.rows {
		if r.id != 0 && (len(order) == 0 || order[len(order)-1] != r.key) {
			order = append(order, r.key)
		}
	}
	if len(order) != 3 {
		t.Fatalf("items: %v", order)
	}
	b.selected = order[1]
	b.layout(now)
	req, send, _ := b.action(keypress{r: 'd'})
	if !send || req.to != state.AskDismissed || b.selected != order[1] {
		t.Fatalf("the selection moved before the request was taken: %q", b.selected)
	}
	// A request that cannot go now leaves the selection on the item it was for.
	b.busy = true
	if b.take(req) || b.selected != order[1] {
		t.Fatalf("a refused request moved the selection to %q", b.selected)
	}
	b.busy = false
	if !b.take(req) || b.selected != order[2] {
		t.Fatalf("after dismissing the middle item: selected %q, want %q", b.selected, order[2])
	}
	b.busy = false
	b.selected = order[2]
	b.layout(now)
	if req, _, _ := b.action(keypress{r: 'd'}); !b.take(req) || b.selected != order[1] {
		t.Fatalf("after dismissing the last item: selected %q, want %q", b.selected, order[1])
	}
}

// y, or c, copies the quote the item shows, as a quote block to answer under.
func TestAsksCopyTakesTheShownQuote(t *testing.T) {
	var got []string
	saved := asksCopier
	asksCopier = func(ctx context.Context, text string) (string, error) {
		got = append(got, text)
		return "clipboard", nil
	}
	t.Cleanup(func() { asksCopier = saved })
	b := newAsksScreen()
	b.w, b.h, b.loaded = 60, 40, true
	now := time.Now()
	b.snapshot.items = []state.Ask{{ID: 7, Workspace: "/example/project", Provider: "claude", State: state.AskOpen,
		RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "Should I merge it?\nOr wait for CI?", LastQuote: "Still waiting: merge?"}}
	b.layout(now)
	b.selected = "ask:7"
	b.layout(now)
	b.action(keypress{r: 'y'})
	b.expanded[7] = true
	b.action(keypress{r: 'c'})
	if len(got) != 2 || got[0] != "> Should I merge it?\n> Or wait for CI?\n\n" || got[1] != "> Still waiting: merge?\n\n" {
		t.Fatalf("copied %q", got)
	}
	if b.message != "Copied #7 to the clipboard" {
		t.Fatalf("message %q", b.message)
	}
}

func TestQuoteForReplyKeepsBlankLinesQuoted(t *testing.T) {
	if got := quoteForReply("one\n\ntwo\n"); got != "> one\n>\n> two\n\n" {
		t.Fatalf("%q", got)
	}
}

// Only a y typed after the question is on screen erases. Another key, a click, a paste, or
// a y that came in with the X keeps everything.
func TestAsksResetAnswersOnlyAShownQuestion(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	screen := func() *asksScreen {
		b := newAsksScreen()
		b.w, b.h, b.loaded = 60, 20, true
		b.snapshot.items = []state.Ask{{ID: 1, Workspace: "/example/project", State: state.AskOpen, RaisedAt: now.Unix(), AsksReader: .9, FirstQuote: "q"}}
		return b
	}
	key := func(r rune) asksInput { return asksInput{key: keypress{r: r}} }
	for name, after := range map[string]asksInput{
		"m":           key('m'),
		"down":        {key: keypress{code: keyDown}},
		"click":       {mouse: true, button: 0, x: 1, y: 5},
		"right click": {mouse: true, button: 2, x: 1, y: 5},
		"paste":       {paste: true},
	} {
		b := screen()
		b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
		b.draw(devnull, now)
		b.input(after, now)
		b.draw(devnull, now)
		if req, send, _ := b.input(key('y'), now); send || req.action == "reset" {
			t.Errorf("X, %s, y erased", name)
		}
	}
	b := screen()
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	if req, send, _ := b.input(key('y'), now); send || req.action == "reset" {
		t.Error("a y that came with the X erased before the question was drawn")
	}
	b = screen()
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	b.draw(devnull, now)
	if req, send, _ := b.input(key('y'), now); send || req.action == "reset" {
		t.Error("a y that came with other input erased")
	}
	// Only a y typed on its own, as a read of its own, answers.
	var typed asksTokenizer
	b = screen()
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	b.draw(devnull, now)
	if ev := typed.feed([]byte("y")); len(ev) != 1 || !ev[0].alone {
		t.Fatalf("a typed y: %+v", ev)
	} else if req, send, _ := b.input(ev[0], now); !send || req.action != "reset" {
		t.Error("X, then y once asked, must erase")
	}
	if ev := typed.feed([]byte("zy")); len(ev) != 2 || ev[1].alone {
		t.Fatalf("a y read with another key: %+v", ev)
	}
	var tk asksTokenizer
	events := tk.feed([]byte("\x1b[200~Xy"))
	events = append(events, tk.feed([]byte("dq\x1b[20"))...)
	events = append(events, tk.feed([]byte("1~z"))...)
	if len(events) != 2 || !events[0].paste || events[1].key.r != 'z' {
		t.Fatalf("pasted text reached the keys: %+v", events)
	}
}

// A clipboard tool that hangs is given up on, so the screen goes on.
func TestCopyGivesUpOnAHungTool(t *testing.T) {
	dir := t.TempDir()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep tool")
	}
	// Each tool notes that it started, then hangs, so the test sees the copy wait and give up.
	mark := filepath.Join(t.TempDir(), "started")
	script := "#!/bin/sh\necho started >> " + mark + "\nexec " + sleep + " 30\n"
	for _, name := range []string{"pbcopy", "wl-copy", "xclip"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMUX", "")
	t.Setenv("SSH_CONNECTION", "")
	saved := copyTimeout
	// Long enough for each tool's shell to start on a loaded machine.
	copyTimeout = time.Second
	defer func() { copyTimeout = saved }()
	start := time.Now()
	if _, err := copyText(context.Background(), "example", nil); err == nil {
		t.Fatal("a hung tool counted as a copy")
	}
	d := time.Since(start)
	if d > 3*copyTimeout+2*time.Second {
		t.Fatalf("copy took %s", d)
	}
	if data, _ := os.ReadFile(mark); !strings.Contains(string(data), "started") || d < copyTimeout {
		t.Fatalf("the tool never hung (%s, %q)", d, data)
	}
}

// Every mouse press reaches the screen, so any click can cancel a question; only a left
// click or the wheel acts.
func TestAsksEveryClickIsAnEvent(t *testing.T) {
	var tk asksTokenizer
	events := tk.feed([]byte("\x1b[<2;5;5M\x1b[<1;5;5M\x1b[<2;5;5m"))
	if len(events) != 2 || !events[0].mouse || events[0].button != 2 || events[1].button != 1 {
		t.Fatalf("%+v", events)
	}
	b := newAsksScreen()
	b.w, b.h, b.loaded = 60, 20, true
	before := b.selected
	if _, send := b.click(events[0], time.Now()); send || b.selected != before {
		t.Fatal("a right click acted")
	}
}

// A dismiss taken just before q lands before the stores close.
func TestAsksQuitKeepsATakenWrite(t *testing.T) {
	reg, leases := asksTestStores(t)
	asksSeed(t, reg, state.Ask{Chain: "example", Workspace: "/example/project", RaisedAt: time.Now().Unix(), AsksReader: .9, FirstQuote: "q"})
	items, err := reg.ListAsks("example")
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	requests, results := make(chan asksRequest, 1), make(chan asksResult, 1)
	stop := make(chan struct{})
	go asksWorker(reg, leases, requests, results, stop)
	requests <- asksRequest{action: "state", id: items[0].ID, to: state.AskDismissed}
	asksDrain(true, results, 5*time.Second)
	close(stop)
	got, err := reg.GetAsk(items[0].ID)
	if err != nil || got.State != state.AskDismissed {
		t.Fatalf("%+v %v", got, err)
	}
}

// In a pane too small to show the whole question, y does not erase.
func TestAsksResetNeedsTheWholeQuestionOnScreen(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	for _, size := range [][2]int{{60, 1}, {60, 2}, {60, 3}, {20, 20}} {
		b := newAsksScreen()
		b.w, b.h, b.loaded = size[0], size[1], true
		b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
		b.draw(devnull, now)
		if req, send, _ := b.input(asksInput{key: keypress{r: 'y'}}, now); send || req.action == "reset" {
			t.Errorf("%dx%d: erased without the whole question shown", size[0], size[1])
		}
		if !strings.Contains(b.message, "too small") {
			t.Errorf("%dx%d: %q", size[0], size[1], b.message)
		}
	}
	// A narrow pane that fits the short question can still erase.
	b := newAsksScreen()
	b.w, b.h, b.loaded = 50, 20, true
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	b.draw(devnull, now)
	if req, send, _ := b.input(asksInput{key: keypress{r: 'y'}, alone: true}, now); !send || req.action != "reset" {
		t.Error("the short question must be answerable")
	}
	// Shrinking after the question was shown takes it back.
	b = newAsksScreen()
	b.w, b.h, b.loaded = 60, 20, true
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	b.draw(devnull, now)
	b.h = 2
	b.draw(devnull, now)
	if req, send, _ := b.input(asksInput{key: keypress{r: 'y'}}, now); send || req.action == "reset" {
		t.Error("erased after the pane shrank past the question")
	}
}

// Escape is a key: alone, or followed at once by another, and it keeps a pending reset.
func TestAsksEscapeIsAKey(t *testing.T) {
	var tk asksTokenizer
	if got := tk.feed([]byte("\x1b")); len(got) != 0 || !tk.waiting() {
		t.Fatalf("a lone escape must wait: %+v", got)
	}
	if got := tk.flush(); len(got) != 1 || got[0].key.code != keyBack || tk.waiting() {
		t.Fatalf("flushed escape: %+v", got)
	}
	got := tk.feed([]byte("\x1bq"))
	if len(got) != 2 || got[0].key.code != keyBack || got[1].key.r != 'q' {
		t.Fatalf("escape q: %+v", got)
	}
	if got := tk.feed([]byte("\x1b[A")); len(got) != 1 || got[0].key.code != keyUp {
		t.Fatalf("arrow: %+v", got)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	b := newAsksScreen()
	b.w, b.h, b.loaded = 100, 20, true
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	b.draw(devnull, now)
	b.input(asksInput{key: keypress{code: keyBack}}, now)
	if req, send, _ := b.input(asksInput{key: keypress{r: 'y'}}, now); send || req.action == "reset" {
		t.Fatal("X, Escape, y erased")
	}
}

// A sequence split right after its Escape stays one sequence: a paste stays a paste and a
// click stays a click, so no byte of either acts as a key.
func TestAsksSplitAfterEscapeStaysOneSequence(t *testing.T) {
	var tk asksTokenizer
	tk.feed([]byte("\x1b"))
	got := tk.feed([]byte("[200~Xy\x1b[201~"))
	if len(got) != 1 || !got[0].paste {
		t.Fatalf("split paste: %+v", got)
	}
	tk.feed([]byte("\x1b"))
	got = tk.feed([]byte("[<0;10;5M"))
	if len(got) != 1 || !got[0].mouse || got[0].x != 10 {
		t.Fatalf("split click: %+v", got)
	}
	if tk.waiting() || tk.flush() != nil {
		t.Fatal("nothing should be held")
	}
}

// A paste mark whose Escape came well before the rest, past the wait, is still a paste:
// the X and y inside it neither ask nor answer the reset question.
func TestAsksPasteLateAfterItsEscapeIsAPaste(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	b := newAsksScreen()
	b.w, b.h, b.loaded = 100, 20, true
	b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
	b.draw(devnull, now)
	var tk asksTokenizer
	tk.feed([]byte("\x1b"))
	events := tk.flush()
	events = append(events, tk.feed([]byte("[200~Xy\x1b[201~"))...)
	if len(events) != 2 || events[0].key.code != keyBack || !events[1].paste {
		t.Fatalf("late paste: %+v", events)
	}
	for _, e := range events {
		if req, send, _ := b.input(e, now); send || req.action == "reset" {
			t.Fatal("a late paste erased")
		}
	}
	// Once the time after the Escape is over, a [ is a plain key again.
	tk.feed([]byte("\x1b"))
	tk.flush()
	tk.forget()
	if got := tk.feed([]byte("[")); len(got) != 1 || got[0].key.r != '[' {
		t.Fatalf("[ after the wait: %+v", got)
	}
}

// Past the second after its Escape, a paste mark split in pieces reads as keys, but the y
// inside it comes with other input, so it does not answer the question its X raised.
func TestAsksPasteLongAfterItsEscapeDoesNotErase(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	b := newAsksScreen()
	b.w, b.h, b.loaded = 100, 20, true
	var tk asksTokenizer
	tk.feed([]byte("\x1b"))
	tk.flush()
	tk.forget()
	// Each piece in a read of its own, with the screen drawn between.
	for _, piece := range []string{"[200~X", "y", "\x1b[201~"} {
		for _, e := range tk.feed([]byte(piece)) {
			if req, send, _ := b.input(e, now); send || req.action == "reset" {
				t.Fatalf("a pasted %q erased", piece)
			}
		}
		b.draw(devnull, now)
	}
	if b.confirmReset {
		t.Fatal("a pasted X raised the question")
	}
	// Split before the mark's digits, the X comes with other input and raises nothing.
	tk = asksTokenizer{}
	b.input(asksInput{key: keypress{code: keyBack}}, now)
	for _, piece := range []string{"[", "200~X", "y"} {
		for _, e := range tk.feed([]byte(piece)) {
			if req, send, _ := b.input(e, now); send || req.action == "reset" {
				t.Fatalf("a pasted %q erased", piece)
			}
		}
		b.draw(devnull, now)
	}
	if b.confirmReset {
		t.Fatal("an X read with other input raised the question")
	}
}

// A paste mark that arrives in pieces, each long after the last and each read on its own,
// still starts a paste, so the X and y inside it act as nothing.
func TestAsksPasteMarkInPiecesDoesNotErase(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	for _, pieces := range [][]string{
		{"[", "200~", "X", "y", "\x1b[201~"},
		{"[2", "00~", "X", "y", "\x1b[201~"},
		{"[", "2", "0", "0", "~", "X", "y", "\x1b[201~"},
	} {
		b := newAsksScreen()
		b.w, b.h, b.loaded = 100, 20, true
		var tk asksTokenizer
		tk.feed([]byte("\x1b"))
		tk.flush()
		tk.forget()
		for _, piece := range pieces {
			for _, e := range tk.feed([]byte(piece)) {
				if req, send, _ := b.input(e, now); send || req.action == "reset" {
					t.Fatalf("%q: a pasted %q erased", pieces, piece)
				}
			}
			b.draw(devnull, now)
		}
		if b.confirmReset {
			t.Fatalf("%q: a pasted X raised the question", pieces)
		}
		// After the end mark, typed keys act again.
		if e := tk.feed([]byte("X")); len(e) != 1 || !e[0].alone || e[0].key.r != 'X' {
			t.Fatalf("%q: X after the paste: %+v", pieces, e)
		}
	}
}

// Ctrl-C quits while a paste waits for its end mark, so a mark that never comes cannot hold
// the screen.
func TestAsksCtrlCQuitsDuringAPaste(t *testing.T) {
	for _, start := range []string{"\x1b[200~hello", "[200~hello"} {
		var tk asksTokenizer
		tk.feed([]byte(start))
		if e := tk.feed([]byte("q")); len(e) != 0 {
			t.Fatalf("%q: q inside a paste gave %+v", start, e)
		}
		e := tk.feed([]byte{0x03})
		if len(e) != 1 || e[0].key.code != keyQuit {
			t.Fatalf("%q: Ctrl-C inside a paste gave %+v", start, e)
		}
		if tk.pasting {
			t.Fatalf("%q: still pasting after Ctrl-C", start)
		}
	}
}

// Ctrl-C or Ctrl-D in a key sequence that never finished still quits, and nothing of the
// sequence becomes a key, then or after.
func TestAsksCtrlCQuitsInAnUnfinishedSequence(t *testing.T) {
	for _, reads := range [][]string{{"\x1b[", "\x03"}, {"\x1b[1;\x03"}, {"\x1bO", "\x04"}, {"\x1b[2", "0", "\x03j"}} {
		var tk asksTokenizer
		var events []asksInput
		for _, r := range reads {
			events = append(events, tk.feed([]byte(r))...)
		}
		if len(events) != 1 || events[0].key.code != keyQuit {
			t.Fatalf("%q gave %+v", reads, events)
		}
		if e := tk.feed([]byte("x")); len(e) != 1 || e[0].key.r != 'x' {
			t.Fatalf("%q: x after it gave %+v", reads, e)
		}
	}
}

// A key the screen does not know, such as Delete or F1, keeps a pending reset.
func TestAsksUnknownKeyKeepsAReset(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	now := time.Now()
	for _, seq := range []string{"\x1b[3~", "\x1bOP"} {
		var tk asksTokenizer
		events := tk.feed([]byte(seq))
		if len(events) != 1 {
			t.Fatalf("%q: %+v", seq, events)
		}
		b := newAsksScreen()
		b.w, b.h, b.loaded = 100, 20, true
		b.input(asksInput{key: keypress{r: 'X'}, alone: true}, now)
		b.draw(devnull, now)
		b.input(events[0], now)
		if req, send, _ := b.input(asksInput{key: keypress{r: 'y'}}, now); send || req.action == "reset" {
			t.Errorf("X, %q, y erased", seq)
		}
	}
}

// Every row starts by clearing its line.
func TestAsksRowsClearToTheEnd(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "screen")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := newAsksScreen()
	b.w, b.h, b.loaded = 60, 12, true
	b.draw(f, time.Now())
	out, _ := os.ReadFile(f.Name())
	if n := strings.Count(string(out), "\x1b[2K"); n != b.h {
		t.Fatalf("%d of %d rows clear", n, b.h)
	}
}
