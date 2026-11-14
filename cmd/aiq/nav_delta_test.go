package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/transcript"
)

func deltaFixture(t *testing.T) (*navDetailHandler, *detailTestFixture, *navDetailView) {
	t.Helper()
	f := detailTestNew(t)
	data := append(detailTestData("first prompt", f.at), []byte(strings.ReplaceAll(string(detailTestData("second prompt", f.at.Add(time.Minute))), "fixture-message", "second-message"))...)
	if err := os.WriteFile(f.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.deps.prefixScope = func(transcript.Hit, transcript.Snapshot, string) string { return "invented-source-scope" }
	h := f.handler(t)
	w := detailTestServe(h, detailTestRequest())
	detailTestOK(t, w)
	var e navDetailEnvelope
	if err := json.Unmarshal(w.body, &e); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(w.body)
	return h, f, &navDetailView{envelope: e, body: append([]byte(nil), w.body...), fingerprint: hex.EncodeToString(digest[:]), encodedLength: int64(len(w.body))}
}

func TestNavDeltaUsesRequestLocalTailWithoutMutatingFullView(t *testing.T) {
	h, f, full := deltaFixture(t)
	if full.envelope.TailTurn != 1 || !navDeltaKey(full.envelope.TailKey) {
		t.Fatal("full view lacks complete prefix proof")
	}
	before := f.cache.Stats()
	path := "/api/nav/sessions/claude/" + detailTestID + "?from_key=" + full.envelope.TailKey + "&from_turn=1&last=50&markup=1"
	w := detailTestServe(h, httptest.NewRequest("GET", path, nil))
	detailTestOK(t, w)
	var e navDetailEnvelope
	if json.Unmarshal(w.body, &e) != nil || e.Reset || e.FromTurn != 1 || len(e.Messages) == 0 || e.TailKey != full.envelope.TailKey {
		t.Fatal("valid prefix did not produce a delta")
	}
	for _, m := range e.Messages {
		if m.Turn < 1 {
			t.Fatal("delta includes retained prefix")
		}
	}
	if string(w.body) == string(full.body) || w.Header().Get("ETag") == `"`+full.fingerprint+`"` {
		t.Fatal("delta reused full representation identity")
	}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	if got := detailTestServe(h, r); got.status != 304 || got.writes != 0 {
		t.Fatal("delta conditional did not use its exact encoded body")
	}
	again := detailTestServe(h, detailTestRequest())
	if string(again.body) != string(full.body) || again.Header().Get("ETag") != `"`+full.fingerprint+`"` || f.cache.Stats().ViewBuilds != before.ViewBuilds || f.cache.Stats().Parses != before.Parses {
		t.Fatal("delta mutated/multiplied the shared full view")
	}
}

func TestNavDeltaChecksEveryRetainedRenderedFactAndSourceContext(t *testing.T) {
	h, _, full := deltaFixture(t)
	mutations := map[string]func(*navDetailEnvelope){
		"raw":          func(e *navDetailEnvelope) { e.Messages[0].Raw += "changed" },
		"identity":     func(e *navDetailEnvelope) { e.Messages[0].ID = strings.Repeat("b", 32) },
		"key":          func(e *navDetailEnvelope) { e.Messages[0].Key = strings.Repeat("b", 64) },
		"origin":       func(e *navDetailEnvelope) { e.Messages[0].Source = "peer" },
		"origin known": func(e *navDetailEnvelope) { e.Messages[0].SourceKnown = !e.Messages[0].SourceKnown },
		"account":      func(e *navDetailEnvelope) { e.Messages[0].Account = "other"; e.Messages[0].AccountKnown = true },
		"paint":        func(e *navDetailEnvelope) { e.Messages[0].Groups[0].Lines[0].Spans[0].Text += "changed" },
		"header":       func(e *navDetailEnvelope) { e.Messages[0].Groups[0].Head.ClockText += "changed" },
		"visibility":   func(e *navDetailEnvelope) { e.Messages[0].Groups[0].Visible = false },
		"window":       func(e *navDetailEnvelope) { e.WindowStart = 1; e.EarlierHidden = 1 },
		"markup":       func(e *navDetailEnvelope) { e.Markup = !e.Markup },
		"build":        func(e *navDetailEnvelope) { e.Build = "other-build" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var e navDetailEnvelope
			if err := json.Unmarshal(full.body, &e); err != nil {
				t.Fatal(err)
			}
			mutate(&e)
			body, _ := json.Marshal(e)
			value := &navDetailView{envelope: e, body: body, fingerprint: "invented-full-etag"}
			r, used, err := h.deltaResponse(context.Background(), value, navDetailQuery{fromTurn: 1, fromKey: full.envelope.TailKey}, "invented-source-scope")
			if err != nil || !used || r.status != 200 || !reflect.DeepEqual(r.body, body) {
				t.Fatal("rewritten prefix was reused", err)
			}
		})
	}
	for _, source := range []string{"new-copy", "new-inode", "new-root-bundle", "new-lease-context"} {
		r, _, err := h.deltaResponse(context.Background(), full, navDetailQuery{fromTurn: 1, fromKey: full.envelope.TailKey}, source)
		if err != nil || !reflect.DeepEqual(r.body, full.body) {
			t.Fatal("source change was not a reset")
		}
	}
}

func TestNavDeltaCodecAndWholeResponseAdmission(t *testing.T) {
	base := "/api/nav/sessions/claude/" + detailTestID + "?last=50&markup=1"
	key := strings.Repeat("a", 64)
	for _, query := range []string{"&from_turn=0", "&from_key=" + key, "&from_turn=00&from_key=" + key, "&from_turn=9007199254740992&from_key=" + key, "&from_turn=0&from_key=" + strings.Repeat("A", 64), "&at=123&from_turn=0&from_key=" + key} {
		if _, _, ok := navDetailDecode(httptest.NewRequest("GET", base+query, nil)); ok {
			t.Fatal("ambiguous delta request accepted")
		}
	}
	if _, _, ok := navDetailDecode(httptest.NewRequest("GET", base+"&from_turn=0&from_key="+key, nil)); !ok {
		t.Fatal("zero tail index refused")
	}
	h, _, full := deltaFixture(t)
	e := full.envelope
	e.TailTurn = 0
	for i := range e.Messages {
		e.Messages[i].Turn = 0
	}
	e.TailKey, _ = navDeltaPrefix(context.Background(), &e, 0, "invented-source-scope")
	body, _ := json.Marshal(e)
	value := &navDetailView{envelope: e, body: body, fingerprint: "full"}
	h.limits.bodyBytes = int64(len(body))
	if _, _, err := h.deltaResponse(context.Background(), value, navDetailQuery{fromKey: e.TailKey}, "invented-source-scope"); err != errNavConversationTooLarge {
		t.Fatal("one-byte larger delta bypassed complete body admission", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := navDeltaPrefix(ctx, &e, 0, "scope"); err != context.Canceled {
		t.Fatal("prefix work ignored cancellation")
	}
}
