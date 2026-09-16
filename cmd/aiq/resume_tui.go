package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/transcript"
)

// The resume browser has three screens: the directory's sessions, one
// session's turns, and one turn in full. It draws with plain ANSI escapes
// on the alternate screen.
const (
	viewSessions = iota
	viewTurns
	viewTurn
)

type browser struct {
	dir     string
	all     []transcript.Session
	live    map[string]liveNote
	origins map[string]origin
	workers bool
	now     time.Time
	bare    bool // the pick resumes without its launcher (b)

	list []transcript.Session // what the session screen shows
	view int
	sel  int // selected session
	top  int
	tsel int // selected turn
	ttop int
	dtop int // first line of the turn screen

	confirm     string // the key a pending "press it again" question waits for
	confirmText string
	w, h        int
}

func newBrowser(dir string, all []transcript.Session, live map[string]liveNote, workers bool) *browser {
	b := &browser{dir: dir, all: all, live: live, workers: workers, now: time.Now()}
	b.filter()
	return b
}

func (b *browser) filter() {
	var keep string
	if b.sel < len(b.list) {
		keep = b.list[b.sel].ID
	}
	b.list = b.all
	if !b.workers {
		b.list = interactiveOnly(b.all)
	}
	b.sel, b.top = 0, 0
	for i, s := range b.list {
		if s.ID == keep {
			b.sel = i
		}
	}
}

func (b *browser) origin(s *transcript.Session) *origin {
	return originOf(b.origins, *s)
}

func (b *browser) launcherOf(s *transcript.Session) string {
	if org := b.origin(s); org != nil {
		return org.launch.Launcher
	}
	return ""
}

func (b *browser) hiddenWorkers() int {
	if b.workers {
		return 0
	}
	return len(b.all) - len(b.list)
}

func (b *browser) current() *transcript.Session {
	if b.sel < 0 || b.sel >= len(b.list) {
		return nil
	}
	return &b.list[b.sel]
}

// key names for the input the browser understands.
const (
	keyUp = iota + 1
	keyDown
	keyLeft
	keyRight
	keyPgUp
	keyPgDn
	keyHome
	keyEnd
	keyEnter
	keyBack
	keyQuit
)

type keypress struct {
	code int
	r    rune
}

// parseKeys splits one read from the terminal into key presses.
func parseKeys(buf []byte) []keypress {
	var out []keypress
	for len(buf) > 0 {
		c := buf[0]
		switch {
		case c == 0x1b && len(buf) == 1:
			out = append(out, keypress{code: keyBack})
			buf = buf[1:]
		case c == 0x1b && (buf[1] == '[' || buf[1] == 'O'):
			// CSI / SS3: parameters, then a final byte in @..~
			i := 2
			for i < len(buf) && (buf[i] < 0x40 || buf[i] > 0x7e) {
				i++
			}
			if i >= len(buf) {
				return out
			}
			seq := string(buf[2:i])
			switch buf[i] {
			case 'A':
				out = append(out, keypress{code: keyUp})
			case 'B':
				out = append(out, keypress{code: keyDown})
			case 'C':
				out = append(out, keypress{code: keyRight})
			case 'D':
				out = append(out, keypress{code: keyLeft})
			case 'H':
				out = append(out, keypress{code: keyHome})
			case 'F':
				out = append(out, keypress{code: keyEnd})
			case '~':
				switch seq {
				case "5":
					out = append(out, keypress{code: keyPgUp})
				case "6":
					out = append(out, keypress{code: keyPgDn})
				case "1", "7":
					out = append(out, keypress{code: keyHome})
				case "4", "8":
					out = append(out, keypress{code: keyEnd})
				}
			}
			buf = buf[i+1:]
		case c == 0x1b:
			out = append(out, keypress{code: keyBack}) // alt+key: treat as escape
			buf = buf[1:]
		case c == '\r' || c == '\n':
			out = append(out, keypress{code: keyEnter})
			buf = buf[1:]
		case c == 0x7f || c == 0x08:
			out = append(out, keypress{code: keyBack})
			buf = buf[1:]
		case c == 0x03 || c == 0x04:
			out = append(out, keypress{code: keyQuit})
			buf = buf[1:]
		case c == 0x02:
			out = append(out, keypress{code: keyPgUp})
			buf = buf[1:]
		case c == 0x06:
			out = append(out, keypress{code: keyPgDn})
			buf = buf[1:]
		default:
			r, n := utf8.DecodeRune(buf)
			switch r {
			case 'k':
				out = append(out, keypress{code: keyUp})
			case 'j':
				out = append(out, keypress{code: keyDown})
			case 'h':
				out = append(out, keypress{code: keyLeft})
			case 'l':
				out = append(out, keypress{code: keyRight})
			case 'g':
				out = append(out, keypress{code: keyHome})
			case 'G':
				out = append(out, keypress{code: keyEnd})
			case ' ':
				out = append(out, keypress{code: keyPgDn})
			default:
				out = append(out, keypress{r: r})
			}
			buf = buf[n:]
		}
	}
	return out
}

