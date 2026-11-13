package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/transcript"
)

func projectionTestContext() convoRenderContext {
	return convoRenderContext{
		palette: darkPalette, width: 44, markup: true,
		now:      time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
		location: time.UTC, skippedNote: "fixture skipped",
		renderer: func(s string, _ int, _ string, _ bool) string { return s },
	}
}

func projectionTestParts(messages []convoProjectionMessage) []convoProjectionPart {
	var parts []convoProjectionPart
	for _, m := range messages {
		parts = append(parts, m.parts...)
	}
	return parts
}

func TestConvoProjectionBranchFacts(t *testing.T) {
	start := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	a, b, c, d := start.Add(time.Minute), start.Add(2*time.Minute), start.Add(3*time.Minute), start.Add(4*time.Minute)
	end := start.Add(5 * time.Minute)
	zero := time.Time{}
	prompt := convoHead{mode: "prompt", who: "you", clockMode: "relative", at: start}
	final := convoHead{mode: "final", who: "fixture", durationKnown: true, duration: 5 * time.Minute, tools: 2}
	earlier := convoHead{mode: "earlier", who: "fixture"}
	reply := func(at time.Time) convoHead {
		return convoHead{mode: "reply", who: "fixture", clockMode: "reply", at: at}
	}
	// Expectations are literals from the observable branch contract, not selector output.
	part := func(kind, sub, text string, at time.Time, head convoHead) convoProjectionPart {
		return convoProjectionPart{kind: kind, text: text, convoFacts: convoFacts{
			turn: 0, turnStarted: start, promptSource: transcript.Human, sourceKnown: true, sub: sub, at: at, head: head,
		}}
	}
	tests := []struct {
		name string
		turn transcript.Turn
		hold bool
		want []convoProjectionPart
	}{
		{"mixed", transcript.Turn{Prompt: "ask", Started: start, Ended: end, Tools: 2, Reply: "finish", ReplyAt: d,
			Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "reply", At: a}, {Kind: transcript.ItemSteer, Text: "steer", At: b},
				{Kind: transcript.ItemAnswer, Text: "early", At: c}, {Kind: transcript.ItemSaid, Text: "aside", At: d}, {Kind: transcript.ItemAnswer, Text: "finish"}}}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("answer", "reply", "reply", a, reply(a)),
				part("steer", "steer", "steer", b, convoHead{mode: "steer", who: "you, while it worked"}), part("answer", "earlier", "early", c, earlier),
				part("line", "said", "aside", d, convoHead{}), part("answer", "final", "finish", zero, final)}},
		{"synthetic final", transcript.Turn{Prompt: "ask", Started: start, Ended: end, Tools: 2, Reply: "finish", ReplyAt: d}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("answer", "final", "finish", d, final)}},
		{"final said item", transcript.Turn{Prompt: "ask", Started: start, Ended: end, Tools: 2, Reply: "finish", ReplyAt: d,
			Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "finish", At: a}}}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("answer", "final", "finish", a, final)}},
		{"Earlier selected final", transcript.Turn{Prompt: "ask", Started: start, Ended: end, Tools: 2, Reply: "b", ReplyAt: d, Earlier: []string{"b"},
			Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "a", At: a}}}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("answer", "reply", "a", a, reply(a)), part("answer", "final", "b", zero, final)}},
		{"Earlier held", transcript.Turn{Prompt: "ask", Started: start, Ended: end, Tools: 2, Reply: "b", ReplyAt: d, Earlier: []string{"b"},
			Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "a", At: a}}}, true,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("answer", "reply", "a", a, reply(a)), part("answer", "earlier", "b", zero, earlier)}},
		{"legacy pieces", transcript.Turn{Prompt: "ask", Started: start, Open: true, Steers: []string{"steer"}, Earlier: []string{"early"}}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("steer", "steer", "steer", zero, convoHead{mode: "steer", who: "you, while it worked"}), part("answer", "earlier", "early", zero, earlier)}},
		{"zero reply clock", transcript.Turn{Prompt: "ask", Started: start, Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "reply"}}}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("answer", "reply", "reply", zero, reply(zero))}},
		{"none", transcript.Turn{Prompt: "ask", Started: start, Ended: end, Tools: 2}, false,
			[]convoProjectionPart{part("you", "prompt", "ask", start, prompt), part("none", "none", "", zero, final)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderConvoProjection(&transcript.Session{Provider: "fixture", Turns: []transcript.Turn{tc.turn}}, convoProjectionOptions{context: projectionTestContext(), hold: tc.hold})
			if err != nil {
				t.Fatal(err)
			}
			if parts := projectionTestParts(got); !reflect.DeepEqual(parts, tc.want) {
				t.Fatalf("parts\ngot  %#v\nwant %#v", parts, tc.want)
			}
			for _, m := range got {
				if m.convoFacts != m.parts[0].convoFacts {
					t.Fatal("group lost first-cut facts")
				}
			}
		})
	}
}

