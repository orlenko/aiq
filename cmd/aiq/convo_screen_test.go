package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/orlenko/aiq/internal/transcript"
)

// screenTurns is a session of n turns, each a prompt and its answer: 2n messages.
func screenTurns(n int, reply func(i int) string) *transcript.Session {
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", ID: "abcdef123456", Cwd: "/w"}
	for i := 0; i < n; i++ {
		r := fmt.Sprintf("answer %d", i)
		if reply != nil {
			r = reply(i)
		}
		start := at.Add(time.Duration(i) * time.Minute)
		s.Turns = append(s.Turns, transcript.Turn{Prompt: fmt.Sprintf("prompt %d", i), Reply: r, Started: start, Ended: start.Add(30 * time.Second)})
	}
	return s
}

func screenOf(live bool, w, h int, s *transcript.Session) *convoScreen {
	c := newConvoScreen(live)
	c.w, c.h = w, h
	c.apply(convoUpdate{s: s, begin: true})
	return c
}

func press(c *convoScreen, keys ...keypress) bool {
	quit := false
	for _, k := range keys {
		quit = c.input(asksInput{key: k})
	}
	return quit
}

var (
	kDown, kUp       = keypress{code: keyDown}, keypress{code: keyUp}
	kTab, kBackTab   = keypress{r: '\t'}, keypress{code: keyBackTab}
	kFirst, kLast    = keypress{r: 'g'}, keypress{code: keyEnd}
	kPgDn, kPgUp     = keypress{code: keyPgDn}, keypress{code: keyPgUp}
	kCopy, kQuitChar = keypress{r: 'y'}, keypress{r: 'q'}
)

// plainFrame is the screen as text, one row a line.
func plainFrame(c *convoScreen) []string {
	var out []string
	for _, r := range c.frame() {
		out = append(out, kitPlain(r))
	}
	return out
}

// The screen opens on the newest message; j/k and the arrows move one message, g and G go to
// the ends, and q, Escape and Ctrl-C quit.
func TestConvoScreenMovesByMessage(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	c := screenOf(false, 80, 40, screenTurns(5, nil))
	if len(c.msgs) != 10 || c.sel != 9 {
		t.Fatalf("%d messages, %d selected; want 10, the last", len(c.msgs), c.sel)
	}
	steps := []struct {
		k    keypress
		want int
	}{
		{keypress{r: 'k'}, 8}, {kUp, 7}, {keypress{r: 'j'}, 8}, {kDown, 9}, {kDown, 9},
		{kFirst, 0}, {kUp, 0}, {kLast, 9}, {keypress{code: keyHome}, 0}, {kDown, 1},
	}
	for i, s := range steps {
		// j and k come as Down and Up from the tokenizer, as in aiq asks.
		k := s.k
		if k.r == 'j' || k.r == 'k' {
			var tok asksTokenizer
			k = tok.feed([]byte(string(k.r)))[0].key
		}
		if press(c, k); c.sel != s.want {
			t.Fatalf("step %d (%+v): selected %d, want %d", i, s.k, c.sel, s.want)
		}
	}
	if c.msgs[0].kind != "you" || c.msgs[1].kind != "answer" {
		t.Fatalf("kinds %q, %q", c.msgs[0].kind, c.msgs[1].kind)
	}
	// The selected message carries the mark on its first row, and a rail on the rest.
	frame := plainFrame(c)
	if len(frame) != c.h {
		t.Fatalf("%d rows, want %d", len(frame), c.h)
	}
	marked := 0
	for i, row := range frame {
		if cells(row) != c.w {
			t.Errorf("row %d is %d cells wide, want %d: %q", i, cells(row), c.w, row)
		}
		if strings.HasPrefix(row, "▶ ") {
			marked++
			if !strings.Contains(row, "claude") {
				t.Errorf("the mark is not on the answer's head: %q", row)
			}
		}
	}
	if marked != 1 {
		t.Fatalf("%d rows marked:\n%s", marked, strings.Join(frame, "\n"))
	}
	if !strings.Contains(frame[len(frame)-1], "j/k move") || !strings.Contains(frame[len(frame)-1], "y copy") {
		t.Errorf("footer %q", frame[len(frame)-1])
	}
	for _, k := range []keypress{kQuitChar, {code: keyBack}, {code: keyQuit}} {
		if !press(c, k) {
			t.Errorf("%+v does not quit", k)
		}
	}
}

// Tab and Shift-Tab jump between what the person typed: prompts and what they typed while the
// agent worked, never an answer or a notice.
func TestConvoScreenTabJumpsBetweenPrompts(t *testing.T) {
	c := screenOf(false, 80, 60, convoFixture())
	var prompts, notices []int
	for i, m := range c.msgs {
		switch m.kind {
		case "you", "steer":
			prompts = append(prompts, i)
		case "notice":
			notices = append(notices, i)
		}
	}
	if len(prompts) < 4 || len(notices) == 0 {
		t.Fatalf("prompts %v, notices %v", prompts, notices)
	}
	press(c, kFirst)
	var got []int
	for range prompts {
		press(c, kTab)
		got = append(got, c.sel)
	}
	if fmt.Sprint(got) != fmt.Sprint(append(prompts[1:], prompts[len(prompts)-1])) {
		t.Fatalf("Tab went %v, prompts are %v", got, prompts)
	}
	got = nil
	for range prompts {
		press(c, kBackTab)
		got = append(got, c.sel)
	}
	want := []int{}
	for i := len(prompts) - 2; i >= 0; i-- {
		want = append(want, prompts[i])
	}
	if fmt.Sprint(got) != fmt.Sprint(append(want, prompts[0])) {
		t.Fatalf("Shift-Tab went %v, prompts are %v", got, prompts)
	}
	// Shift-Tab reaches the screen from the terminal as ESC [ Z.
	var tok asksTokenizer
	if ev := tok.feed([]byte("\x1b[Z")); len(ev) != 1 || ev[0].key.code != keyBackTab {
		t.Fatalf("ESC [ Z reads as %+v", ev)
	}
}

// y copies the selected message as the transcript has it, cleaned as the printer cleans it:
// the Markdown, never what the terminal shows.
func TestConvoScreenCopiesTheMarkdown(t *testing.T) {
	var got []string
	fail := error(nil)
	saved := asksCopier
	asksCopier = func(ctx context.Context, text string) (string, error) {
		got = append(got, text)
		if fail != nil {
			return "", fail
		}
		return "clipboard", nil
	}
	t.Cleanup(func() { asksCopier = saved })
	reply := "**Done.** Run `make test`.\n\n- one\n- two\x1b]52;c;eA==\x07\n"
	s := screenTurns(2, func(i int) string {
		if i == 1 {
			return reply
		}
		return "first answer"
	})
	c := screenOf(false, 80, 40, s)
	press(c, kCopy)
	want := "**Done.** Run `make test`.\n\n- one\n- two]52;c;eA=="
	if len(got) != 1 || got[0] != want {
		t.Fatalf("copied %q, want %q", got, want)
	}
	if c.message != "Copied the message to the clipboard" {
		t.Fatalf("message %q", c.message)
	}
	if frame := plainFrame(c); !strings.Contains(frame[len(frame)-2], "Copied the message to the clipboard") {
		t.Fatalf("status row %q", frame[len(frame)-2])
	}
	press(c, kUp, kCopy)
	if got[1] != "prompt 1" {
		t.Fatalf("copied the prompt as %q", got[1])
	}
	fail = errors.New("no clipboard found")
	press(c, kCopy)
	if c.message != "Could not copy: no clipboard found" {
		t.Fatalf("message %q", c.message)
	}
	press(c, kDown)
	if c.message != "" {
		t.Fatalf("the message outlives the next key: %q", c.message)
	}
}

