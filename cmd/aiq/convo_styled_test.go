package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func styledTestOptions() convoStyledOptions {
	return convoStyledOptions{width: 10, now: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), location: time.UTC, liveMark: "!", skippedNote: "fixture skipped"}
}

func styledRows(group convoStyledGroup) []string {
	if !group.visible {
		return nil
	}
	var rows []string
	for _, line := range group.lines {
		var row strings.Builder
		for _, span := range line.spans {
			row.WriteString(span.text)
		}
		rows = append(rows, row.String())
	}
	if group.emptyBodyLine {
		rows = append(rows, "")
	}
	return rows
}

func styledGroups(messages []convoStyledMessage) []convoStyledGroup {
	var groups []convoStyledGroup
	for _, message := range messages {
		groups = append(groups, message.groups...)
	}
	return groups
}

// Use the ordinary emitter with NO capture and the existing terminal renderer.
// Strip only terminal SGR and known framing, never reconstruct from folded raw.
func styledOrdinaryBodies(s *transcript.Session, opts convoStyledOptions) ([]convoProjectionMessage, [][]string, []bool) {
	palette := webMarkdownPalette()
	ctx := convoRenderContext{palette: palette, width: opts.width, markup: opts.markup, now: opts.now, location: opts.location, liveMark: opts.liveMark, itemBand: palette.band, skippedNote: opts.skippedNote, renderer: (markdownContext{palette: palette}).renderMarkdown}
	p := newConvoPrinterWithContext(s.Provider, true, false, ctx)
	p.track = true
	p.live = opts.live
	text := p.emit(s)
	var bodies [][]string
	var breaks []bool
	previous := 0
	for _, cut := range p.cuts {
		segment := sgrPattern.ReplaceAllString(text[previous:cut.end], "")
		previous = cut.end
		leading := strings.HasPrefix(segment, "\n")
		breaks = append(breaks, leading)
		if cut.kind == "part" {
			bodies = append(bodies, nil)
			continue
		}
		rows := strings.Split(strings.TrimSuffix(segment, "\n"), "\n")
		if leading {
			rows = rows[1:]
		}
		switch cut.facts.head.mode {
		case "prompt", "steer", "final", "earlier", "reply":
			rows = rows[1:]
		}
		bodies = append(bodies, rows)
	}
	return projectConvoCuts(text, p.cuts, opts.accounts), bodies, breaks
}