// handle applies one key. It returns done when the browser should close,
// with the session to resume (nil to just quit).
func (b *browser) handle(k keypress) (pick *transcript.Session, done bool) {
	confirm := b.confirm
	b.confirm = ""
	if k.code == keyQuit || k.r == 'q' {
		return nil, true
	}
	if k.r == 'r' || k.r == 'R' || k.r == 'b' {
		s := b.current()
		if s == nil {
			return nil, false
		}
		bare := k.r == 'b'
		if bare && b.launcherOf(s) == "" {
			return nil, false // nothing to leave out
		}
		if n, ok := b.live[s.ID]; ok && n.tmux == "" && confirm != string(k.r) {
			b.confirm = string(k.r)
			b.confirmText = fmt.Sprintf("this session is %s — press %c again to open a second copy", n.note, k.r)
			return nil, false
		}
		b.bare = bare
		return s, true
	}
	page := b.h - 5
	if page < 1 {
		page = 1
	}
	switch b.view {
	case viewSessions:
		switch {
		case k.code == keyUp:
			b.sel--
		case k.code == keyDown:
			b.sel++
		case k.code == keyPgUp:
			b.sel -= page
		case k.code == keyPgDn:
			b.sel += page
		case k.code == keyHome:
			b.sel = 0
		case k.code == keyEnd:
			b.sel = len(b.list) - 1
		case k.code == keyEnter || k.code == keyRight:
			if b.current() != nil {
				b.view, b.tsel, b.ttop = viewTurns, 0, 0
				// Most people come back for how it ended.
				if n := len(b.current().Turns); n > 0 {
					b.tsel = n - 1
				}
			}
		case k.code == keyBack:
			return nil, true
		case k.r == 'a' || k.r == 'w':
			b.workers = !b.workers
			b.filter()
		}
		b.sel = clamp(b.sel, 0, len(b.list)-1)
	case viewTurns:
		turns := len(b.current().Turns)
		switch {
		case k.code == keyUp:
			b.tsel--
		case k.code == keyDown:
			b.tsel++
		case k.code == keyPgUp:
			b.tsel -= page / 3
		case k.code == keyPgDn:
			b.tsel += page / 3
		case k.code == keyHome:
			b.tsel = 0
		case k.code == keyEnd:
			b.tsel = turns - 1
		case k.code == keyEnter || k.code == keyRight:
			b.view, b.dtop = viewTurn, 0
		case k.code == keyBack || k.code == keyLeft:
			b.view = viewSessions
		}
		b.tsel = clamp(b.tsel, 0, turns-1)
	case viewTurn:
		turns := len(b.current().Turns)
		switch {
		case k.code == keyUp:
			b.dtop--
		case k.code == keyDown:
			b.dtop++
		case k.code == keyPgUp:
			b.dtop -= page
		case k.code == keyPgDn:
			b.dtop += page
		case k.code == keyHome:
			b.dtop = 0
		case k.code == keyEnd:
			b.dtop = 1 << 30
		case k.r == 'n' || k.code == keyRight:
			if b.tsel < turns-1 {
				b.tsel, b.dtop = b.tsel+1, 0
			}
		case k.r == 'p' || k.code == keyLeft:
			if b.tsel > 0 {
				b.tsel, b.dtop = b.tsel-1, 0
			}
		case k.code == keyBack || k.code == keyEnter:
			b.view = viewTurns
		}
	}
	return nil, false
}

func clamp(v, lo, hi int) int {
	if v > hi {
		v = hi
	}
	if v < lo {
		v = lo
	}
	return v
}

// Styling.
const (
	sgrReset   = "\x1b[0m"
	sgrBold    = "\x1b[1m"
	sgrDim     = "\x1b[2m"
	sgrReverse = "\x1b[7m"
	sgrCyan    = "\x1b[36m"
	sgrYellow  = "\x1b[33m"
	sgrGreen   = "\x1b[32m"
)

