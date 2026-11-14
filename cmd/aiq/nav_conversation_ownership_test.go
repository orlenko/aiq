package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/orlenko/aiq/internal/transcript"
)

func wireTestBorrow(s string) string {
	return (strings.Repeat("!", 65536) + s + strings.Repeat("!", 65536))[65536 : 65536+len(s)]
}
func wireTestPrivatePaint() []convoStyledMessage {
	head := convoHead{mode: wireTestBorrow("earlier"), who: wireTestBorrow("claude"), clockMode: wireTestBorrow("reply"), at: time.UnixMilli(0)}
	lines := []webMarkdownLine{{kind: wireTestBorrow("code"), spans: []webMarkdownSpan{{text: wireTestBorrow("visible span"), classes: []string{wireTestBorrow("code")}}}}}
	return []convoStyledMessage{{message: convoProjectionMessage{convoFacts: convoFacts{turn: 2, sub: wireTestBorrow("earlier"), sourceKnown: true, promptSource: transcript.Human}, id: wireTestBorrow("0123456789abcdef0123456789abcdef"), kind: wireTestBorrow("answer"), rawKey: "PRIVATE-key\x00雪", raw: wireTestBorrow("visible copy <tag>"), account: convoAccountFact{known: true, label: wireTestBorrow("account")}}, groups: []convoStyledGroup{{ordinal: 0, head: head, base: []string{wireTestBorrow("muted")}, lines: lines, visible: true, leadingBreak: true}, {ordinal: 1, head: head, base: []string{wireTestBorrow("muted")}, lines: lines, visible: true}}}}
}

func TestNavConversationDetachedStringsAndNestedArrays(t *testing.T) {
	s := wireTestSession(transcript.Turn{}, transcript.Turn{}, transcript.Turn{})
	s.Provider = wireTestBorrow(s.Provider)
	s.ID = wireTestBorrow(s.ID)
	s.Cwd = wireTestBorrow(s.Cwd)
	s.Title = wireTestBorrow(s.Title)
	s.Model = wireTestBorrow(s.Model)
	s.Branch = wireTestBorrow(s.Branch)
	paint := wireTestPrivatePaint()
	o := wireTestOptions()
	render := func(_ *transcript.Session, opts convoStyledOptions) ([]convoStyledMessage, error) {
		if opts.width != 100 || opts.location != time.UTC || opts.skippedNote != "EXPLICIT fixture skipped" {
			t.Error("explicit render context lost")
		}
		return paint, nil
	}
	w, body, f, err := buildNavConversationWith(s, o, render, json.Marshal)
	if err != nil {
		t.Fatal(err)
	}
	if w.Messages[0].Key != "41a34c9b2a9a5d465ebdfaf68b4635ef1ff8b648ff9604a6cb4791f1b2339beb" || strings.Contains(string(body), "PRIVATE-key") || strings.Contains(string(body), s.Path) || !strings.Contains(string(body), "visible copy \\u003ctag\\u003e") {
		t.Fatal("public/private key or visible-content boundary")
	}
	m, g := w.Messages[0], w.Messages[0].Groups[0]
	pm, pg := paint[0].message, paint[0].groups[0]
	for i, pair := range [][2]string{{s.Provider, w.Session.Provider}, {s.ID, w.Session.ID}, {s.Cwd, w.Session.Cwd}, {s.Title, w.Session.Title}, {s.Model, w.Session.Model}, {s.Branch, w.Session.Branch}, {pm.id, m.ID}, {pm.kind, m.Kind}, {pm.sub, m.Sub}, {pm.raw, m.Raw}, {pm.account.label, m.Account}, {pg.head.mode, g.Head.Mode}, {pg.head.who, g.Head.Who}, {pg.head.clockMode, g.Head.ClockMode}, {pg.base[0], g.Base[0]}, {pg.lines[0].kind, g.Lines[0].Kind}, {pg.lines[0].spans[0].text, g.Lines[0].Spans[0].Text}, {pg.lines[0].spans[0].classes[0], g.Lines[0].Spans[0].Classes[0]}} {
		if pair[0] != pair[1] || unsafe.StringData(pair[0]) == unsafe.StringData(pair[1]) {
			t.Fatalf("public string %d changed or retained borrowed storage", i)
		}
	}
	before := append([]byte(nil), body...)
	w.Messages[0].Groups[0].Base[0] = "changed"
	w.Messages[0].Groups[0].Lines[0].Spans[0].Classes[0] = "changed"
	w.Messages[0].Groups[0].Lines[0].Spans[0].Text = "changed"
	if w.Messages[0].Groups[1].Base[0] != "muted" || w.Messages[0].Groups[1].Lines[0].Spans[0].Classes[0] != "code" || paint[0].groups[0].lines[0].spans[0].text != "visible span" || !bytes.Equal(body, before) {
		t.Fatal("DTO arrays share siblings/input/body")
	}
	body[0] = '!'
	other, otherBody, otherF, err := buildNavConversationWith(s, o, render, json.Marshal)
	if err != nil || !bytes.Equal(otherBody, before) || otherF != f || other.Messages[0].Groups[0].Base[0] != "muted" {
		t.Fatal("mutation contaminated subsequent result")
	}
	paint[0].groups[0].lines[0].spans[0].classes[0] = "changed input"
	if other.Messages[0].Groups[0].Lines[0].Spans[0].Classes[0] != "code" {
		t.Fatal("result retained callback arrays")
	}
}

