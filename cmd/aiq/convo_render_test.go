package main

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func renderTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	if value == "" {
		return time.Time{}
	}
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConvoContextTimeInputs(t *testing.T) {
	const childMarker = "AIQ_TEST_CONVO_TIME_CHILD"
	if os.Getenv(childMarker) != "1" {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, self, "-test.run=^TestConvoContextTimeInputs$", "-test.count=1")
		cmd.WaitDelay = 2 * time.Second
		cmd.Env = append(os.Environ(), childMarker+"=1", "TZ=Etc/GMT+5", "AIQ_DATA_DIR="+t.TempDir())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated timezone fixture: %v\n%s", err, out)
		}
		return
	}
	// Starting a separate process selects its local zone without writing the
	// parent's time.Local while another test's screen goroutine may read it.
	_, offset := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).In(time.Local).Zone()
	if offset != -5*3600 {
		t.Fatalf("timezone fixture offset %d, want -18000", offset)
	}
	cases := []struct {
		event, now, head, clock string
		offset                  int
	}{
		{"2026-09-01T23:30:00Z", "2026-09-02T00:10:00Z", "Tue 23:30", "23:30:00", 0},
		{"2026-09-01T23:30:00Z", "2026-09-02T00:10:00Z", "today 01:30", "01:30:00", 2},
		{"2026-09-02T12:00:01Z", "2026-09-08T12:00:00Z", "Wed 12:00", "12:00:01", 0},
		{"2026-09-02T12:00:00Z", "2026-09-08T12:00:00Z", "Sep 2 12:00", "12:00:00", 0},
		{"2025-12-25T12:00:00Z", "2026-01-01T12:00:00Z", "2025-12-25", "12:00:00", 0},
		{"2025-12-31T23:30:00Z", "2026-01-01T00:10:00Z", "today 01:30", "01:30:00", 2},
		{"2026-09-09T12:00:00Z", "2026-09-08T12:00:00Z", "Wed 12:00", "12:00:00", 0},
		{"2026-09-01T23:30:00Z", "", "Tue 23:30", "23:30:00", 0},
		{"", "2026-09-02T00:10:00Z", "", "reply", 0},
	}
	for _, tc := range cases {
		ctx := convoRenderContext{now: renderTestTime(t, tc.now)} // nil location is UTC
		if tc.offset != 0 {
			ctx.location = time.FixedZone("supplied", tc.offset*3600)
		}
		at := renderTestTime(t, tc.event)
		p := newConvoPrinterWithContext("claude", false, false, ctx)
		want := "▌ you"
		if tc.head != "" {
			want += " · " + tc.head
		}
		if got := p.promptHead(at); got != want {
			t.Errorf("%+v: prompt %q, want %q", tc, got, want)
		}
		if got := p.clock(at); got != tc.clock {
			t.Errorf("%+v: clock %q, want %q", tc, got, tc.clock)
		}
		s := &transcript.Session{Turns: []transcript.Turn{{Prompt: transcript.OrchestraNotice, Source: transcript.Notice, Started: at, Open: true}}}
		wantNotice := "orchestra inbox"
		if !at.IsZero() {
			wantNotice += " · " + tc.clock
		}
		if out := p.emit(s); !strings.Contains(out, wantNotice+"\n") {
			t.Errorf("notice %q lacks %q", out, wantNotice)
		}
		p = newConvoPrinterWithContext("claude", false, false, ctx)
		s.Turns[0] = transcript.Turn{Prompt: "p", Started: at, Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "early", At: at}}}
		if out := p.emit(s); !strings.Contains(out, "▌ claude · "+tc.clock+"\n") || !strings.Contains(out, want+"\n") {
			t.Errorf("prompt/early reply %q lacks supplied labels", out)
		}
	}
}