func TestConvoProjectionSourceKnowledge(t *testing.T) {
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name                              string
		source                            transcript.Source
		skipped                           bool
		text, kind, sub, who, mode, clock string
		known                             bool
	}{
		{"human", transcript.Human, false, "ask", "you", "prompt", "you", "prompt", "relative", true},
		{"aiq", transcript.Aiq, false, "ask", "notice", "prompt", "aiq", "notice", "", true},
		{"peer", transcript.Peer, false, "ask", "notice", "prompt", "peer agent", "notice", "", true},
		{"notice", transcript.Notice, false, "ask", "notice", "prompt", "notice", "notice", "", true},
		{"orchestra", transcript.Notice, false, transcript.OrchestraNotice, "notice", "prompt", "orchestra inbox", "notice", "reply", true},
		{"skipped human placeholder", transcript.Human, true, "fixture skipped", "you", "skipped_prompt", "you", "prompt", "relative", false},
		{"skipped peer", transcript.Peer, true, "fixture skipped", "you", "skipped_prompt", "you", "prompt", "relative", true},
		{"unsupported", transcript.Source(255), false, "ask", "notice", "prompt", "you", "notice", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := transcript.Turn{Prompt: tc.text, Source: tc.source, Started: at, Open: true, PromptSkipped: tc.skipped}
			if tc.skipped {
				turn.Prompt, turn.Skipped = "", 1
			}
			got, err := renderConvoProjection(&transcript.Session{Turns: []transcript.Turn{turn}}, convoProjectionOptions{context: projectionTestContext()})
			if err != nil || len(got) != 1 || len(got[0].parts) != 1 {
				t.Fatalf("%+v %v", got, err)
			}
			m := got[0]
			wantAt, headAt := at, time.Time{}
			if tc.skipped {
				wantAt = time.Time{}
			}
			if tc.clock != "" {
				headAt = at
			}
			want := convoFacts{turn: 0, turnStarted: at, at: wantAt, promptSource: tc.source, sourceKnown: tc.known, sub: tc.sub,
				head: convoHead{mode: tc.mode, who: tc.who, clockMode: tc.clock, at: headAt}}
			if m.convoFacts != want || m.kind != tc.kind || m.raw != tc.text {
				t.Fatalf("got %+v; want facts %+v", m, want)
			}
		})
	}
	got, err := renderConvoProjection(&transcript.Session{Skipped: 1}, convoProjectionOptions{context: projectionTestContext(), accounts: []convoAccountFact{{known: true, label: "unrelated"}}})
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].convoFacts != (convoFacts{turn: -1, sub: "skipped"}) || got[0].account != (convoAccountFact{}) || got[0].rawKey != "" {
		t.Fatalf("global skip acquired facts: %+v", got[0])
	}
}