func TestNavConversationMalformedProducerAndWholeFailures(t *testing.T) {
	s := wireTestSession(transcript.Turn{}, transcript.Turn{}, transcript.Turn{})
	o := wireTestOptions()
	for _, mutate := range []func([]convoStyledMessage){
		func(p []convoStyledMessage) { p[0].message.id = "not-an-id" }, func(p []convoStyledMessage) { p[0].message.kind = "more" }, func(p []convoStyledMessage) { p[0].message.sub = "invented" },
		func(p []convoStyledMessage) { p[0].message.turn = -2 }, func(p []convoStyledMessage) { p[0].message.turn = 3 }, func(p []convoStyledMessage) { p[0].message.turn = -1 },
		func(p []convoStyledMessage) { p[0].groups[0].ordinal = 1 }, func(p []convoStyledMessage) { p[0].groups[0].head.mode = "invented" }, func(p []convoStyledMessage) { p[0].groups[0].head.clockMode = "invented" }, func(p []convoStyledMessage) { p[0].groups[0].head.tools = -1 },
		func(p []convoStyledMessage) { p[0].groups[0].base[0] = "payload-class" }, func(p []convoStyledMessage) { p[0].groups[0].lines[0].kind = "html" }, func(p []convoStyledMessage) { p[0].groups[0].lines[0].tableRows = -1 }, func(p []convoStyledMessage) { p[0].groups[0].lines[0].spans[0].classes[0] = "payload-class" },
		func(p []convoStyledMessage) { p[0].message.at = time.Unix(1<<62, 0) }, func(p []convoStyledMessage) { p[0].groups[0].head.at = time.Unix(1<<62, 0) },
	} {
		paint := wireTestPrivatePaint()
		mutate(paint)
		w, b, f, err := buildNavConversationWith(s, o, func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error) { return paint, nil }, json.Marshal)
		wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
	}
	for _, mode := range []string{"render-error", "render-panic", "marshal-error", "marshal-panic", "marshal-size"} {
		render := navConversationRender(renderConvoStyled)
		marshal := json.Marshal
		switch mode {
		case "render-error":
			render = func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error) {
				return nil, errors.New("PRIVATE renderer")
			}
		case "render-panic":
			render = func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error) { panic("PRIVATE renderer") }
		case "marshal-error":
			marshal = func(any) ([]byte, error) { return []byte("partial PRIVATE"), errors.New("PRIVATE encoder") }
		case "marshal-panic":
			marshal = func(any) ([]byte, error) { panic("PRIVATE encoder") }
		case "marshal-size":
			marshal = func(any) ([]byte, error) { return []byte("{}"), nil }
		}
		w, b, f, err := buildNavConversationWith(s, o, render, marshal)
		wireTestWholeFailure(t, w, b, f, err, transcript.ErrUnshowable)
	}
}