func TestConvoPrinterCallsLiveHeadersForNewTurns(t *testing.T) {
	before := renderTestTime(t, "2026-09-01T23:59:00Z")
	after := renderTestTime(t, "2026-09-02T00:01:00Z")
	p := newConvoPrinter("claude", false, false)
	// Controlled per-instance live callback; source review also checks that the
	// actual CLI adapter installs youHeader rather than a fixed-time closure.
	now := before
	calls := 0
	p.promptHead = func(at time.Time) string {
		calls++
		return "▌ you · " + whenLabelIn(at, now, time.UTC)
	}
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "first", Started: before, Open: true}}}
	if out := p.emit(s); !strings.Contains(out, "▌ you · today 23:59") {
		t.Fatal(out)
	}
	now = after
	s.Turns = append(s.Turns, transcript.Turn{Prompt: "newly seen", Started: before, Open: true})
	if out := p.emit(s); !strings.Contains(out, "▌ you · Tue 23:59") || strings.Contains(out, "today") || calls != 2 {
		t.Fatalf("second emit %q, callback calls %d", out, calls)
	}
	if out := p.emit(s); out != "" || calls != 2 {
		t.Fatalf("repeated emit %q, callback calls %d", out, calls)
	}
}

func TestConvoContextSkippedLabelIsExplicit(t *testing.T) {
	saved := transcript.MaxLine
	defer func() { transcript.MaxLine = saved }()
	cases := []struct {
		s     transcript.Session
		count int
	}{
		{transcript.Session{Skipped: 1, Turns: []transcript.Turn{{Prompt: "p", Open: true}}}, 1},
		{transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Open: true, Skipped: 2}}}, 2},
		{transcript.Session{Turns: []transcript.Turn{{Source: transcript.Peer, PromptSkipped: true, Open: true, Skipped: 1}}}, 1},
	}
	for _, label := range []string{"supplied skipped label", ""} {
		for _, tc := range cases {
			var previous string
			var cuts []convoCut
			for i, limit := range []int{1 << 20, 64 << 20} {
				transcript.MaxLine = limit
				p := newConvoPrinterWithContext("claude", false, false, convoRenderContext{skippedNote: label})
				p.track = true
				out := p.emit(&tc.s)
				count := 0
				for _, cut := range p.cuts {
					if cut.kind == "skipped" || tc.s.Turns[0].PromptSkipped && cut.kind == "you" {
						count++
						if cut.text != label {
							t.Fatalf("cut text %q, want supplied %q", cut.text, label)
						}
					}
				}
				if count != tc.count || strings.Count(out, "  · "+label+"\n") != tc.count {
					t.Fatalf("skipped count %d want %d: %q", count, tc.count, out)
				}
				if i > 0 && (out != previous || !reflect.DeepEqual(p.cuts, cuts)) {
					t.Fatal("ambient parser limit changed output/cuts")
				}
				previous, cuts = out, p.cuts
			}
		}
	}
	transcript.MaxLine = 8 << 20
	p := newConvoPrinter("claude", false, false)
	transcript.MaxLine = 64 << 20
	if out := p.emit(&cases[0].s); !strings.Contains(out, "a record over 8 MB was skipped") || strings.Contains(out, "64 MB") {
		t.Fatal(out)
	}
}

func TestConvoRendererDefaultsAreCaptured(t *testing.T) {
	savedPal, savedMarkup, savedRenderer := pal, convoMarkup, markdownRenderer
	defer func() { pal, convoMarkup, markdownRenderer = savedPal, savedMarkup, savedRenderer }()
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	t.Setenv("COLORTERM", "")
	pal, convoMarkup = lightPalette, false
	markdownRenderer = func(string, int, string, bool) string { panic("private-fixture-panic") }
	failing := newConvoPrinter("claude", true, false)
	markdownRenderer = nil
	healthy := newConvoPrinter("claude", true, false)
	ctx := convoRenderContext{palette: lightPalette, width: 100, liveMark: healthy.liveMark, itemBand: healthy.itemBand,
		renderer: (markdownContext{palette: lightPalette}).renderMarkdown}
	pal, convoMarkup = darkPalette, true
	markdownRenderer = func(string, int, string, bool) string { panic("later-fixture-panic") }
	explicit := newConvoPrinterWithContext("claude", true, false, ctx)
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Reply: "**bold** `code`"}}}
	bad, good, fixed := failing.emit(s), healthy.emit(s), explicit.emit(s)
	if !strings.Contains(bad, "**bold** `code`") || strings.Contains(bad, "fixture-panic") {
		t.Fatalf("captured failing renderer: %q", bad)
	}
	if good != fixed || !strings.Contains(good, lightPalette.code+"code") || strings.Contains(good, "**bold**") {
		t.Fatalf("captured default differs from explicit context:\n%q\n%q", good, fixed)
	}
}