func TestConvoProjectionWindowValidationAndAccounts(t *testing.T) {
	s := &transcript.Session{Skipped: 1, Turns: []transcript.Turn{{Prompt: "hidden", Open: true}, {Prompt: "visible", Open: true}}}
	opts := convoProjectionOptions{context: projectionTestContext(), start: 1, accounts: []convoAccountFact{{known: true, label: "hidden"}, {known: true, label: "pool\tblue\nend\u202e"}}}
	got, err := renderConvoProjection(s, opts)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].turn != 1 || got[0].rawKey != "#1" || got[0].account != (convoAccountFact{known: true, label: "pool blue end"}) {
		t.Fatalf("reindexed window: %+v", got[0])
	}
	if opts.accounts[1].label != "pool\tblue\nend\u202e" {
		t.Fatal("normalized caller account in place")
	}
	for _, accounts := range [][]convoAccountFact{nil, {{known: true, label: "hidden"}}, {{}, {known: true}}} {
		opts.accounts = accounts
		m, err := renderConvoProjection(s, opts)
		want := convoAccountFact{}
		if len(accounts) == 2 {
			want.known = true
		}
		if err != nil || len(m) != 1 || m[0].account != want {
			t.Fatalf("missing/known-empty account: %+v %v", m, err)
		}
	}
	for _, tc := range []struct {
		name    string
		session *transcript.Session
		start   int
		want    error
	}{
		{"nil precedence", nil, -1, errConvoProjectionSession}, {"negative", s, -1, errConvoProjectionStart}, {"past end", s, 3, errConvoProjectionStart},
		{"empty", &transcript.Session{}, 0, nil}, {"end window", s, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := renderConvoProjection(tc.session, convoProjectionOptions{start: tc.start})
			if err != tc.want || m != nil {
				t.Fatalf("got %+v, %v; want nil, %v", m, err, tc.want)
			}
		})
	}
}

func TestConvoProjectionMultipartAndRanges(t *testing.T) {
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	for _, source := range []transcript.Source{transcript.Human, transcript.Peer} {
		s := &transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "ask", Source: source, Open: true, Items: []transcript.TurnItem{
			{Kind: transcript.ItemSaid, Text: "root", MsgID: "m", At: at}, {Kind: transcript.ItemSaid, Text: "", MsgID: "m", At: at.Add(time.Minute)}, {Kind: transcript.ItemSaid, Text: "tail", MsgID: "m", At: at.Add(2 * time.Minute)},
		}}}}
		got, err := renderConvoProjection(s, convoProjectionOptions{context: projectionTestContext()})
		if err != nil || len(got) != 2 {
			t.Fatalf("%+v %v", got, err)
		}
		kind, continuation, sub := "answer", "more", "reply"
		if source == transcript.Peer {
			kind, continuation, sub = "line", "part", "said"
		}
		m := got[1]
		if m.kind != kind || m.sub != sub || m.raw != "root\n\n\n\ntail" || len(m.parts) != 3 {
			t.Fatalf("multipart: %+v", m)
		}
		for i, p := range m.parts[1:] {
			if p.kind != continuation || p.sub != "continuation" || p.head != (convoHead{}) || p.at != at.Add(time.Duration(i+1)*time.Minute) {
				t.Fatalf("continuation facts: %+v", p)
			}
		}
	}
	cuts := []convoCut{
		{end: 2, key: "k", kind: "answer", text: "root", facts: convoFacts{sub: "reply", head: convoHead{mode: "reply"}}},
		{end: 2, key: "k", kind: "part", text: "", facts: convoFacts{sub: "continuation"}},
		{end: 4, key: "k", kind: "more", text: "tail", facts: convoFacts{sub: "continuation"}},
		{end: 6, key: "k", kind: "none", text: "ignored", facts: convoFacts{sub: "none", head: convoHead{mode: "final"}}},
	}
	folded := foldConvoCuts("r\nt\nn\n", cuts)
	got := projectConvoCuts("r\nt\nn\n", cuts, nil)
	if len(folded) != 1 || folded[0].firstCut != 0 || folded[0].lastCut != 3 || len(got) != 1 {
		t.Fatalf("range: %+v %+v", folded, got)
	}
	if got[0].raw != "root\n\n\n\ntail" || len(got[0].parts) != 4 || got[0].sub != "reply" || got[0].head.mode != "reply" {
		t.Fatalf("range projection: %+v", got[0])
	}
	orphan := append([]convoCut{{kind: "part", text: "orphan"}}, cuts...)
	f := foldConvoCuts("r\nt\nn\n", orphan)
	if len(f) != 1 || f[0].firstCut != 1 || f[0].lastCut != 4 {
		t.Fatalf("orphan included: %+v", f)
	}
}

func TestConvoProjectionSuppressionAndHeadBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		turn transcript.Turn
		live bool
		want []string
	}{
		{"local command", transcript.Turn{Prompt: "/model", Open: true}, false, nil},
		{"idle peer", transcript.Turn{Source: transcript.Peer, Prompt: `{"type":"idle_notification"}`, Open: true}, false, nil},
		{"live none suppressed", transcript.Turn{Prompt: "ask"}, true, []string{"prompt"}},
		{"turn skipped", transcript.Turn{Prompt: "ask", Open: true, Skipped: 1}, false, []string{"prompt", "skipped"}},
		{"promoted", transcript.Turn{Prompt: "ask", Reply: "answer", Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "answer"}, {Kind: transcript.ItemAnswer, Text: "answer", Promotes: true}}}, false, []string{"prompt", "final"}},
		{"replaced", transcript.Turn{Prompt: "ask", Reply: "new", Items: []transcript.TurnItem{{Kind: transcript.ItemAnswer, Text: "old"}, {Kind: transcript.ItemAnswer, Text: "new", Supersedes: true}}}, false, []string{"prompt", "final"}},
		{"final marked continuation", transcript.Turn{Prompt: "ask", Reply: "tail", Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "root", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "tail", MsgID: "m"}}}, false, []string{"prompt", "reply", "continuation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderConvoProjection(&transcript.Session{Turns: []transcript.Turn{tc.turn}}, convoProjectionOptions{context: projectionTestContext(), live: tc.live})
			if err != nil {
				t.Fatal(err)
			}
			var subs []string
			for _, p := range projectionTestParts(got) {
				subs = append(subs, p.sub)
				if p.sub == "skipped" && (p.turn != 0 || !p.sourceKnown || !p.at.IsZero() || p.head != (convoHead{})) {
					t.Fatalf("turn skip facts: %+v", p)
				}
			}
			if !reflect.DeepEqual(subs, tc.want) {
				t.Fatalf("got %v want %v", subs, tc.want)
			}
		})
	}
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		start, end time.Time
		tools      int
		known      bool
		duration   time.Duration
		shownTools int
	}{
		{at, at.Add(-time.Minute), -2, true, -time.Minute, 0},
		{at, at, 0, true, 0, 0},
		{time.Time{}, at, 2, false, 0, 2},
		{at, time.Time{}, 2, false, 0, 2},
	} {
		m, err := renderConvoProjection(&transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "ask", Reply: "answer", Started: tc.start, Ended: tc.end, Tools: tc.tools}}}, convoProjectionOptions{context: projectionTestContext()})
		want := convoHead{mode: "final", who: "fixture", durationKnown: tc.known, duration: tc.duration, tools: tc.shownTools}
		if err != nil || len(m) != 2 || m[1].head != want || !m[1].at.IsZero() {
			t.Fatalf("boundary head: %+v %v; want %+v", m, err, want)
		}
	}
	m, err := renderConvoProjection(&transcript.Session{Turns: []transcript.Turn{{Source: transcript.Notice, Prompt: transcript.OrchestraNotice, Open: true}}}, convoProjectionOptions{context: projectionTestContext()})
	if err != nil || len(m) != 1 || m[0].head != (convoHead{mode: "notice", who: "orchestra inbox"}) {
		t.Fatalf("zero Orchestra clock: %+v %v", m, err)
	}
}

