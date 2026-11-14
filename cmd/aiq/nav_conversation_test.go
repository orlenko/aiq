package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

const wireTestID = "00000001-0000-4000-8000-000000000001"

func wireTestOptions() navConversationOptions {
	return navConversationOptions{provider: "claude", id: wireTestID, last: 50, markup: true, now: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC), location: time.UTC, skippedNote: "EXPLICIT fixture skipped", liveMark: "!", bodyBytes: 4 << 20}
}
func wireTestSession(turns ...transcript.Turn) *transcript.Session {
	return &transcript.Session{Provider: "claude", ID: wireTestID, Cwd: "/fixture/work", Title: "title", Model: "model", Branch: "branch", Path: "PRIVATE-TRANSCRIPT-PATH", Turns: turns}
}
func wireTestBuild(t *testing.T, s *transcript.Session, o navConversationOptions) (*navConversationWindow, []byte) {
	t.Helper()
	w, b, f, err := buildNavConversation(s, o)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if f != hex.EncodeToString(sum[:]) {
		t.Fatal("fingerprint is not SHA256 of exact served bytes")
	}
	encoded, err := json.Marshal(w)
	if err != nil || !bytes.Equal(encoded, b) {
		t.Fatal("wire/body disagree")
	}
	return w, b
}
func wireTestWholeFailure(t *testing.T, w *navConversationWindow, b []byte, f string, err, want error) {
	t.Helper()
	if w != nil || b != nil || f != "" || !errors.Is(err, want) {
		t.Fatalf("whole failure: window=%v bodyNil=%v fingerprint=%q err=%v want %v", w != nil, b == nil, f, err, want)
	}
}

func TestNavConversationIndependentKeyVectors(t *testing.T) {
	// Fixed values calculated independently with Python hashlib + struct.pack
	// ('>Q', len(field.encode('utf-8'))), never the production key helper.
	base := []string{"aiq.nav.turn.v1", "claude", wireTestID, "0", "2", "PRIVATE-key\x00雪"}
	for _, tc := range []struct {
		fields []string
		want   string
	}{
		{base, "41a34c9b2a9a5d465ebdfaf68b4635ef1ff8b648ff9604a6cb4791f1b2339beb"},
		{[]string{"aiq.nav.turn.v1", "codex", wireTestID, "0", "2", "PRIVATE-key\x00雪"}, "45837265cd60d5ac58cf2f9f02ac1d55d7e94ee1907bdfb9a6aaef64eedbacdd"},
		{[]string{"aiq.nav.turn.v1", "claude", "00000001-0000-4000-8000-000000000002", "0", "2", "PRIVATE-key\x00雪"}, "293916ad3313c0c61e7730795417688e6cf36f2b0aea8603f7087613ac3f1cd9"},
		{[]string{"aiq.nav.turn.v1", "claude", wireTestID, "1", "2", "PRIVATE-key\x00雪"}, "d3a795c4c7bec2a4870a77f8f65f931efb963f10e1300d66c4ae08b078d92eb1"},
		{[]string{"aiq.nav.turn.v1", "claude", wireTestID, "0", "3", "PRIVATE-key\x00雪"}, "0cdab074d3971824bbc4e0f2995e88db17315e78f158f16b0c313f7bfd63cfe0"},
		{[]string{"aiq.nav.preamble.v1", "claude", wireTestID, "0", "0123456789abcdef0123456789abcdef"}, "843ef970c6094c3fbf167b3feffe264dc66a1bb9a8609f41f408dcd4d82823ac"},
		{[]string{"aiq.nav.preamble.v1", "claude", wireTestID, "0", "1123456789abcdef0123456789abcdef"}, "30dd479fb9a91e99b1fa4ac3027e47eddfd078788c3aaa82bfe00b8180e5df53"},
		{[]string{"ab", "c"}, "601d5476e2ccfe2c87a2bba7a322659734a05749d5b5aa781f513e4912db0d5f"},
		{[]string{"a", "bc"}, "3fafa1cf2f19a7c1129beb20cf0983f73a489a221fc0dd2f16d1be292d089205"},
	} {
		if got := navConversationKey(tc.fields...); got != tc.want {
			t.Fatalf("key %q got %s want %s", tc.fields, got, tc.want)
		}
	}
}