func TestNavConversationValidationAndEncodedLimit(t *testing.T) {
	s := wireTestSession()
	renderCalls := 0
	render := func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error) {
		renderCalls++
		return nil, nil
	}
	for _, modify := range []func(*navConversationOptions){func(o *navConversationOptions) { o.provider = "unknown" }, func(o *navConversationOptions) { o.id = "short" }, func(o *navConversationOptions) { o.id = "00000001-0000-4000-8000-000000000002" }, func(o *navConversationOptions) { o.last = 0 }, func(o *navConversationOptions) { o.last = 10001 }, func(o *navConversationOptions) { o.location = nil }, func(o *navConversationOptions) { o.skippedNote = "" }, func(o *navConversationOptions) { o.bodyBytes = 0 }, func(o *navConversationOptions) { o.anchor = -1 }, func(o *navConversationOptions) { o.anchor = 10000000000000 }, func(o *navConversationOptions) { o.stale = true; o.live = true }, func(o *navConversationOptions) { o.stale = true; o.hold = true }} {
		o := wireTestOptions()
		modify(&o)
		w, b, f, err := buildNavConversationWith(s, o, render, json.Marshal)
		wireTestWholeFailure(t, w, b, f, err, errNavConversationInvalid)
	}
	if renderCalls != 0 {
		t.Fatal("invalid options rendered")
	}
	o := wireTestOptions()
	w, b, f, err := buildNavConversationWith(nil, o, render, json.Marshal)
	wireTestWholeFailure(t, w, b, f, err, errNavConversationInvalid)
	o.anchor = 1234
	w, b, f, err = buildNavConversationWith(s, o, render, json.Marshal)
	wireTestWholeFailure(t, w, b, f, err, errNavConversationAnchor)
	if renderCalls != 0 {
		t.Fatal("missing anchor rendered")
	}
	o = wireTestOptions()
	o.liveMark = ""
	wireTestBuild(t, s, o)
	// Independent complete empty-envelope byte literal; production size is
	// never called to construct the expected limit.
	const empty = `{"session":{"provider":"claude","id":"00000001-0000-4000-8000-000000000001","cwd":"/fixture/work","title":"title","model":"model","branch":"branch","started":{"ms":0,"known":false},"updated":{"ms":0,"known":false},"worker":false,"bypass":false},"earlier_hidden":0,"later_hidden":0,"window_start":0,"reset":true,"markup":true,"messages":[]}`
	for _, limit := range []int64{int64(len(empty)), int64(len(empty) - 1)} {
		o = wireTestOptions()
		o.bodyBytes = limit
		calls := 0
		marshal := func(v any) ([]byte, error) { calls++; return json.Marshal(v) }
		w, b, f, err = buildNavConversationWith(s, o, renderConvoStyled, marshal)
		if limit == int64(len(empty)) {
			if err != nil || string(b) != empty || calls != 1 {
				t.Fatal("exact encoded limit failed", err)
			}
		} else {
			wireTestWholeFailure(t, w, b, f, err, errNavConversationTooLarge)
			if calls != 0 {
				t.Fatal("oversized body was marshaled")
			}
		}
	}
	// A complete independent struct literal checks escaping and full-DTO
	// compatibility against the standard encoder, including invalid UTF-8.
	o = wireTestOptions()
	s.Title = "<>&\u2028\u2029\xff\ufffd"
	expected := navConversationWindow{Session: navConversationSession{Provider: "claude", ID: wireTestID, Cwd: "/fixture/work", Title: "<>&\u2028\u2029\xff\ufffd", Model: "model", Branch: "branch"}, Reset: true, Markup: true, Messages: []navConversationMessage{}}
	encoded, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	o.bodyBytes = int64(len(encoded))
	_, b = wireTestBuild(t, s, o)
	if !bytes.Equal(b, encoded) {
		t.Fatal("escaped literal DTO mismatch")
	}
	o.bodyBytes--
	w, b, f, err = buildNavConversation(s, o)
	wireTestWholeFailure(t, w, b, f, err, errNavConversationTooLarge)
}

func wireTestOwnedFixture() (*transcript.Session, []convoAccountFact) {
	s, a := projectionOwnedFixture()
	s.Provider = "claude"
	s.ID = wireTestID
	s.Path = "PRIVATE-path"
	s.Turns[2].Open = true
	return s, a
}