// Every row the screen shows is the printer's, in its order: the screen only cuts it up, and
// wraps what the stream left the terminal to wrap.
func TestConvoScreenShowsWhatThePrinterPrints(t *testing.T) {
	s := convoFixture()
	s.Turns[6].Open = false // a "(no reply)" goes on its prompt
	s.Turns[6].Reply = "Shipped, with a line long enough that a screen this narrow has to wrap it somewhere."
	s.Turns[0].Reply = "A `code span` and **bold words** that run on past the edge of the screen, wrapped."
	c := screenOf(false, 40, 40, s)
	p := newConvoPrinter(s.Provider, true, false)
	p.width = 40 - convoGutter
	printed := p.emit(s)
	var rows []string
	for _, r := range c.rows {
		rows = append(rows, r.text)
		if n := cells(kitPlain(r.text)); n > p.width {
			t.Errorf("row %d cells wide, more than %d: %q", n, p.width, kitPlain(r.text))
		}
	}
	squeeze := strings.NewReplacer(" ", "", "\n", "")
	if got, want := squeeze.Replace(kitPlain(strings.Join(rows, "\n"))), squeeze.Replace(kitPlain(printed)); got != want {
		t.Fatalf("rows differ from the printer\nrows:\n%s\nprinter:\n%s", strings.Join(rows, "\n"), printed)
	}
	for i, m := range c.msgs {
		if m.from >= m.to || c.rows[m.from].text == "" {
			t.Errorf("message %d (%s) starts with a blank row or is empty", i, m.kind)
		}
		for r := m.from; r < m.to; r++ {
			if c.rows[r].msg != i {
				t.Errorf("row %d of message %d says %d", r, i, c.rows[r].msg)
			}
		}
	}
	for _, m := range c.msgs {
		if m.kind == "none" || m.kind == "more" {
			t.Errorf("a %s stands alone", m.kind)
		}
	}
}

// A line too wide breaks at its spaces, else between glyphs, and each row opens with the
// styles still in force where it broke.
func TestConvoWrapCarriesStyles(t *testing.T) {
	line := "plain " + sgrBold + "bold words" + sgrReset + " and " + sgrCode + "averyveryverylongword" + sgrReset
	got := convoWrap(line, 8)
	want := []string{
		"plain" + sgrReset,
		sgrBold + "bold" + sgrReset,
		sgrBold + "words" + sgrReset + sgrReset,
		"and" + sgrReset,
		sgrCode + "averyver" + sgrReset,
		sgrCode + "yverylon" + sgrReset,
		sgrCode + "gword" + sgrReset + sgrReset,
	}
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := convoWrap(line, 80); len(got) != 1 || got[0] != line {
		t.Fatalf("a line that fits: %q", got)
	}
}

// --follow: with the newest message selected, a new one is selected as it comes. Moved up,
// the selection and the view stay, the status line counts what is new below, and G follows
// again.
func TestConvoScreenFollowsTheNewest(t *testing.T) {
	full := screenTurns(8, nil)
	upTo := func(n int) *transcript.Session {
		s := *full
		s.Turns = full.Turns[:n]
		return &s
	}
	c := newConvoScreen(true)
	c.w, c.h = 80, 14
	c.apply(convoUpdate{s: upTo(3), begin: true})
	if c.sel != 5 || !c.visible(5) {
		t.Fatalf("selected %d of %d", c.sel, len(c.msgs))
	}
	c.apply(convoUpdate{s: upTo(4)})
	if c.sel != 7 || !c.visible(7) || c.newBelow() != 0 {
		t.Fatalf("at the end: selected %d of %d, %d new", c.sel, len(c.msgs), c.newBelow())
	}
	press(c, kUp, kUp, kUp, kUp, kUp)
	// The wheel leaves the selection lower on screen than its first row.
	c.input(asksInput{mouse: true, button: 64, x: 1, y: 5})
	id, top := c.msgs[c.sel].id, c.top
	if top == 0 || c.msgs[c.sel].from == top {
		t.Fatalf("the test wants a screen scrolled down, the selection below its top: top %d", top)
	}
	c.apply(convoUpdate{s: upTo(6)})
	if c.msgs[c.sel].id != id || c.top != top {
		t.Fatalf("moved up: selection %q top %d, want %q top %d", c.msgs[c.sel].id, c.top, id, top)
	}
	if c.newBelow() != 4 {
		t.Fatalf("%d new below, want 4", c.newBelow())
	}
	if status := plainFrame(c)[c.h-2]; !strings.Contains(status, "4 new below, G follows") {
		t.Fatalf("status %q", status)
	}
	press(c, kLast)
	if c.sel != 11 || c.newBelow() != 0 {
		t.Fatalf("G: selected %d, %d new", c.sel, c.newBelow())
	}
	c.apply(convoUpdate{s: upTo(8)})
	if c.sel != 15 || !c.visible(15) {
		t.Fatalf("following again: selected %d of %d", c.sel, len(c.msgs))
	}
	// What --follow says shows on the status line.
	c.apply(convoUpdate{status: "working · 1m · 2 tools"})
	if status := plainFrame(c)[c.h-2]; !strings.Contains(status, "working · 1m · 2 tools") {
		t.Fatalf("status %q", status)
	}
	// The pane moved: a divider, and the new conversation below it, followed.
	moved := screenTurns(1, func(int) string { return "fresh start" })
	moved.ID = "99999999ffff"
	c.apply(convoUpdate{s: moved, t: convoTarget{account: "codex/b"}, begin: true, moved: time.Now()})
	frame := strings.Join(plainFrame(c), "\n")
	if !strings.Contains(frame, "MOVED TO CODEX/B · SESSION 99999999") || !strings.Contains(frame, "fresh start") {
		t.Fatalf("after a move:\n%s", frame)
	}
	if !strings.Contains(plainFrame(c)[0], "session 99999999") {
		t.Errorf("header %q", plainFrame(c)[0])
	}
	if c.sel != len(c.msgs)-1 || c.msgs[c.sel].raw != "fresh start" {
		t.Fatalf("after a move: selected %d of %d", c.sel, len(c.msgs))
	}
}