func TestConvoStyledLiteralBodiesMatchOrdinaryEmitter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session transcript.Session
		rows    [][]string
		heads   []string
		visible []bool
		raw     string
	}{
		{"human spacing", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "  a\t b  c\n日本語 x\u2028", Open: true}}}, [][]string{{"  a b c   ", "日本語 x  ", "          ", "          "}}, []string{"prompt"}, []bool{true}, ""},
		{"skipped peer prompt", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Source: transcript.Peer, PromptSkipped: true, Skipped: 1, Open: true}}}, [][]string{{"  · fixture skipped"}}, []string{"prompt"}, []bool{true}, "fixture skipped"},
		{"raw control prefix notice", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Source: transcript.Notice, Prompt: "\x1b" + transcript.OrchestraNotice, Open: true}}}, [][]string{{"· notice: Agent Orchestra local inbox notice."}}, []string{"notice"}, []bool{true}, transcript.OrchestraNotice},
		{"actual orchestra", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Source: transcript.Notice, Prompt: transcript.OrchestraNotice, Open: true}}}, [][]string{{"· orchestra inbox"}}, []string{"notice"}, []bool{true}, transcript.OrchestraNotice},
		{"said and invisible copy", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Source: transcript.Peer, Prompt: "notice", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "abcdefghijklmnopqrstuv\nignored copy", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "hidden continuation", MsgID: "m"}}}}}, [][]string{{"· peer agent: notice"}, {"  · abcdefghijklmno…"}, nil}, []string{"notice", "", ""}, []bool{true, true, false}, "abcdefghijklmnopqrstuv\nignored copy\n\nhidden continuation"},
		{"invisible interposition", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Source: transcript.Peer, Prompt: "notice", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "aside", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "hidden", MsgID: "m"}, {Kind: transcript.ItemAnswer, Text: "answer"}}}}}, [][]string{{"· peer agent: notice"}, {"  · aside"}, nil, {"answer"}}, []string{"notice", "", "", "earlier"}, []bool{true, true, false, true}, "answer"},
		{"none", transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "p"}}}, [][]string{{"p         ", "          "}, {"(no reply)"}}, []string{"prompt", "final"}, []bool{true, true}, "p"},
		{"session skipped", transcript.Session{Provider: "fixture", Skipped: 1}, [][]string{{"  · fixture skipped"}}, []string{""}, []bool{true}, "fixture skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := styledTestOptions()
			got, err := renderConvoStyled(&tc.session, opts)
			if err != nil {
				t.Fatal(err)
			}
			plain, oldRows, oldBreaks := styledOrdinaryBodies(&tc.session, opts)
			groups := styledGroups(got)
			if !reflect.DeepEqual(oldRows, tc.rows) || len(groups) != len(tc.rows) {
				t.Fatalf("ordinary literal oracle: got %q want %q", oldRows, tc.rows)
			}
			for i, group := range groups {
				if group.ordinal != i || group.head.mode != tc.heads[i] || group.visible != tc.visible[i] || group.leadingBreak != oldBreaks[i] || group.emptyBodyLine || !reflect.DeepEqual(styledRows(group), tc.rows[i]) {
					t.Fatalf("group %d framing/body mismatch: %+v", i, group)
				}
			}
			for i, message := range got {
				if !reflect.DeepEqual(message.message, plain[i]) {
					t.Fatal("styled adapter changed plain facts/raw/id")
				}
			}
			if tc.raw != "" && got[len(got)-1].message.raw != tc.raw {
				t.Fatal("copy text was reclassified or included a no-reply marker")
			}
			if tc.name == "human spacing" {
				for i, line := range groups[0].lines {
					want := []string{"band", "b"}
					if i == 3 {
						want = []string{"band"}
					}
					if !reflect.DeepEqual(line.spans[0].classes, want) {
						t.Fatalf("human band/bold row %d", i)
					}
				}
			}
			if tc.name == "raw control prefix notice" && groups[0].head.who != "notice" {
				t.Fatal("normalization changed original notice branch")
			}
		})
	}
}