// line is one screen row: plain text plus the style it is drawn in. Keeping
// the text free of escapes lets truncation count cells correctly.
type line struct {
	text  string
	style string
}

// render lays out the current screen as exactly h rows.
func (b *browser) render() []line {
	var head, body, foot []line
	switch b.view {
	case viewSessions:
		head, body, foot = b.renderSessions()
	case viewTurns:
		head, body, foot = b.renderTurns()
	case viewTurn:
		head, body, foot = b.renderTurn()
	}
	if b.confirm != "" {
		foot = []line{{" " + b.confirmText, sgrYellow + sgrBold}}
	}
	rows := b.h - len(head) - len(foot)
	if rows < 0 {
		rows = 0
	}
	if len(body) > rows {
		body = body[:rows]
	}
	out := append([]line{}, head...)
	out = append(out, body...)
	for len(out) < b.h-len(foot) {
		out = append(out, line{})
	}
	out = append(out, foot...)
	if len(out) > b.h {
		out = out[:max(b.h, 0)] // a terminal too short for the chrome
	}
	return out
}

func (b *browser) renderSessions() (head, body, foot []line) {
	title := fmt.Sprintf(" aiq resume · %s · %d session%s", homeRel(b.dir), len(b.list), plural(len(b.list)))
	if n := b.hiddenWorkers(); n > 0 {
		title += fmt.Sprintf(" · %d worker%s hidden", n, plural(n))
	}
	agentW := 20
	for _, s := range b.list {
		if w := cells(agentLabel(s)); w > agentW && w <= 26 {
			agentW = w
		}
	}
	header := fmt.Sprintf("   %-14s %s %5s %5s  %s", "UPDATED", pad("AGENT", agentW), "TURNS", "TOOLS", "NAME / FIRST PROMPT")
	head = []line{{title, sgrBold}, {}, {header, sgrDim}}
	rows := b.h - len(head) - 2
	if len(b.list) == 0 {
		body = []line{{"   no sessions in this directory", sgrDim}}
		if b.hiddenWorkers() > 0 {
			body = append(body, line{"   press a to show worker sessions", sgrDim})
		}
	}
	b.top = scrollTo(b.top, b.sel, 1, rows)
	for i := b.top; i < len(b.list) && i < b.top+rows; i++ {
		s := b.list[i]
		mark := "  "
		style := ""
		if _, ok := b.live[s.ID]; ok {
			mark = "● "
			style = sgrGreen
		}
		label := transcript.Clean(s.Label())
		if tag := originTag(b.origin(&s)); tag != "" {
			label = "[" + tag + "] " + label
		}
		if s.Worker {
			label = "[worker] " + label
		}
		text := fmt.Sprintf(" %s%-14s %s %5d %5d  %s", mark, whenLabel(s.Updated, b.now), pad(agentLabel(s), agentW), len(s.Turns), s.Tools(), label)
		if i == b.sel {
			style = sgrReverse
		}
		body = append(body, line{text, style})
	}
	keys := " ↑↓ move · enter turns · " + b.resumeKeys() + " · a " + map[bool]string{true: "hide", false: "show"}[b.workers] + " workers · q quit"
	foot = []line{{}, {keys, sgrDim}}
	if s := b.current(); s != nil {
		if n, ok := b.live[s.ID]; ok {
			foot[0] = line{" ● " + n.note, sgrGreen}
		}
	}
	return head, body, foot
}

// resumeKeys names the resume keys for the selected session.
func (b *browser) resumeKeys() string {
	s := b.current()
	if s == nil {
		return "r resume"
	}
	if l := b.launcherOf(s); l != "" {
		return "r resume via " + l + " · b resume bare"
	}
	return "r resume"
}

func (b *browser) sessionHeader(s *transcript.Session) []line {
	meta := []string{s.Provider, shortID(s.ID)}
	if s.Model != "" {
		meta = append(meta, strings.TrimPrefix(agentLabel(*s), s.Provider+" "))
	}
	if s.Branch != "" {
		meta = append(meta, "⎇ "+s.Branch)
	}
	if s.Bypass {
		meta = append(meta, "bypass")
	}
	if tag := originTag(b.origin(s)); tag != "" {
		meta = append(meta, tag)
	}
	meta = append(meta, fmt.Sprintf("%s → %s", whenLabel(s.Started, b.now), whenLabel(s.Updated, b.now)))
	meta = append(meta, fmt.Sprintf("%d turn%s, %d tool%s", len(s.Turns), plural(len(s.Turns)), s.Tools(), plural(s.Tools())))
	out := []line{{" " + transcript.Clean(s.Label()), sgrBold}, {" " + strings.Join(meta, " · "), sgrDim}}
	if n, ok := b.live[s.ID]; ok {
		out = append(out, line{" ● " + n.note, sgrGreen})
	}
	return append(out, line{})
}

