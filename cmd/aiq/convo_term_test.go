package main

import (
	"errors"
	"math"
	"strings"
	"sync"
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

func TestReadReply(t *testing.T) {
	// The reply arrives in pieces: read stops as soon as it is whole.
	pieces := []string{"\x1b]11;rgb:ff", "ff/ffff/ff", "ff\x1b\\", "after"}
	read := func(b []byte) (int, error) {
		if len(pieces) == 0 {
			return 0, nil
		}
		n := copy(b, pieces[0])
		pieces = pieces[1:]
		return n, nil
	}
	if got := string(readReply(read, time.Second)); got != "\x1b]11;rgb:ffff/ffff/ffff\x1b\\" {
		t.Errorf("got %q", got)
	}
	// Nothing comes: it gives up at the timeout.
	start := time.Now()
	silent := func([]byte) (int, error) { return 0, nil }
	if got := readReply(silent, 60*time.Millisecond); len(got) != 0 {
		t.Errorf("got %q", got)
	}
	if d := time.Since(start); d < 60*time.Millisecond || d > 500*time.Millisecond {
		t.Errorf("gave up after %v", d)
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
	t.Setenv("AIQ_THEME", "light")
	if got, _ := themeChoice("dark"); got != "dark" {
		t.Errorf("the flag lost to the environment: %q", got)
	}
	if got, _ := themeChoice(""); got != "light" {
		t.Errorf("the environment was not read: %q", got)
	}
	t.Setenv("AIQ_THEME", "")
	if got, _ := themeChoice(""); got != "" {
		t.Errorf("got %q", got)
	}
	t.Setenv("AIQ_THEME", "solarized")
	if _, err := themeChoice(""); err == nil {
		t.Error("a bad AIQ_THEME passed")
	}
	if _, err := parseConvoArgs([]string{"--theme", "pink"}); err == nil {
		t.Error("a bad --theme passed")
	}
	if o, err := parseConvoArgs([]string{"--theme=light"}); err != nil || o.theme != "light" {
		t.Errorf("got %+v, %v", o, err)
	}
	if choosePalette("light", "") != lightPalette || choosePalette("dark", "") != darkPalette {
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