func TestNavConversationHumanWindowBounds(t *testing.T) {
	turns := []transcript.Turn{
		{Prompt: "first", Started: time.UnixMilli(1000)},
		{Source: transcript.Peer, Prompt: "peer", Started: time.UnixMilli(2000).Add(100 * time.Microsecond)},
		{Prompt: "skipped", PromptSkipped: true, Started: time.UnixMilli(2000).Add(900 * time.Microsecond)},
		{Source: transcript.Peer, Prompt: `{"type":"idle_notification"}`, Started: time.UnixMilli(3000)},
		{Prompt: "/clear", Open: true, Started: time.UnixMilli(4000)},
		{Source: transcript.Notice, Prompt: "notice", Started: time.UnixMilli(5000)},
		{Prompt: "last", Started: time.UnixMilli(6000)},
		{Source: transcript.Aiq, Prompt: "trailing", Started: time.UnixMilli(7000)},
	}
	for _, tc := range []struct {
		last       int
		anchor     int64
		start, end int
	}{{1, 0, 6, 8}, {2, 0, 4, 8}, {50, 0, 0, 8}, {10000, 0, 0, 8}, {1, 1000, 0, 2}, {1, 2000, 1, 4}, {1, 3000, 3, 6}, {1, 4000, 4, 6}, {2, 2000, 1, 6}, {1, 6000, 6, 8}, {1, 7000, 7, 8}} {
		start, end, err := navConversationBounds(turns, tc.last, tc.anchor)
		if err != nil || start != tc.start || end != tc.end {
			t.Fatalf("bounds last=%d anchor=%d: %d,%d %v want %d,%d", tc.last, tc.anchor, start, end, err, tc.start, tc.end)
		}
	}
	for _, last := range []int{0, -1, 10001} {
		if _, _, err := navConversationBounds(turns, last, 0); !errors.Is(err, errNavConversationInvalid) {
			t.Fatal("invalid last accepted")
		}
	}
	for _, anchor := range []int64{-1, 10000000000000} {
		if _, _, err := navConversationBounds(turns, 1, anchor); !errors.Is(err, errNavConversationInvalid) {
			t.Fatal("invalid anchor accepted")
		}
	}
	if _, _, err := navConversationBounds(turns, 1, 2001); !errors.Is(err, errNavConversationAnchor) {
		t.Fatal("missing anchor guessed neighboring turn")
	}
	for _, src := range []transcript.Source{transcript.Peer, transcript.Notice} {
		only := []transcript.Turn{{Source: src, Started: time.UnixMilli(1000)}, {Source: src}}
		a, b, err := navConversationBounds(only, 1, 1000)
		if err != nil || a != 0 || b != 2 {
			t.Fatal("notice-only anchor consumed Human quota")
		}
	}
	if a, b, err := navConversationBounds(nil, 1, 0); err != nil || a != 0 || b != 0 {
		t.Fatal("empty bounds")
	}
	// UnixMilli on this instant would overflow if conversion preceded checking.
	if _, _, err := navConversationBounds([]transcript.Turn{{Started: time.Unix(1<<62, 0)}}, 1, 1000); err == nil {
		t.Fatal("unsafe anchor scan accepted")
	}
}

func TestNavConversationTimeKnowledgeAndSafeInterval(t *testing.T) {
	const m int64 = 9007199254740991
	for _, tc := range []struct {
		at   time.Time
		want navWireTime
	}{{time.Time{}, navWireTime{}}, {time.UnixMilli(0), navWireTime{MS: 0, Known: true}}, {time.UnixMilli(-1), navWireTime{MS: -1, Known: true}}, {time.UnixMilli(-m), navWireTime{MS: -m, Known: true}}, {time.UnixMilli(m).Add(time.Millisecond - time.Nanosecond), navWireTime{MS: m, Known: true}}} {
		got, err := navConversationTime(tc.at)
		if err != nil || got != tc.want {
			t.Fatalf("time %+v %v want %+v", got, err, tc.want)
		}
	}
	for _, at := range []time.Time{time.UnixMilli(-m).Add(-time.Nanosecond), time.UnixMilli(m).Add(time.Millisecond), time.Unix(1<<62, 0), time.Unix(-1<<62, 0)} {
		if _, err := navConversationTime(at); err == nil {
			t.Fatal("unsafe time accepted")
		}
	}
}