func TestNavConversationStaleHoldAndConcurrentOwnership(t *testing.T) {
	s, accounts := wireTestOwnedFixture()
	expected, expectedAccounts := wireTestOwnedFixture()
	before := projectionSnapshot(expected, expectedAccounts)
	oldPalette, oldMarkup, oldRenderer := pal, convoMarkup, markdownRenderer
	defer func() { pal, convoMarkup, markdownRenderer = oldPalette, oldMarkup, oldRenderer }()
	pal = convoPalette{}
	convoMarkup = false
	markdownRenderer = func(string, int, string, bool) string { panic("ambient renderer") }
	options := []navConversationOptions{wireTestOptions(), wireTestOptions(), wireTestOptions()}
	options[1].markup = false
	options[1].hold = true
	options[1].live = true
	options[1].location = time.FixedZone("fixture", 9*3600)
	options[2].stale = true
	baseline := make([][]byte, len(options))
	for i := range options {
		options[i].accounts = accounts
		_, baseline[i] = wireTestBuild(t, s, options[i])
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, body := wireTestBuild(t, s, options[i%len(options)])
			if !bytes.Equal(body, baseline[i%len(options)]) {
				t.Error("concurrent contexts contaminated")
			}
		}(i)
	}
	wg.Wait()
	o := options[0]
	_, _, _, err := buildNavConversationWith(s, o, func(*transcript.Session, convoStyledOptions) ([]convoStyledMessage, error) { panic("fixture failure") }, json.Marshal)
	if !errors.Is(err, transcript.ErrUnshowable) {
		t.Fatal("failure classification")
	}
	assertPanicDiagnostic(t, err, transcript.ErrUnshowable, "cmd/aiq/nav_conversation_ownership_test.go")
	if !reflect.DeepEqual(projectionSnapshot(s, accounts), before) {
		t.Fatal("source or spare-capacity storage mutated across success/hold/stale/failure")
	}
	active := wireTestSession(transcript.Turn{Prompt: "p", Reply: "provisional", Earlier: []string{"settled"}, Items: []transcript.TurnItem{{Kind: transcript.ItemAnswer, Text: "provisional", Provisional: true}}})
	activeOpts := wireTestOptions()
	activeOpts.live = true
	activeOpts.hold = true
	held, _, _, holdErr := buildNavConversationWith(active, activeOpts, func(got *transcript.Session, opts convoStyledOptions) ([]convoStyledMessage, error) {
		if !opts.live || !opts.hold {
			t.Error("active tail options lost")
		}
		return renderConvoStyled(got, opts)
	}, json.Marshal)
	if holdErr != nil {
		t.Fatal(holdErr)
	}
	if len(held.Messages) != 2 || held.Messages[1].Raw != "settled" || held.Messages[1].Sub != "earlier" || active.Turns[0].Reply != "provisional" || len(active.Turns[0].Items) != 1 {
		t.Fatal("provisional hold changed source or visible settled body")
	}
	// Literal stale recipe promotes only the real original open tail and does
	// not manufacture the missing event time or change represented Items.
	at := time.UnixMilli(1000)
	s = wireTestSession(transcript.Turn{Prompt: "old", Started: at, Open: true}, transcript.Turn{Prompt: "tail", Started: time.UnixMilli(2000), Open: true, Earlier: []string{"keep", "promote"}, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "represented"}}})
	o = wireTestOptions()
	o.stale = true
	render := func(got *transcript.Session, opts convoStyledOptions) ([]convoStyledMessage, error) {
		last := got.Turns[1]
		if last.Open || last.Reply != "promote" || !reflect.DeepEqual(last.Earlier, []string{"keep"}) || last.ReplyAt != (time.Time{}) || last.Ended != (time.Time{}) || len(last.Items) != 1 || last.Items[0].Text != "represented" {
			t.Error("stale normalization departed from loader recipe")
		}
		return renderConvoStyled(got, opts)
	}
	w, _, _, err := buildNavConversationWith(s, o, render, json.Marshal)
	if err != nil {
		t.Fatal(err)
	}
	last := w.Messages[len(w.Messages)-1]
	if last.Sub != "final" || last.Raw != "promote" || last.At.Known {
		t.Fatal("stale promotion fabricated final event time")
	}
	if !s.Turns[1].Open || s.Turns[1].Reply != "" || len(s.Turns[1].Earlier) != 2 {
		t.Fatal("stale source modified")
	}
	o.anchor = 1000
	o.last = 1
	o.stale = false
	o.live = true
	o.hold = true
	w, _, _, err = buildNavConversationWith(s, o, func(got *transcript.Session, opts convoStyledOptions) ([]convoStyledMessage, error) {
		if len(got.Turns) != 1 || !got.Turns[0].Open || opts.live || opts.hold || opts.start != 0 {
			t.Error("historical tail acquired current-tail facts")
		}
		return renderConvoStyled(got, opts)
	}, json.Marshal)
	if err != nil || w.LaterHidden != 1 {
		t.Fatal("historical window render")
	}
}