func TestConvoContextResolvedStylesAndWidth(t *testing.T) {
	savedPal, savedMarkup, savedRenderer := pal, convoMarkup, markdownRenderer
	defer func() { pal, convoMarkup, markdownRenderer = savedPal, savedMarkup, savedRenderer }()
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	t.Setenv("COLORTERM", "truecolor")
	pal, convoMarkup = darkPalette, true
	markdownRenderer = func(string, int, string, bool) string { panic("ambient override") }
	palette := convoPalette{band: "\x1b[48;5;109m", you: "\x1b[1;31m", agent: "\x1b[1;32m",
		code: "\x1b[38;5;102m", dim: "\x1b[38;5;101m", accent: "\x1b[38;5;103m"}
	ctx := convoRenderContext{palette: palette, width: 24, liveMark: "@", itemBand: "\x1b[48;5;108m",
		renderer: (markdownContext{palette: palette}).renderMarkdown}
	p := newConvoPrinterWithContext("claude", true, false, ctx)
	ctx.palette, ctx.liveMark, ctx.itemBand, ctx.markup = darkPalette, "!", "", true
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Reply: "`code` **bold**"}}}
	out := p.emit(s)
	if !strings.Contains(out, sgrReset+"\x1b[48;5;108m"+palette.agent+"@ claude") ||
		!strings.Contains(out, palette.code+"code"+sgrReset) || strings.Contains(out, palette.accent) || strings.Contains(out, "**bold**") {
		t.Fatalf("supplied styles/markup not retained: %q", out)
	}
	p.width = 12
	s.Turns = append(s.Turns, transcript.Turn{Prompt: "next", Reply: "reply"})
	want := palette.band + palette.you + "▌ you" + strings.Repeat(" ", 7) + sgrReset + "\n"
	if out := p.emit(s); !strings.Contains(out, want) {
		t.Fatalf("resized human band lacks literal 12-cell row %q: %q", want, out)
	}
	zero := newConvoPrinterWithContext("claude", true, false, convoRenderContext{})
	wantZero := "\n" + sgrReset + " claude" + strings.Repeat(" ", 93) + sgrReset + "\n"
	if out := zero.agentHead("", "▌ claude"); out != wantZero {
		t.Fatalf("zero inputs acquired an ambient band/glyph: %q", out)
	}
}

// Capture all backing capacity, original slice sizes and nilness in owned
// storage. A shallow Session copy cannot detect writes into nested or spare data.
type convoInputSnapshot struct {
	value transcript.Session
	sizes [][6]int
	turns int
}

func fullInputCopy[T any](in []T) []T {
	if in == nil {
		return nil
	}
	return append([]T{}, in[:cap(in)]...)
}

func snapshotConvoInput(s *transcript.Session) convoInputSnapshot {
	out := convoInputSnapshot{value: *s, turns: len(s.Turns)}
	out.value.Turns = fullInputCopy(s.Turns)
	for i := range out.value.Turns {
		v := &out.value.Turns[i]
		out.sizes = append(out.sizes, [6]int{len(v.Items), cap(v.Items), len(v.Steers), cap(v.Steers), len(v.Earlier), cap(v.Earlier)})
		v.Items, v.Steers, v.Earlier = fullInputCopy(v.Items), fullInputCopy(v.Steers), fullInputCopy(v.Earlier)
	}
	return out
}