// A resize re-wraps the conversation and keeps the selected message on screen.
func TestConvoScreenResizeKeepsTheSelection(t *testing.T) {
	long := strings.Repeat("several words that wrap ", 12)
	c := screenOf(true, 100, 30, screenTurns(10, func(i int) string { return fmt.Sprintf("%d %s", i, long) }))
	press(c, kUp, kUp, kUp, kUp, kUp)
	id, rows := c.msgs[c.sel].id, len(c.rows)
	c.resize(40, 30)
	if c.msgs[c.sel].id != id || !c.visible(c.sel) {
		t.Fatalf("after narrowing: selected %q, want %q (visible %v)", c.msgs[c.sel].id, id, c.visible(c.sel))
	}
	if len(c.rows) <= rows {
		t.Fatalf("not re-wrapped: %d rows, was %d", len(c.rows), rows)
	}
	for _, r := range plainFrame(c) {
		if cells(r) != 40 {
			t.Fatalf("row %d cells wide after the resize: %q", cells(r), r)
		}
	}
	c.resize(100, 12)
	if c.msgs[c.sel].id != id || !c.visible(c.sel) {
		t.Fatalf("after shortening: selected %q, want %q", c.msgs[c.sel].id, id)
	}
	// Following stays following.
	press(c, kLast)
	c.resize(60, 20)
	if c.sel != len(c.msgs)-1 || !c.visible(c.sel) {
		t.Fatalf("following: selected %d of %d", c.sel, len(c.msgs))
	}
}

// A message taller than the screen shows its start when selected; j goes on through it a few
// rows at a time and then to the next message, k back through it and then to the one before.
func TestConvoScreenLongMessage(t *testing.T) {
	var paras []string
	for i := 0; i < 60; i++ {
		paras = append(paras, fmt.Sprintf("paragraph %d", i))
	}
	s := screenTurns(3, func(i int) string {
		if i == 1 {
			return strings.Join(paras, "\n\n")
		}
		return fmt.Sprintf("short %d", i)
	})
	c := screenOf(false, 80, 20, s)
	h := c.height()
	press(c, kUp, kUp) // the prompt, then the long answer
	long := c.msgs[c.sel]
	if c.sel != 3 || long.to-long.from <= h {
		t.Fatalf("selected %d, %d rows high on a screen of %d", c.sel, long.to-long.from, h)
	}
	if c.top != long.from {
		t.Fatalf("top %d, want the answer's start %d", c.top, long.from)
	}
	if !strings.Contains(plainFrame(c)[len(c.head)], "claude") {
		t.Fatalf("the first row shown is %q", plainFrame(c)[len(c.head)])
	}
	press(c, kDown)
	if c.sel != 3 || c.top != long.from+convoStep {
		t.Fatalf("j: selected %d top %d, want 3 and %d", c.sel, c.top, long.from+convoStep)
	}
	press(c, kUp)
	if c.sel != 3 || c.top != long.from {
		t.Fatalf("k: selected %d top %d", c.sel, c.top)
	}
	n := 0
	for c.sel == 3 {
		press(c, kDown)
		if n++; n > 100 {
			t.Fatal("j never leaves the long answer")
		}
	}
	if c.sel != 4 {
		t.Fatalf("after the long answer: selected %d", c.sel)
	}
	// Its end was on screen before j moved on.
	if n != (long.to-h-long.from+convoStep-1)/convoStep+1 {
		t.Errorf("%d presses through the answer", n)
	}
	press(c, kUp)
	if c.sel != 3 || c.top != long.from {
		t.Fatalf("back up: selected %d top %d, want its start %d", c.sel, c.top, long.from)
	}
	press(c, kUp)
	if c.sel != 2 {
		t.Fatalf("k at the start: selected %d", c.sel)
	}
	// Coming down to it shows its start too, not its end.
	press(c, kDown)
	if c.sel != 3 || c.top != long.from {
		t.Fatalf("j onto it: selected %d top %d, want 3 and %d", c.sel, c.top, long.from)
	}
	// Page Down scrolls a page and keeps the selection on screen.
	press(c, kFirst, kPgDn)
	if c.top != h-1 || !c.visible(c.sel) {
		t.Fatalf("PgDn: top %d, selected %d visible %v", c.top, c.sel, c.visible(c.sel))
	}
	press(c, kPgUp)
	if c.top != 0 || !c.visible(c.sel) {
		t.Fatalf("PgUp: top %d", c.top)
	}
}

// The wheel scrolls with the selection kept on screen; a click selects the message under it.
func TestConvoScreenMouse(t *testing.T) {
	c := screenOf(false, 80, 14, screenTurns(8, nil))
	top := c.top
	c.input(asksInput{mouse: true, button: 64, x: 5, y: 5})
	if c.top != top-convoStep || !c.visible(c.sel) {
		t.Fatalf("wheel up: top %d, want %d; selection visible %v", c.top, top-convoStep, c.visible(c.sel))
	}
	for i := 0; i < 40; i++ {
		c.input(asksInput{mouse: true, button: 64, x: 5, y: 5})
	}
	if c.top != 0 || !c.visible(c.sel) {
		t.Fatalf("wheel to the top: top %d, selected %d", c.top, c.sel)
	}
	// The row under the click: header rows, then the conversation from top.
	for y := len(c.head) + 1; y <= len(c.head)+c.height(); y++ {
		r := c.rows[c.top+y-1-len(c.head)]
		if r.msg < 0 || r.msg == c.sel {
			continue
		}
		c.input(asksInput{mouse: true, button: 0, x: 3, y: y})
		if c.sel != r.msg {
			t.Fatalf("click on row %d: selected %d, want %d", y, c.sel, r.msg)
		}
		return
	}
	t.Fatal("no other message on screen to click")
}