func TestConvoStyledIndependentMarkdownAndEmptyBody(t *testing.T) {
	for _, marks := range []bool{false, true} {
		for _, body := range []string{"```\n```", "", "\n", "\u2028", "```\n\n```"} {
			s := &transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "p", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "left", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: body, MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "right", MsgID: "m"}}}}}
			opts := styledTestOptions()
			opts.markup = marks
			var calls []string
			got, err := renderConvoStyledWith(s, opts, webMarkdownPalette(), func(text string, width int, markup bool) ([]webMarkdownLine, bool) {
				calls = append(calls, text)
				if width != 10 || markup != marks {
					t.Error("continuation renderer arguments changed")
				}
				return webMarkdownLinesChecked(text, width, markup)
			})
			if err != nil {
				t.Fatal(err)
			}
			callbackBody := body
			if body == "\n" {
				callbackBody = ""
			}
			if body == "\u2028" {
				callbackBody = "\n"
			}
			if !reflect.DeepEqual(calls, []string{"left", callbackBody, "right"}) {
				t.Fatalf("answer/more callbacks: %q", calls)
			}
			_, ordinary, _ := styledOrdinaryBodies(s, opts)
			groups := got[1].groups
			if len(groups) != 3 || groups[0].head.mode != "reply" || groups[1].head.mode != "" || groups[2].head.mode != "" {
				t.Fatal("independent headless groups flattened")
			}
			wantEmpty := body == "```\n```" && !marks
			if groups[1].emptyBodyLine != wantEmpty || !groups[1].visible || !groups[1].leadingBreak {
				t.Fatal("zero-line Markdown confused with invisible/empty-row output")
			}
			wantRows := []string{""}
			if body == "\u2028" {
				wantRows = []string{"", ""}
			}
			if marks && body == "```\n```" {
				wantRows = []string{"```", "```"}
			}
			if marks && body == "```\n\n```" {
				wantRows = []string{"```", "", "```"}
			}
			if !reflect.DeepEqual(styledRows(groups[1]), wantRows) || !reflect.DeepEqual(ordinary[2], wantRows) {
				t.Fatalf("exact empty body rows for %q marks %v: %q / %q", body, marks, styledRows(groups[1]), ordinary[2])
			}
			if wantEmpty && len(groups[1].lines) != 0 || !wantEmpty && len(groups[1].lines) == 0 {
				t.Fatal("blank-body provenance lost")
			}
			if !reflect.DeepEqual(styledRows(groups[0]), []string{"left"}) || !reflect.DeepEqual(styledRows(groups[2]), []string{"right"}) {
				t.Fatal("adjacent parse boundary changed text")
			}
		}
		for _, items := range [][]transcript.TurnItem{
			{{Kind: transcript.ItemSaid, Text: "```\n```", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "```\n```", MsgID: "m"}},
			{{Kind: transcript.ItemAnswer, Text: "```\n```"}},
		} {
			s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Open: true, Items: items}}}
			opts := styledTestOptions()
			opts.markup = marks
			out, err := renderConvoStyled(s, opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range styledGroups(out)[1:] {
				if !g.visible || g.emptyBodyLine == marks {
					t.Fatal("headed/consecutive empty fence lost its body row")
				}
			}
		}
	}
	// Independent unmatched fence, table and quote productions must not share
	// parser state or a future DOM grouping invocation.
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemAnswer, Text: "```go\ncode"}, {Kind: transcript.ItemAnswer, Text: "|h|\n|-|\n|v|"}, {Kind: transcript.ItemAnswer, Text: "> q"}}}}}
	out, err := renderConvoStyled(s, styledTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if out[1].groups[0].lines[0].kind != "code" || out[2].groups[0].lines[0].kind != "table" || out[2].groups[0].lines[0].tableRows != 2 || out[3].groups[0].lines[0].kind != "quote" {
		t.Fatal("Markdown boundaries merged code/table/quote scope")
	}
}

func TestConvoStyledEarlierBaseAndRendererArguments(t *testing.T) {
	texts := []string{"plain", "**bold**", "`code`", "[label](url)", "|h|\n|-|\n|v|", "---"}
	s := &transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "p", Open: true}}}
	for _, body := range texts {
		s.Turns[0].Items = append(s.Turns[0].Items, transcript.TurnItem{Kind: transcript.ItemAnswer, Text: body})
	}
	opts := styledTestOptions()
	opts.width = 20
	var called []string
	out, err := renderConvoStyledWith(s, opts, webMarkdownPalette(), func(text string, width int, marks bool) ([]webMarkdownLine, bool) {
		called = append(called, text)
		if width != 20 || marks {
			t.Error("renderer arguments changed")
		}
		return webMarkdownLinesChecked(text, width, marks)
	})
	if err != nil || !reflect.DeepEqual(called, texts) {
		t.Fatal("actual answer was not parsed exactly once")
	}
	for _, m := range out[1:] {
		g := m.groups[0]
		if g.head.mode != "earlier" || !reflect.DeepEqual(g.base, []string{"muted"}) || !g.leadingBreak {
			t.Fatal("earlier group lost inherited base/header")
		}
	}
	if out[1].groups[0].lines[0].spans[0].classes != nil || !reflect.DeepEqual(out[2].groups[0].lines[0].spans[0].classes, []string{"b"}) || !reflect.DeepEqual(out[3].groups[0].lines[0].spans[0].classes, []string{"code"}) {
		t.Fatal("base was appended to explicit span overrides")
	}
	rule := out[len(out)-1].groups[0]
	if rule.lines[0].kind != "rule" || !rule.lines[0].spans[0].decorative || !reflect.DeepEqual(rule.base, []string{"muted"}) {
		t.Fatal("rule-border inheritance disappeared")
	}
	// Actual terminal reset behavior remains a separate literal oracle.
	md := markdownContext{palette: webMarkdownPalette()}
	if got := md.renderMarkdown("plain **bold**", 20, "\x1b[38;5;204m", false); got != "\x1b[38;5;204mplain \x1b[1mbold\x1b[0m\x1b[38;5;204m\x1b[0m" {
		t.Fatalf("earlier reset oracle: %q", got)
	}
}