func (b *browser) renderTurns() (head, body, foot []line) {
	s := b.current()
	head = b.sessionHeader(s)
	rows := b.h - len(head) - 2
	const per = 3
	b.ttop = scrollTo(b.ttop, b.tsel, per, rows)
	for i := b.ttop; i < len(s.Turns) && (i-b.ttop+1)*per <= rows; i++ {
		t := s.Turns[i]
		cursor, numStyle := "  ", sgrCyan
		if i == b.tsel {
			cursor, numStyle = "▸ ", sgrReverse
		}
		info := fmt.Sprintf(" %s#%d  %s · %s", cursor, i+1, t.Started.Local().Format("Jan 2 15:04"), durationLabel(t.Ended.Sub(t.Started)))
		if t.Tools > 0 {
			info += fmt.Sprintf(" · %d tool%s", t.Tools, plural(t.Tools))
		}
		reply := transcript.Clean(t.Reply)
		if reply == "" {
			reply = "(no reply)"
		}
		body = append(body,
			line{info, numStyle},
			line{"     › " + transcript.Clean(t.Prompt), sgrBold},
			line{"     ‹ " + reply, ""},
		)
	}
	foot = []line{{}, {" ↑↓ move · enter read · " + b.resumeKeys() + " · esc back · q quit", sgrDim}}
	return head, body, foot
}

func (b *browser) renderTurn() (head, body, foot []line) {
	s := b.current()
	t := s.Turns[b.tsel]
	head = []line{
		{fmt.Sprintf(" %s · turn %d of %d", transcript.Clean(s.Label()), b.tsel+1, len(s.Turns)), sgrBold},
		{fmt.Sprintf(" %s · %s · %d tool%s", t.Started.Local().Format("Mon Jan 2 15:04"), durationLabel(t.Ended.Sub(t.Started)), t.Tools, plural(t.Tools)), sgrDim},
		{},
	}
	width := b.w - 4
	var text []line
	text = append(text, line{" YOU", sgrCyan + sgrBold})
	for _, l := range wrap(t.Prompt, width) {
		text = append(text, line{"   " + l, ""})
	}
	text = append(text, line{}, line{" " + strings.ToUpper(s.Provider), sgrCyan + sgrBold})
	reply := t.Reply
	if reply == "" {
		reply = "(no reply)"
	}
	for _, l := range wrap(reply, width) {
		text = append(text, line{"   " + l, ""})
	}
	rows := b.h - len(head) - 2
	b.dtop = clamp(b.dtop, 0, max(len(text)-rows, 0))
	body = text[b.dtop:]
	pos := ""
	if len(text) > rows {
		pos = fmt.Sprintf(" · %d%%", min(100, (b.dtop+rows)*100/len(text)))
	}
	foot = []line{{}, {" ↑↓ scroll · ←→ turn · " + b.resumeKeys() + " · esc back · q quit" + pos, sgrDim}}
	return head, body, foot
}