// Off a terminal aiq convo prints plain text, as it always has.
func TestConvoPrintsPlainTextOffATerminal(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(home, "data"))
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "TMUX_PANE", "AIQ_THEME", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		t.Setenv(v, "")
	}
	t.Chdir(dir)
	const id = "5e551011-0000-4000-8000-000000000001"
	project := filepath.Join(home, ".claude", "projects", claudeNonAlnum.ReplaceAllString(dir, "-"))
	os.MkdirAll(project, 0o700)
	c := `"cwd":"` + dir + `","sessionId":"` + id + `"`
	user := func(ts, text string) string {
		return `{"type":"user",` + c + `,"timestamp":"` + ts + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	reply := func(ts, mid, text string) string {
		return `{"type":"assistant",` + c + `,"timestamp":"` + ts + `","message":{"id":"` + mid + `","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(filepath.Join(project, id+".jsonl"), []byte(user("2026-09-15T10:00:00Z", "first")+reply("2026-09-15T10:01:00Z", "m1", "one")+
		user("2026-09-15T10:02:00Z", "second")+reply("2026-09-15T10:03:00Z", "m2", "**two** and `three`")), 0o600)

	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := cmdConvo([]string{"5e551011"})
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	when := func(ts string) string {
		at, _ := time.Parse(time.RFC3339, ts)
		return whenLabel(at, time.Now())
	}
	want := "claude opus-5 · " + dir + " · session 5e551011\n" +
		"\n▌ you · " + when("2026-09-15T10:00:00Z") + "\nfirst\n" +
		"\n▌ claude · 1m\none\n" +
		"\n▌ you · " + when("2026-09-15T10:02:00Z") + "\nsecond\n" +
		"\n▌ claude · 1m\n**two** and `three`\n"
	if string(out) != want {
		t.Fatalf("got\n%q\nwant\n%q", out, want)
	}
}

// On a terminal --follow's loop hands what it finds to the screen and prints nothing: the
// first conversation, what the file gains, and the status line.
func TestFollowFeedsTheScreen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	c := `"cwd":"/w","sessionId":"s1"`
	user := func(ts, text string) string {
		return `{"type":"user",` + c + `,"timestamp":"` + ts + `","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	reply := func(ts, id, text string) string {
		return `{"type":"assistant",` + c + `,"timestamp":"` + ts + `","message":{"id":"` + id + `","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(path, []byte(user("2026-09-15T10:00:00Z", "first")+reply("2026-09-15T10:01:00Z", "m1", "one")), 0o644)
	var stream bytes.Buffer
	updates := make(chan convoUpdate, 64)
	f := &convoFollow{tgt: convoTarget{provider: "claude", path: path}, w: &stream, tty: true,
		statEvery: 10 * time.Millisecond, parseGap: 30 * time.Millisecond, resolveEvery: time.Hour, quiet: 50 * time.Millisecond,
		send: func(u convoUpdate) { updates <- u }}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.loop(stop); close(done) }()
	screen := newConvoScreen(true)
	screen.w, screen.h = 80, 30
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for !ok() {
			select {
			case u := <-updates:
				screen.apply(u)
			case <-deadline:
				t.Fatalf("no %s: %d messages, status %q", what, len(screen.msgs), screen.status)
			}
		}
	}
	wait("first conversation", func() bool { return len(screen.msgs) == 2 })
	fh, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(user("2026-09-15T10:02:00Z", "second"))
	fh.Close()
	wait("prompt", func() bool { return len(screen.msgs) == 3 && screen.sel == 2 })
	fh, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	fh.WriteString(reply("2026-09-15T10:03:00Z", "m2", "two"))
	fh.Close()
	wait("reply", func() bool { return len(screen.msgs) == 4 && screen.sel == 3 && screen.msgs[3].raw == "two" })
	close(stop)
	<-done
	if stream.Len() != 0 {
		t.Fatalf("the stream got %q", stream.String())
	}
}

// --follow off a terminal prints the stream, byte for byte as it always has.
func TestFollowStreamOffATerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	c := `"cwd":"/w","sessionId":"s1"`
	os.WriteFile(path, []byte(`{"type":"user",`+c+`,"timestamp":"2026-09-15T10:00:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"first"}}`+"\n"+
		`{"type":"assistant",`+c+`,"timestamp":"2026-09-15T10:01:00Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"one"}]}}`+"\n"), 0o644)
	var b bytes.Buffer
	f := &convoFollow{tgt: convoTarget{provider: "claude", path: path}, w: &b,
		statEvery: 10 * time.Millisecond, parseGap: 30 * time.Millisecond, resolveEvery: time.Hour, quiet: time.Millisecond}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() { f.run(stop); close(done) }()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done
	at, _ := time.Parse(time.RFC3339, "2026-09-15T10:00:00Z")
	want := "claude opus-5 · /w · session s1\n\n▌ you · " + whenLabel(at, time.Now()) + "\nfirst\n\n▌ claude · 1m\none\n"
	if b.String() != want {
		t.Fatalf("got\n%q\nwant\n%q", b.String(), want)
	}
}

// A file rewritten shorter than the --last cutoff is shown from what it holds now; growth
// keeps the cutoff.
func TestConvoScreenShorterSnapshot(t *testing.T) {
	c := newConvoScreen(true)
	c.w, c.h, c.last = 80, 40, 2
	six := screenTurns(6, nil)
	c.apply(convoUpdate{s: six, begin: true, from: convoStart(six.Turns, 2)})
	if c.parts[0].from != 4 || len(c.msgs) != 4 {
		t.Fatalf("from %d, %d messages; want 4 and 4", c.parts[0].from, len(c.msgs))
	}
	c.apply(convoUpdate{s: screenTurns(7, nil)})
	if c.parts[0].from != 4 || len(c.msgs) != 6 {
		t.Fatalf("grown: from %d, %d messages; want 4 and 6", c.parts[0].from, len(c.msgs))
	}
	c.apply(convoUpdate{s: screenTurns(1, nil)})
	if c.parts[0].from != 0 || len(c.msgs) != 2 || c.msgs[0].raw != "prompt 0" {
		t.Fatalf("shorter: from %d, %d messages; want 0 and 2", c.parts[0].from, len(c.msgs))
	}
	c.apply(convoUpdate{s: &transcript.Session{Provider: "claude", ID: "abcdef123456"}})
	if c.parts[0].from != 0 || len(c.msgs) != 0 {
		t.Fatalf("empty: from %d, %d messages", c.parts[0].from, len(c.msgs))
	}
	plainFrame(c)
	c.apply(convoUpdate{s: screenTurns(3, nil)})
	if c.parts[0].from != 0 || len(c.msgs) != 6 {
		t.Fatalf("after empty: from %d, %d messages", c.parts[0].from, len(c.msgs))
	}
}

// A file rewritten to hold exactly as many turns as the cutoff skipped shows its last N
// turns, and y copies the one selected.
func TestConvoScreenRewrittenToTheCutoff(t *testing.T) {
	var got []string
	saved := asksCopier
	asksCopier = func(ctx context.Context, text string) (string, error) {
		got = append(got, text)
		return "clipboard", nil
	}
	t.Cleanup(func() { asksCopier = saved })
	c := newConvoScreen(true)
	c.w, c.h, c.last = 80, 40, 2
	six := screenTurns(6, nil)
	c.apply(convoUpdate{s: six, begin: true, from: convoStart(six.Turns, 2)})
	c.apply(convoUpdate{s: screenTurns(4, nil)})
	if c.parts[0].from != 2 || len(c.msgs) != 4 || c.msgs[0].raw != "prompt 2" {
		t.Fatalf("from %d, %d messages", c.parts[0].from, len(c.msgs))
	}
	press(c, kCopy)
	if len(got) != 1 || got[0] != "answer 3" {
		t.Fatalf("copied %q", got)
	}
}

// Narration said in several records of one message shows as one line, and y copies all of it.
func TestConvoScreenCopiesEveryPartOfANarration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(claudeReplyFixtures()["one message, several texts"], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := transcript.ParseClaude(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	saved := asksCopier
	asksCopier = func(ctx context.Context, text string) (string, error) {
		got = append(got, text)
		return "clipboard", nil
	}
	t.Cleanup(func() { asksCopier = saved })
	c := screenOf(false, 80, 40, s)
	narration := -1
	for i, m := range c.msgs {
		var text []string
		for _, r := range c.rows[m.from:m.to] {
			text = append(text, kitPlain(r.text))
		}
		all := strings.Join(text, "\n")
		if strings.Contains(all, "Narration B") {
			t.Fatalf("message %d shows the second part: %q", i, all)
		}
		if strings.Contains(all, "Narration A.") {
			narration = i
			if m.to-m.from != 1 {
				t.Fatalf("the narration takes %d rows", m.to-m.from)
			}
		}
	}
	if narration < 0 {
		t.Fatalf("no narration among %d messages", len(c.msgs))
	}
	c.sel = narration
	press(c, kCopy)
	if len(got) != 1 || !strings.Contains(got[0], "Narration A.") || !strings.Contains(got[0], "Narration B.") {
		t.Fatalf("copied %q", got)
	}
}

func TestConvoExpandTabs(t *testing.T) {
	for in, want := range map[string]string{
		"a\tb":                       "a       b",
		"abcdefgh\ti":                "abcdefgh        i",
		"\x1b[2m\tx\x1b[0m\ty":       "\x1b[2m        x\x1b[0m       y",
		"日\tx":                       "日      x",
		"no tabs \x1b[1mhere\x1b[0m": "no tabs \x1b[1mhere\x1b[0m",
	} {
		if got := convoExpandTabs(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// Tabs in narration, a notice and an answer take the columns a terminal gives them, so
// every row is as wide as the screen, before and after a resize; y still copies the tabs.
func TestConvoScreenTabs(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", ID: "abcdef123456", Cwd: "/w", Turns: []transcript.Turn{
		{Source: transcript.Notice, Prompt: "task\tdone\tnow", Reply: "ok", Started: at, Ended: at.Add(time.Second)},
		{Prompt: "go", Started: at, Ended: at.Add(time.Minute), Reply: "col\tcol\tcol\tcol\tcol\tcol\tcol\tend",
			Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "a\tb\tc\td\te\tf\tg\th\ti\tj\tk", At: at}, {Kind: transcript.ItemAnswer, Text: "col\tcol\tcol\tcol\tcol\tcol\tcol\tend", At: at}}},
	}}
	var got []string
	saved := asksCopier
	asksCopier = func(ctx context.Context, text string) (string, error) {
		got = append(got, text)
		return "clipboard", nil
	}
	t.Cleanup(func() { asksCopier = saved })
	c := screenOf(false, 30, 40, s)
	check := func(w int) {
		t.Helper()
		tabs := 0
		for _, r := range c.rows {
			tabs += strings.Count(r.text, "\t")
		}
		for i, r := range c.frame() {
			tabs += strings.Count(r, "\t")
			if n := cells(kitPlain(r)); n != w {
				t.Errorf("width %d: row %d is %d cells: %q", w, i, n, kitPlain(r))
			}
		}
		if tabs != 0 {
			t.Errorf("width %d: %d tabs on screen", w, tabs)
		}
	}
	check(30)
	c.resize(50, 40)
	check(50)
	var said []string
	for i := range c.msgs {
		c.sel = i
		press(c, kCopy)
		if strings.Contains(got[len(got)-1], "\t") {
			said = append(said, got[len(got)-1])
		}
	}
	if len(said) < 2 {
		t.Fatalf("copies with their tabs: %q", said)
	}
}

// Below twelve columns the screen says it is too narrow, rather than cut every row short;
// wider, every letter of a long word is on some row.
func TestConvoScreenTooNarrow(t *testing.T) {
	t.Setenv("LANG", "en_US.UTF-8")
	s := screenTurns(1, func(int) string { return "ABCDEFGHIJKLMNOPQRST" })
	c := screenOf(false, 10, 8, s)
	frame := plainFrame(c)
	if len(frame) != 8 || !strings.HasPrefix(frame[0], "Too narro") {
		t.Fatalf("frame %q", frame)
	}
	for i, r := range frame {
		if cells(r) != 10 || strings.Contains(r, "ABC") {
			t.Errorf("row %d %q", i, r)
		}
	}
	if !press(c, kQuitChar) {
		t.Fatal("q does not quit")
	}
	for _, w := range []int{12, 40} {
		c.resize(w, 16)
		all := strings.Join(plainFrame(c), "")
		for _, l := range "ABCDEFGHIJKLMNOPQRST" {
			if !strings.ContainsRune(all, l) {
				t.Fatalf("width %d: %c is not on screen:\n%s", w, l, strings.Join(plainFrame(c), "\n"))
			}
		}
	}
}

// fakeConvoTerm is the terminal for runConvoScreen, noting what is done to it.
type fakeConvoTerm struct {
	mu     sync.Mutex
	calls  []string
	typed  chan []byte
	hold   chan struct{} // frames wait on it when set, as on output the terminal paused
	first  bool          // the escapes that enter the screen wait on hold too
	last   []byte        // what the reader hands on as it is stopped
	stuck  bool          // stopping does not end the keys
	panics bool          // size panics
	ended  sync.Once
}

func newFakeConvoTerm() *fakeConvoTerm { return &fakeConvoTerm{typed: make(chan []byte)} }

func (f *fakeConvoTerm) note(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeConvoTerm) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeConvoTerm) raw() error { f.note("raw"); return nil }