func TestConvoStyledProducerCoverageAndFailures(t *testing.T) {
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemAnswer, Text: "first"}, {Kind: transcript.ItemAnswer, Text: "fault"}, {Kind: transcript.ItemAnswer, Text: "last"}}}}}
	for _, style := range []string{"\x1b[1m\x1b[31m", "\x1b[31m\x1b[1m", "\x1b[1m!\x1b[3m"} {
		metadata := []markdownLine{{kind: "text", spans: []span{{sgr: style, text: "kept"}, {sgr: sgrItalic, text: "later"}}}}
		fallback, covered := webMarkdownConvert(metadata)
		want := []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "kept", classes: []string{"b"}}, {text: "later", classes: []string{"i"}}}}}
		if covered || !reflect.DeepEqual(fallback, want) {
			t.Fatal("producer coverage was lost or compatibility output truncated")
		}
		var calls []string
		out, err := renderConvoStyledWith(s, styledTestOptions(), webMarkdownPalette(), func(text string, w int, m bool) ([]webMarkdownLine, bool) {
			calls = append(calls, text)
			if text == "fault" {
				return webMarkdownConvert(metadata)
			}
			return webMarkdownLinesChecked(text, w, m)
		})
		if out != nil || err != transcript.ErrUnshowable || !reflect.DeepEqual(calls, []string{"first", "fault", "last"}) {
			t.Fatal("unknown producer component did not fail the actual collector stickily")
		}
	}
	for _, fault := range []string{"panic", "coverage", "class", "kind", "base", "literal", "nil renderer"} {
		palette := webMarkdownPalette()
		var calls []string
		render := webLineRenderer(func(text string, w int, m bool) ([]webMarkdownLine, bool) {
			calls = append(calls, text)
			if text == "fault" {
				switch fault {
				case "panic":
					panic("fixture private panic")
				case "coverage":
					return nil, false
				case "class":
					return []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "body", classes: []string{"not-a-class"}}}}}, true
				case "kind":
					return []webMarkdownLine{{kind: "not-a-kind"}}, true
				}
			}
			return webMarkdownLinesChecked(text, w, m)
		})
		input := s
		if fault == "base" {
			palette.dim = "\x1b[31m"
		}
		if fault == "literal" {
			palette.band = "\x1b[31m"
			input = &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Open: true}}}
		}
		if fault == "nil renderer" {
			render = nil
		}
		out, err := renderConvoStyledWith(input, styledTestOptions(), palette, render)
		if out != nil || !errors.Is(err, transcript.ErrUnshowable) || strings.Contains(err.Error(), "private") {
			t.Fatalf("presentation fault %s escaped nil/ErrUnshowable", fault)
		}
		if fault == "panic" || fault == "nil renderer" {
			file := "cmd/aiq/convo_styled_test.go"
			if fault == "nil renderer" {
				file = "cmd/aiq/convo_styled.go"
			}
			assertPanicDiagnostic(t, err, transcript.ErrUnshowable, file)
		} else if err != transcript.ErrUnshowable {
			t.Fatal("ordinary sentinel identity changed")
		}
	}
	for _, tc := range []struct {
		s     *transcript.Session
		start int
		err   error
	}{{nil, 0, errConvoProjectionSession}, {s, -1, errConvoProjectionStart}, {s, 2, errConvoProjectionStart}, {&transcript.Session{}, 0, nil}} {
		opts := styledTestOptions()
		opts.start = tc.start
		out, err := renderConvoStyledWith(tc.s, opts, webMarkdownPalette(), nil)
		if err != tc.err || out != nil {
			t.Fatal("argument/empty result classification changed")
		}
	}
}