// scrollTo keeps item sel (each per rows tall) inside a window of rows.
func scrollTo(top, sel, per, rows int) int {
	visible := rows / per
	if visible < 1 {
		visible = 1
	}
	if sel < top {
		top = sel
	}
	if sel >= top+visible {
		top = sel - visible + 1
	}
	if top < 0 {
		top = 0
	}
	return top
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// runeCells is how many terminal cells a rune takes: 0 for combining marks
// and joiners, 2 for wide East Asian and emoji ranges, else 1.
func runeCells(r rune) int {
	switch {
	case r == 0x200d || r == 0xfe0f || (r >= 0x200b && r <= 0x200f) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, r >= 0xac00 && r <= 0xd7a3,
		r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
		r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd,
		r >= 0x2600 && r <= 0x27bf && unicode.Is(unicode.So, r) && emojiPresentation(r):
		return 2
	}
	return 1
}

// emojiPresentation covers the dingbats and symbols terminals draw wide.
func emojiPresentation(r rune) bool {
	switch r {
	case 0x2614, 0x2615, 0x2648, 0x2649, 0x264a, 0x264b, 0x264c, 0x264d, 0x264e, 0x264f,
		0x2650, 0x2651, 0x2652, 0x2653, 0x267f, 0x2693, 0x26a1, 0x26aa, 0x26ab, 0x26bd,
		0x26be, 0x26c4, 0x26c5, 0x26ce, 0x26d4, 0x26ea, 0x26f2, 0x26f3, 0x26f5, 0x26fa,
		0x26fd, 0x2705, 0x270a, 0x270b, 0x2728, 0x274c, 0x274e, 0x2753, 0x2754, 0x2755,
		0x2757, 0x2795, 0x2796, 0x2797, 0x27b0, 0x27bf:
		return true
	}
	return false
}

func cells(s string) int {
	n := 0
	for _, r := range s {
		n += runeCells(r)
	}
	return n
}

// truncate cuts s to at most w cells, ending in … when it had to cut.
func truncate(s string, w int) string {
	if cells(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		c := runeCells(r)
		if used+c > w-1 {
			break
		}
		b.WriteRune(r)
		used += c
	}
	return b.String() + "…"
}

// pad truncates or right-pads s to exactly w cells.
func pad(s string, w int) string {
	s = truncate(s, w)
	return s + strings.Repeat(" ", w-cells(s))
}

// wrap breaks text into rows of at most w cells, keeping its own line
// breaks and splitting words longer than a row.
func wrap(text string, w int) []string {
	if w < 10 {
		w = 10
	}
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(text, "\t", "    "), "\n") {
		para = strings.TrimRight(para, " \r")
		indent := para[:len(para)-len(strings.TrimLeft(para, " "))]
		if cells(indent) > w/2 {
			indent = ""
		}
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		cur, used := indent, cells(indent)
		for _, word := range words {
			wc := cells(word)
			if used > cells(indent) && used+1+wc > w {
				out = append(out, cur)
				cur, used = indent, cells(indent)
			}
			for wc > w-cells(indent) {
				// A long URL or path: hard-split it.
				head := truncateExact(word, w-used)
				if head == "" {
					out = append(out, cur)
					cur, used = indent, cells(indent)
					head = truncateExact(word, w-used)
				}
				out = append(out, cur+head)
				cur, used = indent, cells(indent)
				word = word[len(head):]
				wc = cells(word)
			}
			if used > cells(indent) {
				cur += " "
				used++
			}
			cur += word
			used += wc
		}
		out = append(out, cur)
	}
	return out
}

// truncateExact returns the longest prefix of s that fits in w cells.
func truncateExact(s string, w int) string {
	used := 0
	for i, r := range s {
		c := runeCells(r)
		if used+c > w {
			return s[:i]
		}
		used += c
	}
	return s
}

// draw paints the rows, each cut to the terminal width.
func (b *browser) draw(out *os.File) {
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	for i, l := range b.render() {
		text := l.text
		if l.style == sgrReverse {
			text = pad(text, b.w) // highlight the full row
		} else {
			text = truncate(text, b.w)
		}
		if l.style != "" {
			sb.WriteString(l.style + text + sgrReset)
		} else {
			sb.WriteString(text)
		}
		sb.WriteString("\x1b[K")
		if i < b.h-1 {
			sb.WriteString("\r\n")
		}
	}
	sb.WriteString("\x1b[J")
	out.WriteString(sb.String())
}

// run shows the browser until the user quits or picks a session.
func (b *browser) run() (*transcript.Session, error) {
	in, out := os.Stdin, os.Stdout
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	out.WriteString("\x1b[?1049h\x1b[?25l")
	restore := func() {
		out.WriteString("\x1b[?25h\x1b[?1049l")
		term.Restore(int(in.Fd()), old)
	}
	defer restore()

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	keys := make(chan []byte)
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := in.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- append([]byte(nil), buf[:n]...)
		}
	}()

	resize := func() {
		b.w, b.h, err = term.GetSize(int(out.Fd()))
		if err != nil || b.w <= 0 || b.h <= 0 {
			b.w, b.h = 80, 24
		}
	}
	resize()
	b.draw(out)
	for {
		select {
		case <-winch:
			resize()
		case buf, ok := <-keys:
			if !ok {
				return nil, nil
			}
			for _, k := range parseKeys(buf) {
				if pick, done := b.handle(k); done {
					return pick, nil
				}
			}
		}
		b.draw(out)
	}
}
