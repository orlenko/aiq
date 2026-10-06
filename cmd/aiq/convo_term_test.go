package main

import (
	"errors"
	"math"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/tmux"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestParseOSC11(t *testing.T) {
	cases := []struct {
		reply   string
		r, g, b float64
		ok      bool
	}{
		{"\x1b]11;rgb:ffff/ffff/ffff\x1b\\", 1, 1, 1, true},
		{"\x1b]11;rgb:fdfd/f6f6/e3e3\a", 0xfdfd / 65535.0, 0xf6f6 / 65535.0, 0xe3e3 / 65535.0, true},
		{"\x1b]11;rgb:00/2b/36\a", 0, 0x2b / 255.0, 0x36 / 255.0, true},
		{"\x1b]11;rgb:f/0/8\x1b\\", 1, 0, 8 / 15.0, true},
		{"\x1b]11;rgba:ffff/ffff/ffff/8000\x1b\\", 1, 1, 1, true},
		{"junk\x1b[0n\x1b]11;rgb:0000/0000/0000\x1b\\", 0, 0, 0, true},
		{"\x1b]11;rgb:ffff/ffff/ffff", 0, 0, 0, false},       // no terminator yet
		{"\x1b]11;rgb:ffff/ffff/ffff\x1b[", 0, 0, 0, false},  // ESC, not ST
		{"\x1b]11;rgb:fffff/0/0\a", 0, 0, 0, false},          // five digits
		{"\x1b]11;rgb:zz/00/00\a", 0, 0, 0, false},           // not hex
		{"\x1b]11;rgb:ff/ff\a", 0, 0, 0, false},              // two components
		{"\x1b]11;?\a", 0, 0, 0, false},                      // our own query
		{"\x1b]10;rgb:ffff/ffff/ffff\x1b\\", 0, 0, 0, false}, // the foreground
		{"", 0, 0, 0, false},
	}
	for _, c := range cases {
		r, g, b, ok := parseOSC11([]byte(c.reply))
		if ok != c.ok || ok && (math.Abs(r-c.r) > 1e-9 || math.Abs(g-c.g) > 1e-9 || math.Abs(b-c.b) > 1e-9) {
			t.Errorf("%q: got %v %v %v %v", c.reply, r, g, b, ok)
		}
	}
}

// fakeTerminal hands out its chunks one read at a time, each once its
// time has come; then nothing.
type fakeTerminal struct {
	start  time.Time
	chunks []timedChunk
	reads  int
}

type timedChunk struct {
	at   time.Duration
	data string
}

func (f *fakeTerminal) read(wait time.Duration) []byte {
	f.reads++
	if len(f.chunks) == 0 || time.Since(f.start) < f.chunks[0].at {
		if wait > 0 {
			time.Sleep(min(wait, 5*time.Millisecond))
		}
		return nil
	}
	c := f.chunks[0]
	f.chunks = f.chunks[1:]
	return []byte(c.data)
}

func TestReadUntilDA1(t *testing.T) {
	const osc = "\x1b]11;rgb:ffff/ffff/ffff\x1b\\"
	const da1 = "\x1b[?62;22c"
	cases := []struct {
		name   string
		chunks []timedChunk
		limit  time.Duration
		want   string
		within time.Duration
	}{
		// A slow terminal answers the colour late; DA1 still comes after
		// it, so the reply is read rather than left on the input.
		{"late reply, then DA1", []timedChunk{{40 * time.Millisecond, osc}, {41 * time.Millisecond, da1}}, time.Second, osc, 500 * time.Millisecond},
		{"no colour, only DA1", []timedChunk{{0, da1}}, time.Second, "", 100 * time.Millisecond},
		{"colour split across reads", []timedChunk{{0, "\x1b]11;rgb:ff"}, {0, "ff/ffff/ff"}, {0, "ff\x1b\\\x1b[?6"}, {0, "2;22c"}}, time.Second, osc, 100 * time.Millisecond},
		// Nothing answers at all: give up at the limit, and take what
		// turned up by then.
		{"nothing", nil, 60 * time.Millisecond, "", 300 * time.Millisecond},
		{"no DA1, a stray key at the limit", []timedChunk{{70 * time.Millisecond, "q"}}, 60 * time.Millisecond, "", 300 * time.Millisecond},
	}
	for _, c := range cases {
		term := &fakeTerminal{start: time.Now(), chunks: c.chunks}
		start := time.Now()
		got := string(readUntilDA1(term.read, nil, c.limit))
		if d := time.Since(start); d > c.within {
			t.Errorf("%s: took %v", c.name, d)
		}
		if c.name == "nothing" && time.Since(start) < c.limit {
			t.Errorf("%s: gave up before the limit", c.name)
		}
		if got != c.want && !(c.name == "no DA1, a stray key at the limit" && (got == "" || got == "q")) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if r, g, b, ok := parseOSC11([]byte(got)); c.want != "" && (!ok || r != 1 || g != 1 || b != 1) {
			t.Errorf("%s: %q does not parse", c.name, got)
		}
	}
	// A signal ends the wait at once.
	stop := make(chan struct{})
	close(stop)
	start := time.Now()
	readUntilDA1((&fakeTerminal{start: time.Now()}).read, stop, time.Second)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("a stop took %v", d)
	}
}