func TestConvoProjectionPlainDataBoundary(t *testing.T) {
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	ctx := projectionTestContext()
	var rendered []string
	ctx.renderer = func(s string, _ int, _ string, _ bool) string { rendered = append(rendered, s); return s }
	m, err := renderConvoProjection(&transcript.Session{Provider: "fixture\tprovider\n\u202e", Turns: []transcript.Turn{{
		Prompt: "<prompt>\x00\u202e\u2028next\n", Started: at, Reply: "<tag>\x00value\u202e\u2028next\tcell\n",
	}}}, convoProjectionOptions{context: ctx})
	if err != nil || len(m) != 2 {
		t.Fatalf("%+v %v", m, err)
	}
	if m[0].raw != "<prompt>\nnext" || m[1].raw != "<tag>value\nnext\tcell" || m[1].parts[0].text != m[1].raw || m[1].head.who != "fixture provider " {
		t.Fatalf("plain text boundary: %+v", m)
	}
	if !strings.Contains(m[0].rawKey, "\x00") || !reflect.DeepEqual(rendered, []string{"<tag>value\nnext\tcell"}) {
		t.Fatal("opaque key changed or renderer received unclean payload")
	}
}

func TestConvoProjectionIdentityLiterals(t *testing.T) {
	// Literals obtained with a separate integer FNV loop and SHA256 calculator.
	// No production hashing/formatting helper builds the expectations at runtime.
	for _, tc := range []struct{ key, kind, raw, id string }{
		{"k", "answer", "", "5e8378b30d9d9b6b573884f331d25700"},
		{"k", "answer", "a", "36f81678eb2cce8584820415651caca2"},
		{"k", "answer", "b", "1dc81d50acbe03449b9371e98388aa6f"},
		{"k", "answer", "10", "f8fd2bcebe1bee6a6188f940a7a9064a"}, // leading-zero hash must be unpadded
		{"other", "answer", "a", "ae27fd94a6fafaecc1503339d6632783"},
		{"k", "line", "a", "2172b4126c3fa368575bdaef3eddebb9"},
		{"k\x00opaque", "answer", "a", "320af948d05bffcbada0c0af9e1cd2c6"},
	} {
		got := projectConvoCuts("", []convoCut{{key: tc.key, kind: tc.kind, text: tc.raw}}, nil)
		if len(got) != 1 || got[0].id != tc.id || len(got[0].id) != 32 || strings.Trim(got[0].id, "0123456789abcdef") != "" {
			t.Fatalf("identity %q/%q/%q: %+v", tc.key, tc.kind, tc.raw, got)
		}
	}
	duplicate := []convoCut{{key: "k", kind: "answer", text: "a"}, {key: "k", kind: "answer", text: "a"}}
	for n := 0; n < 2; n++ {
		got := projectConvoCuts("", duplicate, nil)
		if got[0].id != "36f81678eb2cce8584820415651caca2" || got[1].id != "27528fe4465f171711ed5662c7328f2f" {
			t.Fatalf("counter/reset: %+v", got)
		}
	}
	cuts := []convoCut{{key: "k", kind: "answer", text: "a"}, {key: "k", kind: "more", text: "x"},
		{key: "k", kind: "answer", text: "a"}, {key: "k", kind: "part", text: "y"},
		{key: "k", kind: "answer", text: "a"}, {key: "k", kind: "part", text: "x"}}
	got := projectConvoCuts("", cuts, nil)
	want := []string{"8641b0c0d519ac1de0decbc2dcf8e525", "57b553e6c2878381cbd7dd3e73d2510a", "8a1472431c652af3411e2092bb989914"}
	terminal := &convoScreen{}
	terminal.cut(7, "", cuts, 44)
	for i := range want {
		if got[i].id != want[i] {
			t.Fatalf("complete-body occurrence %d: %s want %s", i, got[i].id, want[i])
		}
		if terminal.msgs[i].id != []string{"7\x00k\x00answer\x00af63dc4c8601ec8c\x001", "7\x00k\x00answer\x00af63dc4c8601ec8c\x002", "7\x00k\x00answer\x00af63dc4c8601ec8c\x003"}[i] {
			t.Fatal("terminal identity changed")
		}
	}
	for _, continuation := range []string{"more", "part"} {
		base := []convoCut{{key: "k", kind: "answer", text: "a"}, {key: "k", kind: continuation}}
		m := projectConvoCuts("", base, nil)
		if m[0].raw != "a\n\n" || m[0].id != "b1870661d6566d0fc9fd967740cb6cf4" {
			t.Fatalf("empty continuation trimmed: %+v", m)
		}
		base = append(base, convoCut{key: "k", kind: "none", text: "not copied"})
		withNone := projectConvoCuts("", base, nil)
		if withNone[0].id != m[0].id || withNone[0].raw != m[0].raw {
			t.Fatal("none changed identity/copy")
		}
	}
}