func TestConvoStyledCollectorAlignment(t *testing.T) {
	facts := convoFacts{turn: 0, sub: "said"}
	message := []convoProjectionMessage{{parts: []convoProjectionPart{{kind: "line", convoFacts: facts}}}}
	for _, fault := range []string{"missing", "extra", "ordinal", "kind", "facts", "pending", "failed"} {
		c := &convoPaintCapture{records: []convoPaintRecord{{group: convoStyledGroup{ordinal: 0, visible: true}, kind: "line", facts: facts}}}
		switch fault {
		case "missing":
			c.records = nil
		case "extra":
			c.records = append(c.records, c.records[0])
		case "ordinal":
			c.records[0].group.ordinal = 1
		case "kind":
			c.records[0].kind = "notice"
		case "facts":
			c.records[0].facts.turn = 9
		case "pending":
			c.markdownPending = true
		case "failed":
			c.failed = true
		}
		if out, err := c.project(message); out != nil || err != transcript.ErrUnshowable {
			t.Fatalf("collector %s mismatch escaped", fault)
		}
	}
}

func TestConvoStyledCallbackAndResultOwnership(t *testing.T) {
	shared := []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "callback", classes: []string{"b"}}}}}
	s := &transcript.Session{Turns: []transcript.Turn{{Prompt: "p", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemAnswer, Text: "one"}, {Kind: transcript.ItemAnswer, Text: "two"}}}}}
	render := func(string, int, bool) ([]webMarkdownLine, bool) { return shared, true }
	out, err := renderConvoStyledWith(s, styledTestOptions(), webMarkdownPalette(), render)
	if err != nil {
		t.Fatal(err)
	}
	shared[0].kind = "changed"
	shared[0].spans[0].text = "changed"
	shared[0].spans[0].classes[0] = "changed"
	for _, m := range out[1:] {
		line := m.groups[0].lines[0]
		if line.kind != "text" || line.spans[0].text != "callback" || !reflect.DeepEqual(line.spans[0].classes, []string{"b"}) {
			t.Fatal("callback arrays were borrowed")
		}
	}
	out[1].groups[0].lines[0].spans[0].classes[0] = "u"
	out[1].groups[0].lines[0].spans[0].text = "changed"
	out[1].groups[0].base[0] = "changed"
	out[1].message.parts[0].text = "changed"
	if out[2].groups[0].lines[0].spans[0].classes[0] != "b" || out[2].groups[0].lines[0].spans[0].text != "callback" || out[2].groups[0].base[0] != "muted" {
		t.Fatal("groups shared mutable storage")
	}
	shared = []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "callback", classes: []string{"b"}}}}}
	again, err := renderConvoStyledWith(s, styledTestOptions(), webMarkdownPalette(), render)
	if err != nil || again[1].groups[0].lines[0].spans[0].text != "callback" || again[1].message.parts[0].text != "one" {
		t.Fatal("prior result mutation escaped its call")
	}
}

