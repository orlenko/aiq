package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func detailTestKey(t *testing.T, f *detailTestFixture, q navDetailQuery, facts navDetailFacts) (transcript.ViewKey, navConversationOptions) {
	t.Helper()
	s, snap, err := f.cache.Session(context.Background(), f.path)
	if err != nil {
		t.Fatal(err)
	}
	facts, err = navDetailOwnFacts(facts, len(s.Turns))
	if err != nil {
		t.Fatal(err)
	}
	o := navConversationOptions{provider: q.provider, id: q.id, last: q.last, markup: q.markup, anchor: q.anchor, now: f.deps.now(), location: f.deps.location, live: facts.live, hold: facts.hold, stale: facts.stale, accounts: facts.accounts, skippedNote: f.deps.skippedNote, liveMark: f.deps.liveMark, bodyBytes: f.limits.bodyBytes}
	key, err := navDetailContextKey(context.Background(), context.Background(), s, snap, q, facts, o, f.deps.build)
	if err != nil {
		t.Fatal(err)
	}
	return key, o
}

func TestNavDetailPrivateContextLiteralAndLeaseFacts(t *testing.T) {
	// Independent Python hashlib/struct.pack('>Q', UTF8 length) vector for the
	// empty-turn context, with false='0', true='1' as specified before code.
	key, err := navDetailContextKey(context.Background(), context.Background(), &transcript.Session{}, transcript.Snapshot{}, navDetailQuery{provider: "claude", id: detailTestID, last: 50, markup: true}, navDetailFacts{leaseKnown: true}, navConversationOptions{bodyBytes: 4096, skippedNote: "skip", liveMark: "!"}, "fixture-build")
	if err != nil || key.Lease != "4db019e48202bf3db4ca8473497095e0f8811e763cffb27dadd9c1b83e68b963" {
		t.Fatal("private LP frame differs from literal bool/domain/budget recipe", key.Lease, err)
	}
}

func TestNavDetailPrivateLeaseFacts(t *testing.T) {
	f := detailTestNew(t)
	facts := navDetailFacts{}
	var projects atomic.Int32
	f.deps.facts = func(context.Context, transcript.Hit, *transcript.Session) (navDetailFacts, error) { return facts, nil }
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		projects.Add(1)
		return projectNavConversation(s, o)
	}
	h := f.handler(t)
	etag := ""
	for i, next := range []navDetailFacts{{}, {leaseKnown: true}, {leaseKnown: true, leaseIdentity: "lease-7@123"}, {leaseKnown: true, leaseIdentity: "lease-7@456"}} {
		facts = next
		w := detailTestServe(h, detailTestConditional(etag))
		if i == 0 {
			detailTestOK(t, w)
			etag = w.Header().Get("ETag")
		} else if w.status != 304 || w.writes != 0 {
			t.Fatal("identical body did not retain content ETag after lease-context reevaluation")
		}
		if projects.Load() != int32(i+1) {
			t.Fatal("lease-known/generation context reused old build")
		}
		q, _, _ := navDetailDecode(detailTestRequest())
		k, _ := detailTestKey(t, f, q, next)
		v, err := f.cache.ViewContext(context.Background(), k, func() (*transcript.View, error) {
			t.Error("expected retained view missing")
			return nil, errNavDetailChanged
		})
		if err != nil || v.Settled != next.leaseKnown {
			t.Fatal("unknown lease incorrectly certified settled")
		}
		if strings.Contains(string(w.body), "lease-7@") || strings.Contains(w.Header().Get("ETag"), "lease-7@") {
			t.Fatal("private lease token escaped")
		}
	}
	facts = navDetailFacts{leaseIdentity: "unknown-but-nonempty"}
	detailTestError(t, detailTestServe(h, detailTestRequest()), 500, "internal_error")
}