func (f *fakeConvoTerm) write(s string) error {
	switch {
	case s == asksTerminalLeave:
		f.note("leave")
	case strings.HasPrefix(s, asksTerminalEnter):
		f.note("enter")
		if f.first {
			<-f.hold
		}
	default:
		if f.hold != nil {
			f.note("frame")
			<-f.hold
		}
	}
	return nil
}

func (f *fakeConvoTerm) size() (int, int, error) {
	if f.panics {
		panic("size")
	}
	return 80, 24, nil
}

func (f *fakeConvoTerm) keys() (<-chan []byte, func()) {
	// As readKeys, stopping ends the keys; what was typed before has been taken. A last read
	// comes a little after the stop, so the way out has to wait for the parser to see it.
	return f.typed, func() {
		f.note("stop keys")
		if f.stuck {
			return // the keys never end, so the parser never settles
		}
		f.ended.Do(func() {
			if f.last == nil {
				close(f.typed)
				return
			}
			go func() {
				time.Sleep(5 * time.Millisecond)
				f.typed <- f.last
				close(f.typed)
			}()
		})
	}
}

func (f *fakeConvoTerm) discard(pasting bool) { f.note(fmt.Sprintf("discard %v", pasting)) }

func (f *fakeConvoTerm) cooked() { f.note("cooked") }

func (f *fakeConvoTerm) abandon() { f.note("abandon") }

// afterRun is what was done to the terminal once the screen ends.
var afterRun = []string{"stop keys", "discard false", "cooked", "leave"}

func runFake(t *testing.T, f *fakeConvoTerm, failed chan error, stop chan struct{}) chan error {
	t.Helper()
	c := screenOf(false, 80, 24, screenTurns(2, nil))
	done := make(chan error, 1)
	go func() { done <- runConvoScreen(c, f, nil, failed, stop) }()
	return done
}

func waitRun(t *testing.T, done chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatal("the screen did not end")
		return nil
	}
}

func tail(calls []string, n int) []string { return calls[max(len(calls)-n, 0):] }

// noExit fails a test where aiq would exit.
func noExit(t *testing.T) {
	saved := convoExit
	convoExit = func(code int) { t.Errorf("exit %d", code) }
	t.Cleanup(func() { convoExit = saved })
}