func TestNavConversationLiteralEmptyAndPreambles(t *testing.T) {
	o := wireTestOptions()
	w, b := wireTestBuild(t, wireTestSession(), o)
	const empty = `{"session":{"provider":"claude","id":"00000001-0000-4000-8000-000000000001","cwd":"/fixture/work","title":"title","model":"model","branch":"branch","started":{"ms":0,"known":false},"updated":{"ms":0,"known":false},"worker":false,"bypass":false},"earlier_hidden":0,"later_hidden":0,"window_start":0,"reset":true,"markup":true,"messages":[]}`
	if string(b) != empty || w.Messages == nil {
		t.Fatalf("literal empty window %s", b)
	}
	for _, provider := range []string{"claude", "codex", "agy"} {
		s := wireTestSession()
		s.Provider = provider
		o.provider = provider
		w, _ := wireTestBuild(t, s, o)
		if w.Session.Provider != provider {
			t.Fatal("scope provider changed")
		}
	}
	o = wireTestOptions()
	s := wireTestSession()
	s.Skipped = 2
	o.accounts = []convoAccountFact{{known: true, label: "unrelated turn zero"}}
	w, b = wireTestBuild(t, s, o)
	if len(w.Messages) != 2 || w.Messages[0].Key == w.Messages[1].Key || w.Messages[0].ID == w.Messages[1].ID {
		t.Fatal("preamble occurrence identity lost")
	}
	for _, message := range w.Messages {
		if message.Turn != -1 || message.Kind != "skipped" || message.Sub != "skipped" || message.At != (navWireTime{}) || message.SourceKnown || message.Source != "unknown" || message.AccountKnown || message.Account != "" || message.Raw != "EXPLICIT fixture skipped" {
			t.Fatal("preamble facts borrowed turn zero", message)
		}
		if len(message.Groups) != 1 || message.Groups[0].Head.Mode != "" || message.Groups[0].Lines[0].Spans[0].Text != "  · EXPLICIT fixture skipped" {
			t.Fatal("preamble recipe lost")
		}
	}
	if strings.Contains(string(b), "PRIVATE-TRANSCRIPT-PATH") || strings.Contains(string(b), "unrelated turn zero") {
		t.Fatal("private path/account leaked")
	}
	s.Turns = []transcript.Turn{{Prompt: "hidden", Open: true}, {Prompt: "selected", Open: true}}
	o.last = 1
	w, _ = wireTestBuild(t, s, o)
	if w.WindowStart != 1 || w.EarlierHidden != 1 || len(w.Messages) != 1 || w.Messages[0].Turn != 1 || w.Messages[0].Kind == "skipped" {
		t.Fatal("hidden prefix leaked session preamble")
	}
}

func TestNavConversationOriginalAccountsAndKnowledge(t *testing.T) {
	for _, tc := range []struct {
		source         transcript.Source
		skipped, known bool
		token          string
	}{{transcript.Human, false, true, "human"}, {transcript.Aiq, false, true, "aiq"}, {transcript.Peer, false, true, "peer"}, {transcript.Notice, false, true, "notice"}, {transcript.Human, true, false, "unknown"}, {transcript.Peer, true, true, "peer"}, {transcript.Source(255), false, false, "unknown"}} {
		s := wireTestSession(transcript.Turn{Prompt: "hidden", Open: true}, transcript.Turn{Prompt: "selected", Source: tc.source, PromptSkipped: tc.skipped, Skipped: 1, Started: time.UnixMilli(1234), Open: true})
		o := wireTestOptions()
		o.anchor = 1234
		o.last = 1
		o.accounts = []convoAccountFact{{known: true, label: "hidden account"}, {known: true, label: ""}}
		w, _ := wireTestBuild(t, s, o)
		if w.WindowStart != 1 || w.EarlierHidden != 1 || w.LaterHidden != 0 {
			t.Fatal("window coordinate rebased")
		}
		for _, message := range w.Messages {
			if message.Turn != 1 || message.SourceKnown != tc.known || message.Source != tc.token || !message.AccountKnown || message.Account != "" {
				t.Fatal("classification/account facts", message)
			}
			if tc.skipped && message.Sub == "skipped_prompt" && message.At != (navWireTime{}) {
				t.Fatal("skipped prompt borrowed Started event time")
			}
			if (message.Sub == "skipped_prompt" || message.Sub == "skipped") && message.Raw != "EXPLICIT fixture skipped" {
				t.Fatal("turn-local skip lost explicit explanation")
			}
		}
	}
	o := wireTestOptions()
	o.accounts = []convoAccountFact{{label: "PRIVATE-UNKNOWN-ACCOUNT"}}
	w, body := wireTestBuild(t, wireTestSession(transcript.Turn{Prompt: "p", Open: true}), o)
	if w.Messages[0].AccountKnown || w.Messages[0].Account != "" || strings.Contains(string(body), "PRIVATE-UNKNOWN-ACCOUNT") {
		t.Fatal("unknown account label serialized")
	}
}