func TestNavDetailActualClockContextChanges(t *testing.T) {
	for _, tc := range []struct {
		name              string
		at, before, after time.Time
		first, last       string
	}{
		{"six-day-midday", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), time.Date(2026, 1, 7, 11, 59, 59, 0, time.UTC), time.Date(2026, 1, 7, 12, 0, 1, 0, time.UTC), "Thu 12:00", "Jan 1 12:00"},
		{"midnight", time.Date(2026, 1, 1, 23, 59, 0, 0, time.UTC), time.Date(2026, 1, 1, 23, 59, 30, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 30, 0, time.UTC), "today 23:59", "Thu 23:59"},
		{"year", time.Date(2025, 12, 20, 12, 0, 0, 0, time.UTC), time.Date(2025, 12, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), "Dec 20 12:00", "2025-12-20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := detailTestNew(t)
			f.at = tc.at
			f.write(t, "question-old")
			now := tc.before
			f.deps.now = func() time.Time { return now }
			h := f.handler(t)
			first := detailTestServe(h, detailTestRequest())
			detailTestOK(t, first)
			if !strings.Contains(string(first.body), `"clock_text":"`+tc.first+`"`) {
				t.Fatal("initial literal clock absent")
			}
			now = tc.after
			second := detailTestServe(h, detailTestConditional(first.Header().Get("ETag")))
			detailTestOK(t, second)
			if !strings.Contains(string(second.body), `"clock_text":"`+tc.last+`"`) || second.Header().Get("ETag") == first.Header().Get("ETag") || f.cache.Stats().Parses != 1 {
				t.Fatal("unchanged Snapshot reused stale display clock")
			}
		})
	}
	f := detailTestNew(t)
	f.deps.location = time.FixedZone("same-name", 0)
	first := detailTestServe(f.handler(t), detailTestRequest())
	detailTestOK(t, first)
	f.deps.location = time.FixedZone("same-name", 3600)
	second := detailTestServe(f.handler(t), detailTestConditional(first.Header().Get("ETag")))
	detailTestOK(t, second)
	if !strings.Contains(string(first.body), `Thu 12:00`) || !strings.Contains(string(second.body), `Thu 13:00`) {
		t.Fatal("same zone name hid different fixed offsets")
	}
}

func TestNavDetailEncodedLimitConstructorAndHeldEstimate(t *testing.T) {
	f := detailTestNew(t)
	var encoded atomic.Int32
	f.deps.marshal = func(v any) ([]byte, error) { encoded.Add(1); return json.Marshal(v) }
	h := f.handler(t)
	first := detailTestServe(h, detailTestRequest())
	detailTestOK(t, first)
	if encoded.Load() != 1 {
		t.Fatal("success envelope encoded more than once")
	}
	f.limits.bodyBytes = int64(len(first.body))
	equal := f.handler(t)
	again := detailTestServe(equal, detailTestRequest())
	detailTestOK(t, again)
	if string(again.body) != string(first.body) {
		t.Fatal("body policy entered public body")
	}
	f.limits.bodyBytes--
	small := f.handler(t)
	before := encoded.Load()
	detailTestError(t, detailTestServe(small, detailTestRequest()), 503, "view_too_large")
	if encoded.Load() != before {
		t.Fatal("known overflow was marshaled")
	}
	for _, limits := range []navDetailLimits{{0, 1}, {1000, 0}, {1, 1}} {
		if _, err := newNavDetailHandler(f.deps, limits); err == nil {
			t.Fatal("invalid limits accepted")
		}
	}
	for _, build := range []string{"", strings.Repeat("a", 129), "space build", "line\nbuild", "é"} {
		d := f.deps
		d.build = build
		if _, err := newNavDetailHandler(d, navDetailLimits{4096, 1}); err == nil {
			t.Fatal("invalid Build accepted")
		}
	}
	// Valid serialized output can have a retained graph exceeding the cache's
	// view budget because spare array capacity remains owned by that graph.
	f = detailTestNew(t)
	f.deps.project = func(s *transcript.Session, o navConversationOptions) (*navConversationWindow, error) {
		w, err := projectNavConversation(s, o)
		if err != nil {
			return nil, err
		}
		original := w.Messages[0].Groups[0].Lines
		oversized := make([]navConversationLine, len(original), 400000)
		copy(oversized, original)
		w.Messages[0].Groups[0].Lines = oversized
		return w, nil
	}
	large := f.handler(t)
	success := detailTestServe(large, detailTestRequest())
	detailTestOK(t, success)
	if f.cache.Stats().OversizedViews != 1 {
		t.Fatal("spare DTO capacity not charged to retained view")
	}
	f.limits.bodyBytes = 256
	detailTestError(t, detailTestServe(f.handler(t), detailTestConditional("*")), 503, "view_too_large")
	detailTestOK(t, detailTestServe(large, detailTestRequest()))
}