func TestConvoProjectionRenderFailure(t *testing.T) {
	s := &transcript.Session{Provider: "fixture", Turns: []transcript.Turn{{Prompt: "ask", Open: true, Items: []transcript.TurnItem{
		{Kind: transcript.ItemAnswer, Text: "one"}, {Kind: transcript.ItemAnswer, Text: "bad"}, {Kind: transcript.ItemAnswer, Text: "three"},
	}}}}
	ctx := projectionTestContext()
	var seen []string
	ctx.renderer = func(s string, _ int, _ string, _ bool) string {
		seen = append(seen, s)
		if s == "bad" {
			panic("fixture panic payload")
		}
		return s
	}
	got, err := renderConvoProjection(s, convoProjectionOptions{context: ctx})
	if got != nil || !errors.Is(err, errConvoProjectionRender) || strings.Contains(err.Error(), "payload") || !reflect.DeepEqual(seen, []string{"one", "bad", "three"}) {
		t.Fatalf("sticky failure: %+v %v %v", got, err, seen)
	}
	assertPanicDiagnostic(t, err, errConvoProjectionRender, "cmd/aiq/convo_projection_test.go")
	p := newConvoPrinterWithContext("fixture", true, false, ctx)
	out := p.emit(s)
	if !p.renderFailed || !strings.Contains(out, "one") || !strings.Contains(out, "bad") || !strings.Contains(out, "three") {
		t.Fatal("CLI lost fallback/continuation")
	}
	if p.emit(s) != "" || p.renderFailed || p.faults.Origin().Present() {
		t.Fatal("failure did not reset on next emit")
	}
	more := &transcript.Session{Turns: []transcript.Turn{{Prompt: "ask", Open: true, Items: []transcript.TurnItem{
		{Kind: transcript.ItemSaid, Text: "one", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "bad", MsgID: "m"},
	}}}}
	if m, e := renderConvoProjection(more, convoProjectionOptions{context: ctx}); m != nil || !errors.Is(e, errConvoProjectionRender) {
		t.Fatalf("failed more: %+v %v", m, e)
	}
	if m, e := renderConvoProjection(s, convoProjectionOptions{context: projectionTestContext()}); e != nil || len(m) != 4 {
		t.Fatalf("healthy retry: %+v %v", m, e)
	}
	ctx.renderer = nil
	if m, e := renderConvoProjection(s, convoProjectionOptions{context: ctx}); m != nil || !errors.Is(e, errConvoProjectionRender) {
		t.Fatalf("nil attempted: %+v %v", m, e)
	}
	for _, turn := range []transcript.Turn{{Prompt: "open", Open: true}, {Prompt: "closed"}, {Source: transcript.Peer, Prompt: "peer", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "line"}}}} {
		if _, e := renderConvoProjection(&transcript.Session{Turns: []transcript.Turn{turn}}, convoProjectionOptions{context: ctx}); e != nil {
			t.Fatalf("nil renderer without attempt: %v", e)
		}
	}
	ctx.renderer = func(string, int, string, bool) string { return "" }
	if m, e := renderConvoProjection(s, convoProjectionOptions{context: ctx}); e != nil || len(m) != 4 || m[1].raw != "one" {
		t.Fatalf("empty successful renderer: %+v %v", m, e)
	}
}