func TestNavConversationLiteralBranchFactsAndIDs(t *testing.T) {
	at := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	s := wireTestSession(transcript.Turn{Prompt: "ask", Started: at, Ended: at.Add(5 * time.Minute), Reply: "finish", ReplyAt: at.Add(4 * time.Minute), Tools: 2, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "reply", At: at.Add(time.Minute)}, {Kind: transcript.ItemSteer, Text: "steer", At: at.Add(2 * time.Minute)}, {Kind: transcript.ItemAnswer, Text: "early", At: at.Add(3 * time.Minute)}, {Kind: transcript.ItemSaid, Text: "aside", At: at.Add(4 * time.Minute)}, {Kind: transcript.ItemAnswer, Text: "finish"}}})
	s.Started = time.UnixMilli(0)
	s.Updated = time.UnixMilli(-1)
	s.Worker = true
	s.Bypass = true
	o := wireTestOptions()
	o.accounts = []convoAccountFact{{known: true, label: "account"}}
	w, _ := wireTestBuild(t, s, o)
	if w.Session.Started != (navWireTime{MS: 0, Known: true}) || w.Session.Updated != (navWireTime{MS: -1, Known: true}) || !w.Session.Worker || !w.Session.Bypass {
		t.Fatal("session time/flags borrowed from turn")
	}
	want := []struct {
		kind, sub, raw, head, clock, text string
		offset                            int
		known                             bool
	}{{"you", "prompt", "ask", "prompt", "relative", "today 09:00", 0, true}, {"answer", "reply", "reply", "reply", "reply", "09:01:00", 1, true}, {"steer", "steer", "steer", "steer", "", "", 2, true}, {"answer", "earlier", "early", "earlier", "", "", 3, true}, {"line", "said", "aside", "", "", "", 4, true}, {"answer", "final", "finish", "final", "", "", 0, false}}
	if len(w.Messages) != len(want) {
		t.Fatal("mixed branch count")
	}
	for i, expected := range want {
		m := w.Messages[i]
		if m.Kind != expected.kind || m.Sub != expected.sub || m.Raw != expected.raw || m.Turn != 0 || m.Key != w.Messages[0].Key || !m.AccountKnown || m.Account != "account" || !m.SourceKnown || m.Source != "human" {
			t.Fatalf("branch %d facts %+v", i, m)
		}
		ms := int64(0)
		if expected.known {
			ms = at.Add(time.Duration(expected.offset) * time.Minute).UnixMilli()
		}
		if m.At != (navWireTime{MS: ms, Known: expected.known}) {
			t.Fatal("branch event time borrowed", i, m.At)
		}
		h := m.Groups[0].Head
		if h.Mode != expected.head || h.ClockMode != expected.clock || h.ClockText != expected.text {
			t.Fatal("branch head recipe", i, h)
		}
	}
	h := w.Messages[5].Groups[0].Head
	if !h.DurationKnown || h.DurationMS != 300000 || h.Tools != 2 || h.At != (navWireTime{}) {
		t.Fatal("final duration/tools/event domains conflated")
	}
	// Independent fixed web identity for #0 / you / full raw 'ask'.
	s = wireTestSession(transcript.Turn{Prompt: "ask", Open: true})
	o = wireTestOptions()
	w, _ = wireTestBuild(t, s, o)
	if w.Messages[0].ID != "2ff22e2650fce3dd7a208469e8a169c7" || w.Messages[0].Key != "24ef50b377ebdbd7539ecba3fec57e404ca9d9b0cd309e0e1f742103b29a9219" {
		t.Fatal("literal web message/turn identity")
	}
	id := w.Messages[0].ID
	s.Turns[0].Open = false
	w, _ = wireTestBuild(t, s, o)
	if w.Messages[0].ID != id || len(w.Messages[0].Groups) != 2 || w.Messages[0].Groups[1].Head.Mode != "final" {
		t.Fatal("none changed full copy body identity")
	}
	s.Turns[0].Reply = "answer"
	w, _ = wireTestBuild(t, s, o)
	if w.Messages[1].ID != "9c709676c1ad6b96d82322b6f46c680f" {
		t.Fatal("synthetic answer ID changed")
	}
	for _, tc := range []struct {
		delta time.Duration
		ms    int64
	}{{0, 0}, {-1500 * time.Microsecond, -1}} {
		s = wireTestSession(transcript.Turn{Prompt: "p", Reply: "answer", Started: at, Ended: at.Add(tc.delta)})
		w, _ = wireTestBuild(t, s, o)
		h = w.Messages[1].Groups[0].Head
		if !h.DurationKnown || h.DurationMS != tc.ms {
			t.Fatal("known signed/zero duration lost")
		}
	}
	// Existing Earlier-origin-selected-final exception has no event timestamp.
	s = wireTestSession(transcript.Turn{Prompt: "p", Started: at, Ended: at.Add(time.Minute), ReplyAt: at.Add(30 * time.Second), Reply: "b", Earlier: []string{"b"}, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "a", At: at}}})
	w, _ = wireTestBuild(t, s, o)
	m := w.Messages[len(w.Messages)-1]
	if m.Sub != "final" || m.At != (navWireTime{}) {
		t.Fatal("Earlier-origin final borrowed ReplyAt")
	}
	o.hold = true
	w, _ = wireTestBuild(t, s, o)
	m = w.Messages[len(w.Messages)-1]
	if m.Sub != "earlier" || m.At != (navWireTime{}) {
		t.Fatal("held Earlier-origin event changed")
	}
}

