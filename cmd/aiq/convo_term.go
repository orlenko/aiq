package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/orlenko/aiq/internal/tmux"
)

// convoPalette is every colour aiq convo uses, chosen once for the
// terminal's background.
type convoPalette struct {
	band   string // background behind what the person typed
	you    string // header of what the person typed, on the band
	agent  string // header of an answer
	dim    string // earlier answers, notices, table borders, URLs, the status line
	accent string // the working line and the "moved to" divider
	code   string // inline code and code blocks
}

var (
	// darkPalette is for a dark background, and the default.
	darkPalette = convoPalette{band: sgrBand, you: sgrBoldCyan, agent: sgrBoldGreen, dim: sgrDim, accent: sgrYellow, code: sgrCode}
	// lightPalette keeps text at 4.5:1 or better against white (the you
	// header against its grey band: 5.1:1), where bright cyan, yellow
	// and green wash out.
	lightPalette = convoPalette{
		band:   "\x1b[48;5;254m",
		you:    "\x1b[1;38;5;25m",
		agent:  "\x1b[1;38;5;22m",
		dim:    "\x1b[38;5;242m",
		accent: "\x1b[38;5;94m",
		code:   "\x1b[38;5;130m",
	}
	// pal is the palette in use; cmdConvo picks it before anything prints.
	pal = darkPalette
)

// themeChoice is --theme (already checked), else AIQ_THEME, else auto. A
// mistyped AIQ_THEME is reported once on warn and read as auto.
func themeChoice(flag, env string, warn io.Writer) string {
	if flag != "" {
		return flag
	}
	switch v := strings.ToLower(strings.TrimSpace(env)); v {
	case "", "auto", "light", "dark":
		return v
	}
	fmt.Fprintf(warn, "aiq: ignoring AIQ_THEME=%s; use auto, light or dark\n", env)
	return ""
}

// choosePalette settles the theme: light or dark as asked, else what the
// terminal says its background is (OSC 11), else the theme tmux learned
// from the terminals showing pane, else $COLORFGBG, else dark.
func choosePalette(choice, pane string, stop <-chan struct{}) convoPalette {
	light := false
	switch choice {
	case "light":
		light = true
	case "dark":
	default:
		light = detectLight(pane, stop)
	}
	if light {
		return lightPalette
	}
	return darkPalette
}

func detectLight(pane string, stop <-chan struct{}) bool {
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		if light, ok := queryBackground(stop); ok {
			return light
		}
	}
	// tmux does not answer OSC 11 in a popup, but knows the theme its
	// clients' terminals reported. Only the clients of the pane's own
	// session count; without a pane there is no telling which they are.
	if pane == "" {
		pane = os.Getenv("TMUX_PANE")
	}
	if pane != "" {
		if light, ok := clientTheme(tmux.ClientThemes, pane, os.Getenv("AIQ_TMUX_CLIENT")); ok {
			return light
		}
	}
	light, _ := colorFGBG(os.Getenv("COLORFGBG"))
	return light
}

// clientTheme is the theme of the clients showing pane: the one named
// prefer when it reported one, else the first that did.
func clientTheme(list func(target string) ([]tmux.ClientTheme, error), pane, prefer string) (light, ok bool) {
	clients, err := list(pane)
	if err != nil {
		return false, false
	}
	theme := ""
	for _, c := range clients {
		if c.Theme == "" {
			continue
		}
		if theme == "" {
			theme = c.Theme
		}
		if prefer != "" && c.Client == prefer {
			theme = c.Theme
			break
		}
	}
	switch theme {
	case "light":
		return true, true
	case "dark":
		return false, true
	}
	return false, false
}

// colorFGBG reads $COLORFGBG ("fg;bg", some terminals "fg;default;bg"): a
// background of 7 or 15 is light, 0 to 6 or 8 dark; anything else says
// nothing.
func colorFGBG(v string) (light, ok bool) {
	f := strings.Split(v, ";")
	n, err := strconv.Atoi(f[len(f)-1])
	switch {
	case err != nil:
		return false, false
	case n == 7 || n == 15:
		return true, true
	case n >= 0 && n <= 6 || n == 8:
		return false, true
	}
	return false, false
}

// queryBackground asks the terminal for its background colour (OSC 11) and
// reports whether it is light. Primary Device Attributes (DA1) goes out
// right after it: every terminal answers that, and in order, so once its
// reply is in, an OSC 11 reply has come before it or is not coming, and
// nothing is left to turn up later on the shell's or the pager's input.
// The terminal is in raw mode meanwhile (no echo, keys are bytes, not
// signals); a stop, or a second with no DA1 reply, ends the wait early.
func queryBackground(stop <-chan struct{}) (light, ok bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, false
	}
	defer tty.Close()
	fd := int(tty.Fd())
	saved, err := term.MakeRaw(fd)
	if err != nil {
		return false, false
	}
	defer term.Restore(fd, saved)
	if err := unix.SetNonblock(fd, true); err != nil {
		return false, false
	}
	if _, err := tty.WriteString("\x1b]11;?\x1b\\\x1b[c"); err != nil {
		return false, false
	}
	reply := readUntilDA1(func(wait time.Duration) []byte { return pollRead(fd, wait) }, stop, time.Second)
	r, g, b, ok := parseOSC11(reply)
	if !ok {
		return false, false
	}
	return luminance(r, g, b) > 0.5, true
}

// pollRead returns what fd has to read within wait (nothing if nothing
// came). It waits in select(2): poll(2) does not work on terminals on
// macOS. fd is non-blocking, so a read never waits past what select saw.
func pollRead(fd int, wait time.Duration) []byte {
	var set unix.FdSet
	set.Set(fd)
	tv := unix.NsecToTimeval(wait.Nanoseconds())
	if n, err := unix.Select(fd+1, &set, nil, nil, &tv); err != nil || n == 0 {
		return nil
	}
	buf := make([]byte, 512)
	n, _ := unix.Read(fd, buf)
	return buf[:max(n, 0)]
}