// Each call constructs independent arrays, including spare-capacity sentinels,
// nil slices and non-nil empty slices. No production clone helper is used.
func projectionOwnedFixture() (*transcript.Session, []convoAccountFact) {
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	items := []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "**root**", MsgID: "m", At: at}, {Kind: transcript.ItemSaid, Text: "tail", MsgID: "m", At: at},
		{Kind: transcript.ItemAnswer, Text: "finish", Provisional: true, At: at}, {Text: "spare item"}}
	steers, earlier := []string{"steer", "spare steer"}, []string{"earlier", "spare earlier"}
	emptyItems, emptyStrings := []transcript.TurnItem{{Text: "empty spare"}}, []string{"empty spare"}
	turns := []transcript.Turn{
		{Prompt: "nil slices", Open: true},
		{Prompt: "empty slices", Source: transcript.Peer, Open: true, Items: emptyItems[:0], Steers: emptyStrings[:0], Earlier: []string{}},
		{Prompt: "ask\x1b\u202e", Started: at, Ended: at.Add(time.Minute), ReplyAt: at.Add(30 * time.Second), Reply: "finish", Tools: 2,
			Items: items[:3], Steers: steers[:1], Earlier: earlier[:1]},
		{Prompt: "spare turn", Items: []transcript.TurnItem{{Text: "nested spare"}}},
	}
	accounts := []convoAccountFact{{known: true, label: "first"}, {}, {known: true, label: "pool\tblue\n"}, {known: true, label: "spare account"}}
	return &transcript.Session{Provider: "fixture\nprovider\u202e", ID: "invented", Title: "fixture", Turns: turns[:3]}, accounts[:3]
}

type projectionSliceSnapshot[T any] struct {
	Len, Cap int
	Values   []T
}

func projectionFullSlice[T any](s []T) projectionSliceSnapshot[T] {
	var values []T
	if s != nil {
		values = make([]T, cap(s))
		copy(values, s[:cap(s)])
	}
	return projectionSliceSnapshot[T]{len(s), cap(s), values}
}

type projectionTurnSnapshot struct {
	Header          transcript.Turn
	Items           projectionSliceSnapshot[transcript.TurnItem]
	Steers, Earlier projectionSliceSnapshot[string]
}

type projectionInputSnapshot struct {
	Header   transcript.Session
	Len, Cap int
	Turns    []projectionTurnSnapshot
	Accounts projectionSliceSnapshot[convoAccountFact]
}

func projectionSnapshot(s *transcript.Session, accounts []convoAccountFact) projectionInputSnapshot {
	header := *s
	header.Turns = nil
	out := projectionInputSnapshot{Header: header, Len: len(s.Turns), Cap: cap(s.Turns), Accounts: projectionFullSlice(accounts)}
	for _, turn := range s.Turns[:cap(s.Turns)] {
		entry := projectionTurnSnapshot{Items: projectionFullSlice(turn.Items), Steers: projectionFullSlice(turn.Steers), Earlier: projectionFullSlice(turn.Earlier)}
		turn.Items, turn.Steers, turn.Earlier = nil, nil, nil
		entry.Header = turn
		out.Turns = append(out.Turns, entry)
	}
	return out
}