func TestNavConversationClockAndGroupFidelity(t *testing.T) {
	o := wireTestOptions()
	at := time.Date(2026, 1, 2, 0, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		zone            *time.Location
		relative, reply string
	}{{time.UTC, "today 00:30", "00:30:00"}, {time.FixedZone("fixture-minus-two", -7200), "Thu 22:30", "22:30:00"}} {
		o.location = tc.zone
		s := wireTestSession(transcript.Turn{Prompt: "ask", Started: at, Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "reply", At: at}}})
		w, _ := wireTestBuild(t, s, o)
		if w.Messages[0].Groups[0].Head.ClockText != tc.relative || w.Messages[1].Groups[0].Head.ClockText != tc.reply {
			t.Fatal("explicit timezone/date boundary ignored")
		}
		s.Turns[0].Items[0].At = time.Time{}
		w, _ = wireTestBuild(t, s, o)
		h := w.Messages[1].Groups[0].Head
		if h.ClockText != "reply" || h.At.Known || w.Messages[1].At.Known {
			t.Fatal("unknown reply clock fabricated a timestamp")
		}
	}
	o = wireTestOptions()
	s := wireTestSession(transcript.Turn{Source: transcript.Notice, Prompt: transcript.OrchestraNotice, Started: at, Open: true})
	w, _ := wireTestBuild(t, s, o)
	h := w.Messages[0].Groups[0].Head
	if h.Mode != "notice" || h.Who != "orchestra inbox" || h.ClockMode != "reply" || h.ClockText != "00:30:00" {
		t.Fatal("notice clock recipe lost")
	}
	for _, markup := range []bool{false, true} {
		o.markup = markup
		s = wireTestSession(transcript.Turn{Source: transcript.Peer, Prompt: "peer", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "aside", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "hidden\ncopy", MsgID: "m"}, {Kind: transcript.ItemAnswer, Text: "```go\ncode"}, {Kind: transcript.ItemAnswer, Text: "|h|\n|-|\n|v|"}, {Kind: transcript.ItemAnswer, Text: "> quote"}, {Kind: transcript.ItemAnswer, Text: "---"}}})
		w, _ = wireTestBuild(t, s, o)
		if len(w.Messages) != 6 {
			t.Fatal("folded group boundary count")
		}
		m := w.Messages[1]
		if m.Raw != "aside\n\nhidden\ncopy" || len(m.Groups) != 2 || !m.Groups[0].Visible || m.Groups[1].Visible || m.Groups[1].Head.Mode != "" || m.Groups[1].Lines == nil || len(m.Groups[1].Lines) != 0 {
			t.Fatal("invisible part flattened or uncopied")
		}
		if w.Messages[2].Groups[0].Lines[0].Kind != "code" || w.Messages[3].Groups[0].Lines[0].Kind != "table" || w.Messages[3].Groups[0].Lines[0].TableRows != 2 || w.Messages[4].Groups[0].Lines[0].Kind != "quote" {
			t.Fatal("separate Markdown groups reparsed together")
		}
		rule := w.Messages[5].Groups[0]
		if rule.Head.Mode != "earlier" || !rule.LeadingBreak || !reflect.DeepEqual(rule.Base, []string{"muted"}) || rule.Lines[0].Kind != "rule" || rule.Lines[0].Spans[0].Decorative == markup {
			t.Fatal("base/line/decorative/leading recipe changed")
		}
		for _, m := range w.Messages {
			for _, g := range m.Groups {
				for _, line := range g.Lines {
					if line.TableRows != 0 && line.TableRows != 2 {
						t.Fatal("table row prefix changed")
					}
					if markup {
						for _, span := range line.Spans {
							if span.Decorative {
								t.Fatal("marks mode hid visible content")
							}
						}
					}
				}
			}
		}
		s = wireTestSession(transcript.Turn{Prompt: "p", Open: true, Items: []transcript.TurnItem{{Kind: transcript.ItemSaid, Text: "left", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "```\n```", MsgID: "m"}, {Kind: transcript.ItemSaid, Text: "right", MsgID: "m"}}})
		w, _ = wireTestBuild(t, s, o)
		m = w.Messages[1]
		if len(m.Groups) != 3 || m.Raw != "left\n\n```\n```\n\nright" || m.Groups[1].EmptyBodyLine == markup || m.Groups[1].Head.Mode != "" {
			t.Fatal("headless empty fence recipe lost")
		}
		id := m.ID
		s.Turns[0].Items[2].Text = "right grows"
		w, _ = wireTestBuild(t, s, o)
		if w.Messages[1].ID == id || w.Messages[1].Key != m.Key {
			t.Fatal("complete body growth did not change message ID alone")
		}
	}
}