func TestLuminanceThreshold(t *testing.T) {
	cases := map[[3]float64]bool{
		{1, 1, 1}: true,  // white
		{0, 0, 0}: false, // black
		{0xfd / 255.0, 0xf6 / 255.0, 0xe3 / 255.0}: true,  // Solarized light
		{0, 0x2b / 255.0, 0x36 / 255.0}:            false, // Solarized dark
		{0x28 / 255.0, 0x2c / 255.0, 0x34 / 255.0}: false, // One Dark
		{0.45, 0.45, 0.45}:                         false,
		{0.55, 0.55, 0.55}:                         true,
	}
	for c, want := range cases {
		if got := luminance(c[0], c[1], c[2]) > 0.5; got != want {
			t.Errorf("%v: light=%v, want %v", c, got, want)
		}
	}
}

func TestColorFGBG(t *testing.T) {
	cases := map[string][2]bool{ // light, ok
		"0;15":         {true, true},
		"0;7":          {true, true},
		"15;0":         {false, true},
		"7;8":          {false, true},
		"15;default;0": {false, true},
		"0;default;15": {true, true},
		"0;12":         {false, false},
		"default":      {false, false},
		"":             {false, false},
	}
	for v, want := range cases {
		if light, ok := colorFGBG(v); light != want[0] || ok != want[1] {
			t.Errorf("%q: got %v %v, want %v", v, light, ok, want)
		}
	}
}

func TestThemeChoice(t *testing.T) {
	var warn strings.Builder
	cases := []struct{ flag, env, want, warned string }{
		{"dark", "light", "dark", ""}, // the flag wins
		{"", "light", "light", ""},
		{"", "LIGHT", "light", ""},
		{"", " Dark ", "dark", ""},
		{"", "", "", ""},
		{"", "solarized", "", "aiq: ignoring AIQ_THEME=solarized; use auto, light or dark\n"},
	}
	for _, c := range cases {
		warn.Reset()
		if got := themeChoice(c.flag, c.env, &warn); got != c.want || warn.String() != c.warned {
			t.Errorf("flag %q env %q: got %q, warned %q", c.flag, c.env, got, warn.String())
		}
	}
	if _, err := parseConvoArgs([]string{"--theme", "pink"}); err == nil {
		t.Error("a bad --theme passed")
	}
	if o, err := parseConvoArgs([]string{"--theme=Light"}); err != nil || o.theme != "light" {
		t.Errorf("got %+v, %v", o, err)
	}
	if choosePalette("light", "", nil) != lightPalette || choosePalette("dark", "", nil) != darkPalette {
		t.Error("an explicit theme was not taken")
	}
}

// One human block and one answer, as the light palette draws them.
func TestLightPalette(t *testing.T) {
	saved := pal
	defer func() { pal = saved }()
	pal = lightPalette
	at := time.Date(2026, 9, 1, 14, 0, 0, 0, time.Local)
	s := &transcript.Session{Provider: "claude", Turns: []transcript.Turn{
		{Prompt: "check it", Reply: "Run `go test` now.", Started: at, Ended: at.Add(time.Minute)},
	}}
	p := newConvoPrinter("claude", true, false)
	p.width = 20
	want := "\n" +
		"\x1b[48;5;254m\x1b[1;38;5;25m▌ you · " + whenLabel(at, time.Now()) + strings.Repeat(" ", 20-cells("▌ you · "+whenLabel(at, time.Now()))) + "\x1b[0m\n" +
		"\x1b[48;5;254m\x1b[1mcheck it            \x1b[0m\n" +
		"\x1b[48;5;254m                    \x1b[0m\n" +
		"\n" +
		"\x1b[1;38;5;22m▌ claude · 1m\x1b[0m\n" +
		"Run \x1b[38;5;130mgo test\x1b[0m now.\n"
	if got := p.emit(s); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if lightPalette.band == darkPalette.band || darkPalette.band != "\x1b[48;5;236m" {
		t.Error("the band is the same in both themes")
	}
}