func TestNavDetailMalformedCachedValueConsumerGuard(t *testing.T) {
	for _, mode := range []string{"type", "nil", "fingerprint", "uppercase", "length", "empty", "budget"} {
		t.Run(mode, func(t *testing.T) {
			f := detailTestNew(t)
			f.limits.bodyBytes = 256
			h := f.handler(t)
			q, _, _ := navDetailDecode(detailTestRequest())
			key, _ := detailTestKey(t, f, q, navDetailFacts{leaseKnown: true})
			body := []byte(`{}`)
			if mode == "budget" {
				body = make([]byte, 257)
			}
			hash := sha256.Sum256(body)
			fingerprint := hex.EncodeToString(hash[:])
			value := &navDetailView{body: body, fingerprint: fingerprint, encodedLength: int64(len(body))}
			view := &transcript.View{Value: value, ETag: fingerprint, Bytes: 512, Settled: true}
			switch mode {
			case "type":
				view.Value = "not-private-value"
			case "nil":
				view.Value = (*navDetailView)(nil)
			case "fingerprint":
				view.ETag = strings.Repeat("a", 64)
			case "uppercase":
				value.fingerprint = strings.ToUpper(fingerprint)
				view.ETag = value.fingerprint
			case "length":
				value.encodedLength++
			case "empty":
				value.body = nil
				value.encodedLength = 0
			}
			if _, err := f.cache.ViewContext(context.Background(), key, func() (*transcript.View, error) { return view, nil }); err != nil {
				t.Fatal(err)
			}
			for _, tag := range []string{"", "*", `"` + fingerprint + `"`} {
				detailTestError(t, detailTestServe(h, detailTestConditional(tag)), 500, "internal_error")
			}
		})
	}
}

// Independent structural enumeration counts all backing capacity, including
// strings/slices in unused slots. This checks the typed production estimator
// without repeating its hand-maintained list of DTO fields.
func detailTestOwnedExtras(v reflect.Value) int64 {
	switch v.Kind() {
	case reflect.String:
		return int64(v.Len())
	case reflect.Struct:
		var n int64
		for i := 0; i < v.NumField(); i++ {
			n += detailTestOwnedExtras(v.Field(i))
		}
		return n
	case reflect.Slice:
		n := int64(v.Cap()) * int64(v.Type().Elem().Size())
		full := v.Slice(0, v.Cap())
		for i := 0; i < full.Len(); i++ {
			n += detailTestOwnedExtras(full.Index(i))
		}
		return n
	}
	return 0
}
func TestNavDetailRetainedGraphCapacityOracle(t *testing.T) {
	v := &navDetailView{body: make([]byte, 3, 19), fingerprint: strings.Repeat("f", 64), envelope: navDetailEnvelope{Build: "fixture-build", Messages: make([]navConversationMessage, 1, 3)}}
	m := &v.envelope.Messages[0]
	m.Raw = "visible"
	m.Groups = make([]navConversationGroup, 1, 2)
	g := &m.Groups[0]
	g.Base = make([]string, 1, 3)
	g.Base[0] = "muted"
	g.Lines = make([]navConversationLine, 1, 2)
	g.Lines[0].Spans = make([]navConversationSpan, 1, 2)
	g.Lines[0].Spans[0].Text = "span"
	g.Lines[0].Spans[0].Classes = []string{"code"}
	// These payloads are not serialized, yet the backing graph retains them.
	v.envelope.Messages[:cap(v.envelope.Messages)][1].Raw = strings.Repeat("hidden spare", 100)
	g.Lines[:cap(g.Lines)][1].Spans = []navConversationSpan{{Text: strings.Repeat("spare payload", 100)}}
	value := reflect.ValueOf(v).Elem()
	minimum := int64(value.Type().Size()) + detailTestOwnedExtras(value)
	got, ok := navDetailRetainedBytes(v)
	if !ok || got < minimum {
		t.Fatalf("retained estimate %d/%v below independent owned graph %d", got, ok, minimum)
	}
}