// However the screen ends, the key reader stops, what was typed and not read is dropped, the
// mode goes back, and then the screen is left, once.
func TestConvoScreenPutsTheTerminalBack(t *testing.T) {
	noExit(t)
	for name, end := range map[string]func(f *fakeConvoTerm, failed chan error, stop chan struct{}){
		"q":      func(f *fakeConvoTerm, _ chan error, _ chan struct{}) { f.typed <- []byte("q") },
		"Ctrl-C": func(f *fakeConvoTerm, _ chan error, _ chan struct{}) { f.typed <- []byte{3} },
		"failed": func(_ *fakeConvoTerm, failed chan error, _ chan struct{}) { failed <- errors.New("gone") },
		"stop":   func(_ *fakeConvoTerm, _ chan error, stop chan struct{}) { close(stop) },
	} {
		t.Run(name, func(t *testing.T) {
			f, failed, stop := newFakeConvoTerm(), make(chan error, 1), make(chan struct{})
			done := runFake(t, f, failed, stop)
			// The screen is up first: the watcher starts before the enter write, so a stop
			// sent at once can restore before "enter" is noted. A stop before the screen is
			// drawn is TestConvoScreenStuckFromTheStart's case.
			for deadline := time.Now().Add(time.Second); !slices.Contains(f.log(), "enter"); {
				if time.Now().After(deadline) {
					t.Fatalf("no enter; calls %q", f.log())
				}
				time.Sleep(time.Millisecond)
			}
			end(f, failed, stop)
			err := waitRun(t, done, 2*time.Second)
			if (name == "failed") != (err != nil) {
				t.Fatalf("err %v", err)
			}
			if got := f.log(); !reflect.DeepEqual(tail(got, 4), afterRun) || strings.Count(strings.Join(got, ","), "leave") != 1 {
				t.Fatalf("calls %q", got)
			}
		})
	}
	t.Run("panic", func(t *testing.T) {
		f := newFakeConvoTerm()
		f.panics = true
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			runConvoScreen(screenOf(false, 80, 24, screenTurns(1, nil)), f, nil, nil, nil)
		}()
		if got := f.log(); !reflect.DeepEqual(tail(got, 4), afterRun) {
			t.Fatalf("calls %q", got)
		}
	})
}

// Ctrl-C inside a key sequence not yet finished quits, and since the sequence may be a paste
// mark, what may follow is read and dropped.
func TestConvoScreenCtrlCCutsASequence(t *testing.T) {
	noExit(t)
	for _, reads := range [][]string{{"\x1b[", "\x03"}, {"\x1b[20", "\x03"}, {"\x1b[20\x03"}} {
		f := newFakeConvoTerm()
		done := runFake(t, f, nil, nil)
		for _, r := range reads {
			f.typed <- []byte(r)
			time.Sleep(20 * time.Millisecond)
		}
		waitRun(t, done, 2*time.Second)
		if got := f.log(); !reflect.DeepEqual(tail(got, 4), []string{"stop keys", "discard true", "cooked", "leave"}) {
			t.Fatalf("%q: calls %q", reads, got)
		}
	}
}

// With the output stuck from the start, a paste mark begun before a signal ends the screen is
// still read and dropped.
func TestConvoScreenStuckFromTheStartWithAPasteMark(t *testing.T) {
	saved, savedExit := convoLeaveWait, convoExit
	exited := make(chan int, 1)
	convoLeaveWait, convoExit = 100*time.Millisecond, func(code int) { exited <- code }
	t.Cleanup(func() { convoLeaveWait, convoExit = saved, savedExit })
	f := newFakeConvoTerm()
	f.hold, f.first = make(chan struct{}), true
	stop := make(chan struct{})
	done := runFake(t, f, nil, stop)
	select {
	case f.typed <- []byte("\x1b[20"):
	case <-time.After(time.Second):
		t.Fatalf("the keys are not read; calls %q", f.log())
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatalf("no exit; calls %q", f.log())
	}
	if got := f.log(); !reflect.DeepEqual(got, []string{"raw", "enter", "stop keys", "discard true", "cooked", "abandon", "abandon"}) {
		t.Fatalf("calls %q", got)
	}
	close(f.hold)
	waitRun(t, done, 2*time.Second)
}

// The key reader convo uses hands on a read it holds when it is stopped, so the parser sees
// it; readKeys, as resume and asks use it, drops it as before.
func TestReadKeysHandsOnARead(t *testing.T) {
	for _, hand := range []time.Duration{0, time.Second, -1} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		var keys <-chan []byte
		var stop func()
		if hand < 0 {
			// -1: the reader the convo screen takes from its terminal.
			keys, stop = newConvoTTY(&devTTY{f: r, fd: int(r.Fd())}, w).keys()
		} else {
			keys, stop = readKeysHanding(int(r.Fd()), hand, nil)
		}
		w.Write([]byte("\x1b[20"))
		time.Sleep(100 * time.Millisecond) // read, and held: nobody takes it yet
		stopped := make(chan struct{})
		go func() { stop(); close(stopped) }()
		time.Sleep(20 * time.Millisecond) // stopped before anyone takes the keys
		var got []byte
		for b := range keys {
			got = append(got, b...)
		}
		<-stopped
		r.Close()
		w.Close()
		if want := hand != 0; (string(got) == "\x1b[20") != want {
			t.Fatalf("hand %v: got %q", hand, got)
		}
	}
}

// A read the convo screen's key reader drops, as nobody took it in time, may have begun a
// paste: the way out drains the rest as one, and nothing of it is left for the shell.
func TestConvoTTYDrainsALostRead(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	tty := newConvoTTY(&devTTY{f: r, fd: int(r.Fd())}, w)
	_, stop := tty.keys()
	w.Write([]byte("\x1b[20"))
	time.Sleep(100 * time.Millisecond) // read, and held
	stop()                             // nobody takes the keys: the read is dropped
	w.Write([]byte("0~pasted text"))
	tty.discard(false) // the parser never saw the read, so it cannot say a paste began
	if b := tty.in.wait(20 * time.Millisecond); len(b) > 0 {
		t.Fatalf("left for the shell %q", b)
	}
}

// A signal, such as a resize, that ends select early is not quiet: the drain waits on, and
// a tail that comes later in the same quiet window is still dropped.
func TestConvoTTYWaitOutlastsASignal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	interrupted := 0
	old := ttySelect
	t.Cleanup(func() { ttySelect = old })
	ttySelect = func(n int, rd, wr, ex *unix.FdSet, tv *unix.Timeval) (int, error) {
		if interrupted == 0 {
			interrupted++
			time.Sleep(20 * time.Millisecond)
			return -1, unix.EINTR
		}
		return old(n, rd, wr, ex, tv)
	}
	tty := newConvoTTY(&devTTY{f: r, fd: int(r.Fd())}, w)
	written := make(chan struct{})
	go func() {
		defer close(written)
		time.Sleep(60 * time.Millisecond) // inside the 100 ms quiet window
		w.Write([]byte("pasted tail\n"))
	}()
	tty.discard(true)
	<-written
	if interrupted != 1 {
		t.Fatalf("select interrupted %d times; want 1", interrupted)
	}
	ttySelect = old
	if b := tty.in.wait(20 * time.Millisecond); len(b) > 0 {
		t.Fatalf("left for the shell %q", b)
	}
}