// lockedReader is typed input the discard loop and the final drain share.
type lockedReader struct {
	mu sync.Mutex
	s  string
}

func (r *lockedReader) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := copy(b, r.s)
	r.s = r.s[n:]
	return n, nil
}

func (r *lockedReader) left() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

// Whatever --follow ends with, the terminal comes back as it was and what
// was typed into it is gone.
func TestMutedInputRestoresTheTerminal(t *testing.T) {
	for name, body := range map[string]func() error{
		"return": func() error { return nil },
		"error":  func() error { return errors.New("boom") },
		"panic":  func() error { panic("bug") },
	} {
		var stty fakeStty
		typed := &lockedReader{s: "ls -la\n"}
		m, err := muteInput(stty.run, typed)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() { recover() }()
			withMutedInput(m, body)
		}()
		calls := strings.Join(stty.calls(), "|")
		if want := "-g|" + strings.Join(muteMode, " ") + "|min 0 time 0|saved-state"; calls != want {
			t.Errorf("%s: stty calls %q, want %q", name, calls, want)
		}
		if left := typed.left(); left != "" {
			t.Errorf("%s: %q left for the shell", name, left)
		}
	}
}

type fakeStty struct {
	mu  sync.Mutex
	log []string
}

func (f *fakeStty) run(args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, strings.Join(args, " "))
	if len(args) == 1 && args[0] == "-g" {
		return "saved-state", nil
	}
	return "", nil
}

func (f *fakeStty) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func TestClientTheme(t *testing.T) {
	list := func(clients ...tmux.ClientTheme) func(string) ([]tmux.ClientTheme, error) {
		return func(string) ([]tmux.ClientTheme, error) { return clients, nil }
	}
	cases := []struct {
		name      string
		list      func(string) ([]tmux.ClientTheme, error)
		prefer    string
		light, ok bool
	}{
		{"one light", list(ct("/dev/a", "light")), "", true, true},
		{"first that reported", list(ct("/dev/a", ""), ct("/dev/b", "dark"), ct("/dev/c", "light")), "", false, true},
		{"the named client", list(ct("/dev/b", "dark"), ct("/dev/c", "light")), "/dev/c", true, true},
		{"named, but silent", list(ct("/dev/b", "dark"), ct("/dev/c", "")), "/dev/c", false, true},
		{"none reported", list(ct("/dev/a", "")), "", false, false},
		{"no clients", list(), "", false, false},
		{"tmux failed", func(string) ([]tmux.ClientTheme, error) { return nil, errors.New("no server") }, "", false, false},
	}
	for _, c := range cases {
		if light, ok := clientTheme(c.list, "%3", c.prefer); light != c.light || ok != c.ok {
			t.Errorf("%s: got %v %v, want %v %v", c.name, light, ok, c.light, c.ok)
		}
	}
	var asked string
	clientTheme(func(target string) ([]tmux.ClientTheme, error) { asked = target; return nil, nil }, "%7", "")
	if asked != "%7" {
		t.Errorf("asked tmux about %q", asked)
	}
}

func ct(client, theme string) tmux.ClientTheme { return tmux.ClientTheme{Client: client, Theme: theme} }

// A SIGCONT that discard is handling never lands after restore's stty.
func TestMutedInputRestoreWinsOverSIGCONT(t *testing.T) {
	for i := 0; i < 50; i++ {
		var stty fakeStty
		m, err := muteInput(stty.run, &lockedReader{})
		if err != nil {
			t.Fatal(err)
		}
		m.cont <- syscall.SIGCONT
		m.restore()
		calls := stty.calls()
		if calls[len(calls)-1] != "saved-state" {
			t.Fatalf("stty calls end %q", calls)
		}
	}
}