func TestConvoStyledImmutableInputsAndConcurrentContexts(t *testing.T) {
	s, accounts := projectionOwnedFixture()
	independent, expectedAccounts := projectionOwnedFixture()
	snapshot := projectionSnapshot(independent, expectedAccounts)
	oldPalette, oldMarkup, oldRenderer := pal, convoMarkup, markdownRenderer
	defer func() { pal, convoMarkup, markdownRenderer = oldPalette, oldMarkup, oldRenderer }()
	pal, convoMarkup = convoPalette{}, true
	markdownRenderer = func(string, int, string, bool) string { panic("ambient renderer used") }
	a, b := styledTestOptions(), styledTestOptions()
	a.width = 17
	b.width, b.markup, b.hold, b.location = 61, true, true, time.FixedZone("fixture", 7200)
	a.accounts, b.accounts = accounts, accounts
	options := []convoStyledOptions{a, b}
	var baseline [2][]convoStyledMessage
	for i, opts := range options {
		var err error
		baseline[i], err = renderConvoStyled(s, opts)
		if err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		i := n % 2
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := renderConvoStyled(s, options[i])
			if err != nil || !reflect.DeepEqual(got, baseline[i]) {
				t.Error("concurrent explicit context changed output")
			}
		}()
	}
	close(start)
	wg.Wait()
	if !reflect.DeepEqual(projectionSnapshot(s, accounts), snapshot) {
		t.Fatal("styled build mutated input/full-capacity sentinels")
	}
	for i, opts := range options {
		bad := func(string, int, bool) ([]webMarkdownLine, bool) { panic("fixture failure") }
		if out, err := renderConvoStyledWith(s, opts, webMarkdownPalette(), bad); out != nil || !errors.Is(err, transcript.ErrUnshowable) {
			t.Fatalf("failure context %d classification", i)
		} else {
			assertPanicDiagnostic(t, err, transcript.ErrUnshowable, "cmd/aiq/convo_styled_test.go")
		}
	}
	if !reflect.DeepEqual(projectionSnapshot(s, accounts), snapshot) {
		t.Fatal("failed build mutated input")
	}
}

func TestConvoStyledAdmittedCacheFailureWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a2000000-1111-4222-8333-000000000001.jsonl")
	fixture := &goldenClaude{cwd: "/invented/work", id: "a2000000-1111-4222-8333-000000000001", at: goldenAt(12, 9, 0, 0)}
	fixture.typed("first prompt")
	fixture.answer("first", "fault body")
	fixture.typed("second prompt")
	fixture.answer("second", "healthy body")
	goldenWrite(t, path, fixture.lines...)
	cache := transcript.NewCache()
	session, snap, err := cache.Session(context.Background(), path)
	if err != nil || cache.Stats().Parsed != 1 {
		t.Fatal("fixture was not admitted through Cache.Session")
	}
	var builds, parses atomic.Int32
	render := func(text string, w int, m bool) ([]webMarkdownLine, bool) {
		parses.Add(1)
		if text == "fault body" {
			return webMarkdownConvert([]markdownLine{{kind: "text", spans: []span{{sgr: sgrBold + "\x1b[31m", text: text}}}})
		}
		return webMarkdownLinesChecked(text, w, m)
	}
	build := func(start int) func() (*transcript.View, error) {
		return func() (*transcript.View, error) {
			builds.Add(1)
			opts := styledTestOptions()
			opts.start = start
			out, err := renderConvoStyledWith(session, opts, webMarkdownPalette(), render)
			if err != nil {
				if out != nil {
					t.Error("failed Value escaped")
				}
				return nil, err
			}
			return &transcript.View{Value: out, ETag: "fixture", Bytes: 512, Settled: true}, nil
		}
	}
	key := transcript.ViewKey{Snap: snap, Start: 0}
	for i := 0; i < 2; i++ {
		if view, err := cache.ViewContext(context.Background(), key, build(0)); view != nil || err != transcript.ErrUnshowable {
			t.Fatal("failed same-shape view was not suppressed")
		}
	}
	if builds.Load() != 1 {
		t.Fatal("failed window rendered again")
	}
	key.Start = 1
	view, err := cache.ViewContext(context.Background(), key, build(1))
	if err != nil || view == nil || view.Value == nil || builds.Load() != 2 || parses.Load() != 3 || cache.Stats().Parses != 1 || cache.Stats().Quarantined != 0 {
		t.Fatal("render failure became parser quarantine or blocked another window")
	}
	if len(view.Value.([]convoStyledMessage)) != 2 {
		t.Fatal("healthy start window changed")
	}
}