// When the key parser has not settled in the time the way out gives it, what it has not
// parsed may begin a paste: the rest is drained as one.
func TestConvoScreenUnsettledKeysDrainAsAPaste(t *testing.T) {
	noExit(t)
	f, stop := newFakeConvoTerm(), make(chan struct{})
	f.stuck = true
	done := runFake(t, f, nil, stop)
	for deadline := time.Now().Add(time.Second); !slices.Contains(f.log(), "enter"); {
		if time.Now().After(deadline) {
			t.Fatalf("no enter; calls %q", f.log())
		}
		time.Sleep(time.Millisecond)
	}
	close(stop)
	waitRun(t, done, 2*time.Second)
	if got := f.log(); !reflect.DeepEqual(tail(got, 4), []string{"stop keys", "discard true", "cooked", "leave"}) {
		t.Fatalf("calls %q", got)
	}
}

// A paste cut short by Ctrl-C leaves the rest of it to be read and dropped.
func TestConvoScreenDropsTheRestOfAPaste(t *testing.T) {
	noExit(t)
	f := newFakeConvoTerm()
	done := runFake(t, f, nil, nil)
	f.typed <- []byte("\x1b[200~some text")
	f.typed <- []byte("more\x03")
	waitRun(t, done, 2*time.Second)
	if got := f.log(); !reflect.DeepEqual(tail(got, 4), []string{"stop keys", "discard true", "cooked", "leave"}) {
		t.Fatalf("calls %q", got)
	}
}

// convoEnds are the ways to end the screen from outside it: a signal's stop, and the keys
// that quit, which work as a stop does.
var convoEnds = map[string]func(f *fakeConvoTerm, stop chan struct{}){
	"stop":   func(_ *fakeConvoTerm, stop chan struct{}) { close(stop) },
	"Ctrl-C": func(f *fakeConvoTerm, _ chan struct{}) { f.typed <- []byte{3} },
	"q":      func(f *fakeConvoTerm, _ chan struct{}) { f.typed <- []byte("q") },
}