func TestNavConversationExplicitAllPreservesWholeHistoryAndBounds(t *testing.T) {
	turns := make([]transcript.Turn, 60)
	for i := range turns {
		at := time.UnixMilli(1000 + int64(i)*60000)
		turns[i] = transcript.Turn{Prompt: "invented prompt " + strconv.Itoa(i), Reply: "invented answer", Started: at, ReplyAt: at.Add(time.Second)}
	}
	s := wireTestSession(turns...)
	bounded := wireTestOptions()
	bounded.last = 50
	window, body := wireTestBuild(t, s, bounded)
	if window.EarlierHidden != 10 || window.WindowStart != 10 {
		t.Fatal("bounded window changed")
	}
	all := bounded
	all.last = 0
	all.all = true
	whole, complete := wireTestBuild(t, s, all)
	if whole.EarlierHidden != 0 || whole.LaterHidden != 0 || whole.WindowStart != 0 || !bytes.Contains(complete, []byte("invented prompt 0")) || !bytes.Contains(complete, []byte("invented prompt 59")) || len(complete) <= len(body) {
		t.Fatal("all did not return complete history")
	}
	for _, o := range []navConversationOptions{func() navConversationOptions { x := all; x.last = 50; return x }(), func() navConversationOptions { x := all; x.anchor = 1000; return x }()} {
		w, b, f, e := buildNavConversation(s, o)
		wireTestWholeFailure(t, w, b, f, e, errNavConversationInvalid)
	}
	all.bodyBytes = int64(len(body))
	w, b, f, e := buildNavConversation(s, all)
	wireTestWholeFailure(t, w, b, f, e, errNavConversationTooLarge)
}
