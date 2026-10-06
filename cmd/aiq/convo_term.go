package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
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

// themeChoice is --theme, else AIQ_THEME, else auto.
func themeChoice(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	switch env := os.Getenv("AIQ_THEME"); env {
	case "", "auto", "light", "dark":
		return env, nil
	default:
		return "", fmt.Errorf("AIQ_THEME is %q; it takes auto, light or dark", env)
	}
}

// choosePalette settles the theme: light or dark as asked, else what the
// terminal says its background is (OSC 11), else $COLORFGBG, else dark.
func choosePalette(choice string) convoPalette {
	light := false
	switch choice {
	case "light":
		light = true
	case "dark":
	default:
		light = detectLight()
	}
	if light {
		return lightPalette
	}
	return darkPalette
}

func detectLight() bool {
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		if light, ok := queryBackground(150 * time.Millisecond); ok {
			return light
		}
	}
	light, _ := colorFGBG(os.Getenv("COLORFGBG"))
	return light
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
// reports whether it is light. The reply comes on the terminal's input, so
// it is read with echo off and line editing off, and whatever is left
// after it (a late reply, a key) is drained before the mode comes back.
// tmux answers in a pane when it knows the colour; in a popup it does not.
func queryBackground(timeout time.Duration) (light, ok bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, false
	}
	defer tty.Close()
	stty := func(args ...string) (string, error) { return sttyOn(tty, args...) }
	saved, err := stty("-g")
	if err != nil {
		return false, false
	}
	defer stty(saved)
	if _, err := stty("-icanon", "-echo", "min", "0", "time", "1"); err != nil {
		return false, false
	}
	if _, err := tty.WriteString("\x1b]11;?\x1b\\"); err != nil {
		return false, false
	}
	reply := readReply(tty.Read, timeout)
	drain(stty, tty)
	r, g, b, ok := parseOSC11(reply)
	if !ok {
		return false, false
	}
	return luminance(r, g, b) > 0.5, true
}

// readReply reads until an OSC reply is complete or timeout has passed.
// read must return within a short while when nothing comes (the terminal
// is in min 0 time 1 mode: a tenth of a second).
func readReply(read func([]byte) (int, error), timeout time.Duration) []byte {
	var got []byte
	buf := make([]byte, 256)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := read(buf)
		got = append(got, buf[:n]...)
		if _, _, _, ok := parseOSC11(got); ok {
			return got
		}
		if n == 0 || err != nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return got
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
	m := &mutedInput{stty: stty, r: r, saved: saved, done: make(chan struct{}), cont: make(chan os.Signal, 1)}
	// After Ctrl-Z and fg the shell may have put its own mode back.
	signal.Notify(m.cont, syscall.SIGCONT)
	go m.discard()
	return m, nil
}

func (m *mutedInput) discard() {
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
// back as --follow found it.
func (m *mutedInput) restore() {
	signal.Stop(m.cont)
	close(m.done)
	drain(m.stty, m.r)
	m.stty(m.saved)
}

// withMutedInput runs body and restores the terminal however body ends: a
// return, an error, or a panic (deferred calls run while a panic unwinds,
// so the panic keeps its stack). SIGINT, SIGTERM and SIGHUP end body
// through its stop channel, so they come here too.
func withMutedInput(m *mutedInput, body func() error) error {
	if m != nil {
		defer m.restore()
	}
	return body()
}