// A stop or a quit key puts the keyboard back while a frame cannot be written, and aiq exits
// when the screen still has not ended after a while, dropping the output the terminal has
// not taken.
func TestConvoScreenStopWithOutputStuck(t *testing.T) {
	for name, end := range convoEnds {
		t.Run(name, func(t *testing.T) {
			saved, savedExit := convoLeaveWait, convoExit
			exited := make(chan int, 1)
			convoLeaveWait, convoExit = 100*time.Millisecond, func(code int) { exited <- code }
			t.Cleanup(func() { convoLeaveWait, convoExit = saved, savedExit })
			f := newFakeConvoTerm()
			f.hold = make(chan struct{})
			stop := make(chan struct{})
			done := runFake(t, f, nil, stop)
			time.Sleep(50 * time.Millisecond) // the first frame is stuck
			start := time.Now()
			end(f, stop)
			select {
			case code := <-exited:
				if code != 0 {
					t.Fatalf("exit %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("no exit; calls %q", f.log())
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("exit after %v", d)
			}
			if got := f.log(); !reflect.DeepEqual(got, []string{"raw", "enter", "frame", "stop keys", "discard false", "cooked", "abandon", "abandon"}) {
				t.Fatalf("calls with the output stuck %q", got)
			}
			// Once the terminal takes the piece it held, nothing more goes out: no rest of the
			// frame, and no escapes to leave the screen, which would only fill its queue again.
			close(f.hold)
			waitRun(t, done, 2*time.Second)
			if got := f.log(); len(got) != 8 {
				t.Fatalf("calls after the output went on %q", got)
			}
		})
	}
}

// With the output stuck from the start, so that not even the escapes that enter the screen go
// out, a stop or a quit key still puts the keyboard back and aiq exits.
func TestConvoScreenStuckFromTheStart(t *testing.T) {
	for name, end := range convoEnds {
		t.Run(name, func(t *testing.T) {
			saved, savedExit := convoLeaveWait, convoExit
			exited := make(chan int, 1)
			convoLeaveWait, convoExit = 100*time.Millisecond, func(code int) { exited <- code }
			t.Cleanup(func() { convoLeaveWait, convoExit = saved, savedExit })
			f := newFakeConvoTerm()
			f.hold, f.first = make(chan struct{}), true
			stop := make(chan struct{})
			done := runFake(t, f, nil, stop)
			time.Sleep(50 * time.Millisecond)
			start := time.Now()
			go end(f, stop) // a key nobody reads would hold the test, not fail it
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				t.Fatalf("no exit; calls %q", f.log())
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("exit after %v", d)
			}
			if got := f.log(); !reflect.DeepEqual(got, []string{"raw", "enter", "stop keys", "discard false", "cooked", "abandon", "abandon"}) {
				t.Fatalf("calls %q", got)
			}
			close(f.hold)
			waitRun(t, done, 2*time.Second)
			if got := f.log(); len(got) != 7 {
				t.Fatalf("calls after the output went on %q", got)
			}
		})
	}
}

// A paste mark the reader hands on as it is stopped is parsed before the way out decides
// whether a paste may still be coming.
func TestConvoScreenPasteMarkAtTheStop(t *testing.T) {
	noExit(t)
	for i := 0; i < 50; i++ {
		f, stop := newFakeConvoTerm(), make(chan struct{})
		f.last = []byte("\x1b[200~")
		done := runFake(t, f, nil, stop)
		for deadline := time.Now().Add(time.Second); !slices.Contains(f.log(), "enter"); {
			if time.Now().After(deadline) {
				t.Fatalf("run %d: no enter; calls %q", i, f.log())
			}
			time.Sleep(time.Millisecond)
		}
		close(stop)
		waitRun(t, done, 2*time.Second)
		if got := f.log(); !reflect.DeepEqual(tail(got, 4), []string{"stop keys", "discard true", "cooked", "leave"}) {
			t.Fatalf("run %d: calls %q", i, got)
		}
	}
}

// q inside a paste, and an Escape that starts a sequence, do not quit; q after them does.
func TestConvoScreenPastedQDoesNotQuit(t *testing.T) {
	noExit(t)
	f := newFakeConvoTerm()
	done := runFake(t, f, nil, nil)
	f.typed <- []byte("\x1b[A")
	f.typed <- []byte("\x1b[200~q")
	f.typed <- []byte("q") // a read of its own, still inside the paste
	select {
	case <-done:
		t.Fatalf("quit; calls %q", f.log())
	case <-time.After(200 * time.Millisecond):
	}
	f.typed <- []byte("\x1b[201~")
	f.typed <- []byte("q")
	waitRun(t, done, 2*time.Second)
	if got := f.log(); !reflect.DeepEqual(tail(got, 4), afterRun) {
		t.Fatalf("calls %q", got)
	}
}

// An Escape that quits may be the start of a paste mark whose rest comes late: the way out
// reads and drops what may follow, as for a paste.
func TestConvoScreenEscapeMayStartAPaste(t *testing.T) {
	noExit(t)
	f := newFakeConvoTerm()
	done := runFake(t, f, nil, nil)
	f.typed <- []byte{0x1b}
	waitRun(t, done, 2*time.Second)
	if got := f.log(); !reflect.DeepEqual(tail(got, 4), []string{"stop keys", "discard true", "cooked", "leave"}) {
		t.Fatalf("calls %q", got)
	}
	// A paste mark only begun when a signal ends the screen is dropped the same way.
	f, stop := newFakeConvoTerm(), make(chan struct{})
	done = runFake(t, f, nil, stop)
	f.typed <- []byte("\x1b[20")
	time.Sleep(20 * time.Millisecond)
	close(stop)
	waitRun(t, done, 2*time.Second)
	if got := f.log(); !reflect.DeepEqual(tail(got, 4), []string{"stop keys", "discard true", "cooked", "leave"}) {
		t.Fatalf("calls after a stop %q", got)
	}
}

// A frame goes out in pieces no bigger than convoPiece.
func TestConvoScreenWritesInPieces(t *testing.T) {
	noExit(t)
	f := &piecesTerm{fakeConvoTerm: newFakeConvoTerm()}
	c := screenOf(false, 80, 24, screenTurns(2, nil))
	done := make(chan error, 1)
	go func() { done <- runConvoScreen(c, f, nil, nil, nil) }()
	for start := time.Now(); ; time.Sleep(time.Millisecond) {
		f.mu.Lock()
		n := f.pieces
		f.mu.Unlock()
		if n >= 2 || time.Since(start) > 2*time.Second {
			break
		}
	}
	f.typed <- []byte("q")
	waitRun(t, done, 2*time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pieces < 2 || f.biggest > convoPiece {
		t.Fatalf("%d pieces, the biggest %d bytes", f.pieces, f.biggest)
	}
}

// piecesTerm notes the size of each frame write.
type piecesTerm struct {
	*fakeConvoTerm
	pieces, biggest int
}

func (p *piecesTerm) write(s string) error {
	if s != asksTerminalLeave && !strings.HasPrefix(s, asksTerminalEnter) {
		p.mu.Lock()
		p.pieces++
		p.biggest = max(p.biggest, len(s))
		p.mu.Unlock()
	}
	return p.fakeConvoTerm.write(s)
}

// A copy in progress gives up when the screen is told to stop, or a key quits it.
func TestConvoScreenStopEndsACopy(t *testing.T) {
	for name, end := range convoEnds {
		t.Run(name, func(t *testing.T) {
			saved, savedExit, savedCopier := convoLeaveWait, convoExit, asksCopier
			exited := make(chan int, 1)
			convoLeaveWait, convoExit = 2*time.Second, func(code int) { exited <- code }
			release, copying := make(chan struct{}), make(chan struct{})
			asksCopier = func(ctx context.Context, text string) (string, error) {
				close(copying)
				select {
				case <-ctx.Done():
					return "", ctx.Err()
				case <-release:
					return "", errors.New("released")
				}
			}
			t.Cleanup(func() { convoLeaveWait, convoExit, asksCopier = saved, savedExit, savedCopier })
			defer close(release)
			f, stop := newFakeConvoTerm(), make(chan struct{})
			done := runFake(t, f, nil, stop)
			f.typed <- []byte("y")
			<-copying
			start := time.Now()
			end(f, stop)
			waitRun(t, done, time.Second)
			if d := time.Since(start); d > 500*time.Millisecond {
				t.Fatalf("the screen took %v to end", d)
			}
			select {
			case <-exited:
				t.Fatal("exited")
			default:
			}
		})
	}
}

// Ctrl-C during a copy stops the clipboard tool running, and no other is tried after it.
func TestConvoScreenCtrlCStopsTheClipboard(t *testing.T) {
	noExit(t)
	dir := t.TempDir()
	started, fallback := filepath.Join(dir, "started"), filepath.Join(dir, "fallback")
	for name, script := range map[string]string{
		"pbcopy":  "#!/bin/sh\n: > " + started + "\nexec /bin/sleep 10\n",
		"wl-copy": "#!/bin/sh\n: > " + fallback + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMUX", "")
	t.Setenv("SSH_CONNECTION", "")
	saved := asksCopier
	var term bytes.Buffer
	asksCopier = func(ctx context.Context, text string) (string, error) { return copyText(ctx, text, &term) }
	t.Cleanup(func() { asksCopier = saved })
	f := newFakeConvoTerm()
	done := runFake(t, f, nil, nil)
	f.typed <- []byte("y")
	for start := time.Now(); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("the clipboard tool did not start")
		}
	}
	start := time.Now()
	f.typed <- []byte{3}
	waitRun(t, done, 2*time.Second)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("the screen took %v to end", d)
	}
	if _, err := os.Stat(fallback); err == nil {
		t.Fatal("another clipboard tool ran")
	}
	if term.Len() != 0 {
		t.Fatalf("the terminal was asked: %q", term.String())
	}
}

func TestDrainPaste(t *testing.T) {
	script := func(chunks ...string) (func(time.Duration) []byte, *int) {
		n := 0
		return func(time.Duration) []byte {
			n++
			if n > len(chunks) {
				return nil
			}
			return []byte(chunks[n-1])
		}, &n
	}
	read, n := script("abc", "def\x1b[2", "01~shell", "later")
	drainPaste(read, time.Second, time.Second)
	if *n != 3 {
		t.Fatalf("%d reads past a split end mark; want 3", *n)
	}
	read, n = script("abc")
	drainPaste(read, time.Second, time.Second)
	if *n != 2 {
		t.Fatalf("%d reads; want 2, the second quiet", *n)
	}
	reads := 0
	start := time.Now()
	drainPaste(func(d time.Duration) []byte {
		if reads++; reads > 1e7 {
			t.Fatal("no end")
		}
		return []byte("x")
	}, time.Second, 20*time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatal("a paste that never ends held the way out")
	}
}

// A copy asked for after the screen stopped tries no clipboard, and asks no terminal.
func TestCopyTextStopped(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var term bytes.Buffer
	if _, err := copyText(ctx, "text", &term); err == nil {
		t.Fatalf("err %v", err)
	}
	if term.Len() != 0 {
		t.Fatalf("the terminal got %q", term.String())
	}
	if where, err := copyText(context.Background(), "text", &term); err != nil || where != "terminal clipboard" || term.Len() == 0 {
		t.Fatalf("not stopped: %q %v", where, err)
	}
}

// A panic the --follow loop had goes on once the screen has put the terminal back.
func TestRepanic(t *testing.T) {
	plain := errors.New("plain")
	if err := repanic(plain); err != plain {
		t.Fatalf("got %v", err)
	}
	defer func() {
		p, ok := recover().(convoPanic)
		if !ok || p.value != "boom" || !strings.Contains(p.Error(), "boom") {
			t.Fatalf("recovered %v", p)
		}
	}()
	repanic(convoPanic{value: "boom", stack: []byte("stack")})
	t.Fatal("no panic")
}