func renderContextSession(t *testing.T) *transcript.Session {
	at := renderTestTime(t, "2026-09-01T23:30:00Z")
	turns := make([]transcript.Turn, 2, 3)
	items := make([]transcript.TurnItem, 2, 3)
	items[0] = transcript.TurnItem{Kind: transcript.ItemSaid, Text: "**early** [`code`](url)", At: at}
	items[1] = transcript.TurnItem{Kind: transcript.ItemSteer, Text: "steer"}
	items[:cap(items)][2] = transcript.TurnItem{Text: "spare item"}
	turns[0] = transcript.Turn{Prompt: "prompt 日本語\t\x1b]52;x\a", Started: at, Items: items, Reply: "| a | b |\n|---|---|\n| `c` | *d* |", Earlier: []string{}}
	turns[1] = transcript.Turn{Source: transcript.Notice, Prompt: transcript.OrchestraNotice, Started: at, Open: true, Steers: make([]string, 0, 2)}
	turns[1].Steers[:2][1] = "spare steer"
	turns[:cap(turns)][2] = transcript.Turn{Prompt: "spare turn", Earlier: []string{"spare earlier"}}
	return &transcript.Session{Provider: "claude", Turns: turns}
}

func TestConvoContextRendererFailureAndInputOwnership(t *testing.T) {
	s := renderContextSession(t)
	before := snapshotConvoInput(s)
	cases := []struct {
		renderer convoMarkdownRenderer
		color    bool
		raw      bool
	}{
		{nil, true, true},
		{func(string, int, string, bool) string { panic("private-fixture-panic") }, true, true},
		{func(string, int, string, bool) string { return "" }, true, false},
		{func(string, int, string, bool) string { t.Fatal("plain mode invoked renderer"); return "" }, false, true},
	}
	for _, tc := range cases {
		p := newConvoPrinterWithContext("claude", tc.color, false, convoRenderContext{renderer: tc.renderer})
		out := p.emit(s)
		if strings.Contains(out, "**early**") != tc.raw || strings.Contains(out, "fixture-panic") || strings.Contains(out, "\x1b]") || strings.ContainsRune(out, '\a') {
			t.Fatalf("raw=%v, rendered %q", tc.raw, out)
		}
		if !reflect.DeepEqual(snapshotConvoInput(s), before) {
			t.Fatal("emit changed input or spare backing capacity")
		}
	}
}

func TestConvoContextsOwnStateDuringGrowthAndResize(t *testing.T) {
	s := renderContextSession(t)
	before := snapshotConvoInput(s)
	grown := *s
	grown.Turns = append(append([]transcript.Turn{}, s.Turns...), transcript.Turn{Prompt: "new prompt", Reply: "**new answer**"})
	palettes := markdownTestPalettes()
	type emission struct {
		text string
		cuts []convoCut
	}
	run := func(ctx convoRenderContext) []emission {
		p := newConvoPrinterWithContext("claude", true, false, ctx)
		p.track = true
		var out []emission
		for _, input := range []*transcript.Session{s, s, &grown} {
			out = append(out, emission{p.emit(input), append([]convoCut(nil), p.cuts...)})
			p.width = 18 // same printer honors resize before the next emission
		}
		return out
	}
	var contexts []convoRenderContext
	var expected [][]emission
	for i, palette := range palettes {
		ctx := convoRenderContext{palette: palette, markup: i%2 == 0, width: 44,
			liveMark: string(rune('A' + i)), itemBand: "\x1b[48;5;108m", skippedNote: "fixed label",
			now: renderTestTime(t, "2026-09-02T00:10:00Z"), location: time.FixedZone("supplied", i*3600),
			renderer: (markdownContext{palette: palette}).renderMarkdown}
		contexts = append(contexts, ctx)
		expected = append(expected, run(ctx))
		if expected[i][1].text != "" || len(expected[i][1].cuts) != 0 || !strings.Contains(expected[i][2].text, "new prompt") {
			t.Fatal("fixture did not exercise repeated emit and growth")
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, ctx := range contexts {
		wg.Add(1)
		go func(i int, ctx convoRenderContext) {
			defer wg.Done()
			<-start
			for n := 0; n < 20; n++ {
				if !reflect.DeepEqual(run(ctx), expected[i]) {
					t.Errorf("context %d differs during concurrent growth/resize", i)
					return
				}
			}
		}(i, ctx)
	}
	close(start)
	wg.Wait()
	if !reflect.DeepEqual(snapshotConvoInput(s), before) {
		t.Fatal("concurrent rendering changed shared input")
	}
}