func TestConvoProjectionOwnershipAndConcurrentContexts(t *testing.T) {
	s, accounts := projectionOwnedFixture()
	expectedSession, expectedAccounts := projectionOwnedFixture()
	wantInput := projectionSnapshot(expectedSession, expectedAccounts)
	oldPal, oldMarkup, oldRenderer := pal, convoMarkup, markdownRenderer
	defer func() { pal, convoMarkup, markdownRenderer = oldPal, oldMarkup, oldRenderer }()
	pal, convoMarkup = convoPalette{}, false
	markdownRenderer = func(string, int, string, bool) string { panic("ambient renderer used") }
	var options []convoProjectionOptions
	for i := 0; i < 4; i++ {
		ctx := projectionTestContext()
		ctx.width, ctx.markup = 17+20*i, i%2 == 0
		ctx.location = time.FixedZone("fixture", i*3600)
		if i%2 != 0 {
			ctx.palette = lightPalette
		}
		width, markup, palette := ctx.width, ctx.markup, ctx.palette
		render := (markdownContext{palette: palette}).renderMarkdown
		ctx.renderer = func(s string, w int, base string, marks bool) string {
			if w != width || marks != markup || (base != "" && base != palette.dim) {
				t.Errorf("context crossed: width %d markup %v base %q", w, marks, base)
			}
			return render(s, w, base, marks)
		}
		if i >= 2 {
			ctx.renderer = func(string, int, string, bool) string { panic("fixture failure") }
		}
		options = append(options, convoProjectionOptions{context: ctx, hold: i%2 != 0, accounts: accounts})
	}
	type result struct {
		index    int
		messages []convoProjectionMessage
		err      error
	}
	baseline := make([]result, len(options))
	for i, opts := range options {
		messages, err := renderConvoProjection(s, opts)
		if i < 2 && err != nil || i >= 2 && (!errors.Is(err, errConvoProjectionRender) || messages != nil) {
			t.Fatalf("baseline %d: %+v %v", i, messages, err)
		}
		if i >= 2 {
			assertPanicDiagnostic(t, err, errConvoProjectionRender, "cmd/aiq/convo_projection_test.go")
		}
		baseline[i] = result{i, messages, err}
		if i == 1 {
			var earlierSeen bool
			for _, part := range projectionTestParts(messages) {
				if part.text == "finish" {
					t.Fatal("hold exposed trailing provisional item")
				}
				earlierSeen = earlierSeen || part.text == "earlier"
			}
			if !earlierSeen {
				t.Fatal("hold discarded settled Earlier answer")
			}
		}
		if !reflect.DeepEqual(projectionSnapshot(s, accounts), wantInput) {
			t.Fatalf("input/capacity mutated after baseline %d", i)
		}
	}
	// Change returned storage, then compare a separate call with its prior output.
	mutated, err := renderConvoProjection(s, options[0])
	if err != nil {
		t.Fatal(err)
	}
	for i := range mutated {
		mutated[i].raw, mutated[i].rawKey, mutated[i].id, mutated[i].head.who, mutated[i].account.label = "changed", "changed", "changed", "changed", "changed"
		for j := range mutated[i].parts[:cap(mutated[i].parts)] {
			mutated[i].parts[:cap(mutated[i].parts)][j].text = "changed part"
		}
		mutated[i].parts = append(mutated[i].parts, convoProjectionPart{text: "appended"})
	}
	again, err := renderConvoProjection(s, options[0])
	if err != nil || !reflect.DeepEqual(again, baseline[0].messages) {
		t.Fatal("result mutation leaked to next call/baseline")
	}
	for _, m := range again {
		if strings.ContainsAny(m.raw, "\x1b\u202e") || strings.ContainsAny(m.head.who, "\n\t\u202e") {
			t.Fatal("unsanitized exposed text")
		}
	}
	start := make(chan struct{})
	results := make(chan result, 16)
	for job := 0; job < cap(results); job++ {
		i := job % len(options)
		go func() { <-start; m, e := renderConvoProjection(s, options[i]); results <- result{i, m, e} }()
	}
	close(start)
	for job := 0; job < cap(results); job++ {
		r := <-results
		if !sameDiagnosticError(r.err, baseline[r.index].err) || !reflect.DeepEqual(r.messages, baseline[r.index].messages) {
			t.Errorf("concurrent context %d diverged", r.index)
		}
	}
	if !reflect.DeepEqual(projectionSnapshot(s, accounts), wantInput) {
		t.Fatal("input/full backing capacity changed")
	}
	// Display-only context and account changes must not change identity.
	other := options[0]
	other.context, other.accounts = options[1].context, []convoAccountFact{{label: "other"}, {}, {known: true, label: "different"}}
	messages, err := renderConvoProjection(s, other)
	if err != nil || len(messages) != len(baseline[0].messages) {
		t.Fatalf("context variants: %v", err)
	}
	for i, m := range messages {
		if m.id != baseline[0].messages[i].id {
			t.Fatal("display context changed identity")
		}
	}
}

func sameDiagnosticError(a, b error) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Error() == b.Error() && errors.Is(a, errConvoProjectionRender) == errors.Is(b, errConvoProjectionRender) && diagnostic.FromError(a) == diagnostic.FromError(b)
}
