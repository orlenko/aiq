package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Same independently stated literal165-unit/eight-entry envelope as the
// existing JavaScript controller_data fixture; it is not a JSON byte count.
func detailStructureLiteral() navDetailEnvelope {
	return navDetailEnvelope{Build: "b", Session: navConversationSession{Provider: "agy", ID: "00000001-0000-4000-8000-000000000001", Cwd: "/f", Title: "T", Model: "M", Branch: "B", Bypass: true}, Reset: true,
		Messages: []navConversationMessage{{ID: strings.Repeat("0", 31) + "1", Key: strings.Repeat("a", 64), Kind: "you", Sub: "prompt", Turn: 0, SourceKnown: true, Source: "human", Raw: "r",
			Groups: []navConversationGroup{{Visible: true, Base: []string{"b", "b"}, Lines: []navConversationLine{{Kind: "text", Spans: []navConversationSpan{{Classes: []string{"i", "u"}, Text: "x"}}}}}}}}}
}

func TestNavDetailStructureMatchesLiteralBrowserUnits(t *testing.T) {
	for _, row := range []struct {
		entries, units int64
		wide, ok       bool
	}{{8, 165, false, true}, {7, 165, false, false}, {8, 164, false, false}, {8, 165, true, false}, {8, 166, true, true}} {
		e := detailStructureLiteral()
		if row.wide {
			e.Messages[0].Groups[0].Lines[0].Spans[0].Text = "😀"
		}
		err := navDetailAdmitStructure(context.Background(), &e, row.entries, row.units)
		if (err == nil) != row.ok {
			t.Fatal("Go/browser structural boundary differs", row, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := detailStructureLiteral()
	if !errors.Is(navDetailAdmitStructure(ctx, &e, 8, 165), context.Canceled) {
		t.Fatal("cancellation lost")
	}
}

func TestNavDetailDenseStylesUnderByteCapRefuseWholeStructure(t *testing.T) {
	e := detailStructureLiteral()
	spans := make([]navConversationSpan, 131073)
	for i := range spans {
		spans[i] = navConversationSpan{Classes: []string{"b"}, Text: "x"}
	}
	e.Messages[0].Groups[0].Lines[0].Spans = spans
	body, err := json.Marshal(e)
	if err != nil || len(body) >= 16<<20 {
		t.Fatal("fixture is not below encoded byte limit")
	}
	if err = navDetailAdmitStructure(context.Background(), &e, 262144, 16<<20); !errors.Is(err, errNavConversationTooLarge) {
		t.Fatal("compact styled200 would exceed browser admission", err)
	}
}

func TestNavDetailStructuralPoliciesCannotPoisonOrBypassEachOther(t *testing.T) {
	f := detailTestNew(t)
	f.limits.arrayEntries = 1
	f.limits.stringUnits = 1
	detailTestError(t, detailTestServe(f.handler(t), detailTestRequest()), 503, "view_too_large")
	// Same parse/view cache, more permissive policy must still succeed.
	f.limits.arrayEntries = 100000
	f.limits.stringUnits = 100000
	w := detailTestServe(f.handler(t), detailTestRequest())
	detailTestOK(t, w)
	f.limits.arrayEntries = 2
	f.limits.stringUnits = 2
	detailTestError(t, detailTestServe(f.handler(t), detailTestConditional(w.Header().Get("ETag"))), 503, "view_too_large")
	for _, l := range []navDetailLimits{{bodyBytes: 4096, requests: 1, arrayEntries: 1}, {bodyBytes: 4096, requests: 1, stringUnits: 1}, {bodyBytes: 4096, requests: 1, arrayEntries: -1, stringUnits: 1}} {
		if _, err := newNavDetailHandler(f.deps, l); err == nil {
			t.Fatal("incomplete structural policy admitted")
		}
	}
}