// da1Reply is a Primary Device Attributes answer: ESC [ ? params c.
var da1Reply = regexp.MustCompile("\x1b\\[\\?[0-9;]*c")

// readUntilDA1 collects input until the DA1 reply is in, stop closes, or
// limit passes; then it takes whatever else is already waiting. It returns
// what came before the DA1 reply (or all of it, without one).
func readUntilDA1(read func(wait time.Duration) []byte, stop <-chan struct{}, limit time.Duration) []byte {
	var got []byte
	deadline := time.Now().Add(limit)
	for {
		if loc := da1Reply.FindIndex(got); loc != nil {
			return got[:loc[0]]
		}
		select {
		case <-stop:
			return got
		default:
		}
		left := time.Until(deadline)
		if left <= 0 {
			for b := read(0); len(b) > 0; b = read(0) {
				got = append(got, b...) // drain what turned up
			}
			return got
		}
		got = append(got, read(min(left, 50*time.Millisecond))...)
	}
}

// drain throws away input that is waiting, so none of it reaches the
// shell or a pager afterwards.
func drain(stty func(args ...string) (string, error), r io.Reader) {
	stty("min", "0", "time", "0")
	buf := make([]byte, 256)
	for i := 0; i < 64; i++ {
		if n, _ := r.Read(buf); n == 0 {
			return
		}
	}
}

// parseOSC11 reads "ESC ] 11 ; rgb:RRRR/GGGG/BBBB" ended by BEL or ST, with
// one to four hex digits a component (rgba: carries an alpha, ignored).
func parseOSC11(b []byte) (r, g, bl float64, ok bool) {
	s := string(b)
	i := strings.Index(s, "\x1b]11;")
	if i < 0 {
		return 0, 0, 0, false
	}
	s = s[i+5:]
	end := strings.IndexAny(s, "\a\x1b")
	if end < 0 || s[end] == '\x1b' && !strings.HasPrefix(s[end:], "\x1b\\") {
		return 0, 0, 0, false
	}
	s = s[:end]
	var body string
	switch {
	case strings.HasPrefix(s, "rgb:"):
		body = s[4:]
	case strings.HasPrefix(s, "rgba:"):
		body = s[5:]
	default:
		return 0, 0, 0, false
	}
	parts := strings.Split(body, "/")
	if len(parts) < 3 {
		return 0, 0, 0, false
	}
	var c [3]float64
	for k := 0; k < 3; k++ {
		p := parts[k]
		v, err := strconv.ParseUint(p, 16, 16)
		if err != nil || len(p) < 1 || len(p) > 4 {
			return 0, 0, 0, false
		}
		c[k] = float64(v) / float64(uint64(1)<<(4*len(p))-1)
	}
	return c[0], c[1], c[2], true
}

// luminance weighs the (gamma-encoded) components the way the eye does;
// a middle grey comes out at 0.5.
func luminance(r, g, b float64) float64 {
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// sttyOn runs stty on the terminal f.
func sttyOn(f *os.File, args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = f
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func sttyRun(args ...string) (string, error) { return sttyOn(os.Stdin, args...) }

// muteMode is the terminal --follow keeps: no echo and no line editing,
// so keys typed into it show nowhere; Ctrl-C still quits and output is
// processed as usual. A read gives up after a tenth of a second.
var muteMode = []string{"-icanon", "-echo", "min", "0", "time", "1"}

// mutedInput swallows what is typed while --follow runs: it reads the
// terminal and throws everything away, so nothing looks like it reaches
// the session and nothing is left for the shell when --follow ends.
type mutedInput struct {
	stty  func(args ...string) (string, error)
	r     io.Reader
	saved string // stty -g, to restore
	done  chan struct{}
	gone  chan struct{} // closed when discard has returned
	cont  chan os.Signal
}

func muteInput(stty func(args ...string) (string, error), r io.Reader) (*mutedInput, error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, err
	}
	if _, err := stty(muteMode...); err != nil {
		return nil, err
	}
	m := &mutedInput{stty: stty, r: r, saved: saved, done: make(chan struct{}), gone: make(chan struct{}), cont: make(chan os.Signal, 1)}
	// After Ctrl-Z and fg the shell may have put its own mode back.
	signal.Notify(m.cont, syscall.SIGCONT)
	go m.discard()
	return m, nil
}

func (m *mutedInput) discard() {
	defer close(m.gone)
	buf := make([]byte, 256)
	for {
		select {
		case <-m.done:
			return
		case <-m.cont:
			m.stty(muteMode...)
		default:
		}
		if n, err := m.r.Read(buf); n == 0 || err != nil {
			time.Sleep(50 * time.Millisecond) // timed out, or the terminal is gone
		}
	}
}

// restore drops what is still waiting to be read and puts the terminal
// back as --follow found it. discard has returned first, so a SIGCONT it
// was handling cannot put the muted mode back after this.
func (m *mutedInput) restore() {
	signal.Stop(m.cont)
	close(m.done)
	<-m.gone
	drain(m.stty, m.r)
	m.stty(m.saved)
}

// withMutedInput runs body and restores the terminal however body ends: a
// return, an error, or a panic (deferred calls run while a panic unwinds,
// so the panic keeps its stack). SIGINT, SIGTERM, SIGHUP and SIGQUIT end
// body through its stop channel, so they come here too.
func withMutedInput(m *mutedInput, body func() error) error {
	if m != nil {
		defer m.restore()
	}
	return body()
}
