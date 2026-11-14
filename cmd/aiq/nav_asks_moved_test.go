package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

func movedBody(t *testing.T, body []byte) navAsksMoved {
	t.Helper()
	var data struct {
		Moved navAsksMoved `json:"moved_today"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	if data.Moved.IDs == nil {
		t.Fatal("overview omitted or null IDs")
	}
	return data.Moved
}
func movedAsk(id int64, st state.AskState, reason string, at int64) state.Ask {
	return state.Ask{ID: id, State: st, StateAt: at, StateReason: reason, FirstQuote: "z", Refs: []string{}}
}

func TestNavAsksMovedHistoricalRootProjection(t *testing.T) {
	for _, known := range []bool{false, true} {
		s := responseSnapshot()
		s.ReportPresent = known
		body, err := buildNavAsksResponse(context.Background(), s, responseOptions())
		if err != nil {
			t.Fatal(err)
		}
		index := strings.LastIndex(string(body), `,"moved_today":`)
		if index < 0 {
			t.Fatal("missing additive member")
		}
		legacy := string(body[:index]) + "}"
		want := responseLegacyEmptyJSON
		if known {
			want = strings.Replace(want, `"report":{"known":false}`, `"report":{"known":true,"pass_at":{"ms":0,"known":false},"ok_at":{"ms":0,"known":false},"error_at":{"ms":0,"known":false},"next_try_at":{"ms":0,"known":false},"watched":0,"waiting":0,"skipped":0,"calls_today":0,"last_error":"","not_watched":[]}`, 1)
		}
		if legacy != want {
			t.Fatalf("historical root changed known%v: %s", known, legacy)
		}
		if got := movedBody(t, body); got.Day != "2031-04-06" || got.Start.MS != 1933200000000 || !got.Start.Known || len(got.IDs) != 0 {
			t.Fatalf("empty complete overview: %+v", got)
		}
	}
}

func TestNavAsksMovedMembershipOrderScopeAndLocalDay(t *testing.T) {
	now := time.Date(2031, 4, 5, 21, 0, 0, 0, time.FixedZone("fixture-west", -5*3600))
	const start int64 = 1933131600 // 2031-04-05T05:00:00Z, distinct from UTC usage day.
	for _, mode := range []string{"all", "lexical", "alias"} {
		t.Run(mode, func(t *testing.T) {
			s := responseSnapshot()
			s.UTCDay = "2031-04-06"
			s.Items = []state.Ask{movedAsk(7, state.AskClosed, "rule", start), movedAsk(9007199254740992, state.AskProbablyDone, "rule", start+3600),
				movedAsk(6, state.AskClosed, "rule", start-1), movedAsk(9007199254740993, state.AskClosed, "rule", start+3600),
				movedAsk(5, state.AskClosed, state.AskPerson, now.Unix()), movedAsk(4, state.AskDismissed, "rule", now.Unix()),
				movedAsk(3, state.AskOpen, "rule", now.Unix()), movedAsk(8, state.AskProbablyDone, "rule", now.Unix()+3600)}
			for i := range s.Items {
				s.Items[i].Workspace = "/invented/project/sub"
			}
			s.Items[7].Workspace = "/outside"
			o := responseOptions()
			o.now = now
			want := []string{"8", "9007199254740993", "9007199254740992", "7"}
			if mode == "lexical" {
				o.scope = navAsksScope{"directory", "/invented/project/"}
				o.aliases = map[string]string{o.scope.Dir: o.scope.Dir, "/outside": "/outside"}
				want = want[1:]
			}
			if mode == "alias" {
				o.scope = navAsksScope{"directory", "/link/"}
				o.aliases = map[string]string{"/link/": "/invented/project", "/invented/project/sub": "/invented/project/sub", "/outside": "/outside"}
				want = want[1:]
			}
			for _, known := range []bool{false, true} {
				s.ReportPresent = known
				body, err := buildNavAsksResponse(context.Background(), s, o)
				if err != nil {
					t.Fatal(err)
				}
				got := movedBody(t, body)
				if got.Day != "2031-04-05" || got.Start.MS != start*1000 || !got.Start.Known || !reflect.DeepEqual(got.IDs, want) {
					t.Fatalf("membership/local day: %+v want%v", got, want)
				}
				if responseJSON(t, body)["totals"].(map[string]any)["utc_day"] != "2031-04-06" {
					t.Fatal("local day replaced UTC counters day")
				}
			}
		})
	}
}

func TestNavAsksMovedMidnightLocationsAndSafeStart(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	brazil, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name       string
		now, start time.Time
		day        string
	}{
		{"epoch", time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC(), "1970-01-01"},
		{"negative", time.Unix(-1, 0).UTC(), time.Unix(-86400, 0).UTC(), "1969-12-31"},
		{"east", time.Date(2031, 4, 6, 1, 0, 0, 0, time.FixedZone("fixture-east", 14*3600)), time.Date(2031, 4, 5, 10, 0, 0, 0, time.UTC), "2031-04-06"},
		{"DST", time.Date(2026, 3, 8, 12, 0, 0, 0, ny), time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), "2026-03-08"},
		{"normalized midnight", time.Date(2018, 11, 4, 12, 0, 0, 0, brazil), time.Date(2018, 11, 4, 2, 0, 0, 0, time.UTC), "2018-11-04"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := responseOptions()
			o.now = c.now
			s := responseSnapshot()
			s.UTCDay = state.AsksDay(c.now)
			s.Items = []state.Ask{movedAsk(1, state.AskClosed, "rule", c.start.Unix()-1), movedAsk(2, state.AskClosed, "rule", c.start.Unix()), movedAsk(3, state.AskProbablyDone, "rule", c.start.Unix()+1)}
			body, err := buildNavAsksResponse(context.Background(), s, o)
			if err != nil {
				t.Fatal(err)
			}
			got := movedBody(t, body)
			if got.Start.MS != c.start.UnixMilli() || got.Day != c.day || !reflect.DeepEqual(got.IDs, []string{"3", "2"}) {
				t.Fatalf("independent midnight vector: %+v expected%d %s", got, c.start.UnixMilli(), c.day)
			}
		})
	}
	o := responseOptions()
	o.now = time.Unix(-navConversationSafeInteger/1000, 0).UTC()
	s := responseSnapshot()
	s.UTCDay = state.AsksDay(o.now)
	calls := 0
	render := func(string, int, bool) ([]webMarkdownLine, bool) { calls++; return nil, true }
	if b, e := buildNavAsksResponseWith(context.Background(), s, o, render, json.Marshal); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) || calls != 0 {
		t.Fatalf("safe now/unsafe computed midnight: %v calls%d", e, calls)
	}
}

func TestNavAsksMovedReferenceSharedLimitsBeforeRender(t *testing.T) {
	o := responseOptions()
	s := responseSnapshot()
	s.Items = []state.Ask{movedAsk(1, state.AskClosed, "", o.now.Unix())}
	// Header100 + closed-row metadata7 + reference1 + actual wording13 =121 units.
	o.rows, o.inputBytes, o.renderCells, o.arrayEntries, o.stringUnits = 1, 20, 35, 9, 121
	body, err := buildNavAsksResponse(context.Background(), s, o)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(movedBody(t, body).IDs, []string{"1"}) {
		t.Fatal("single reference")
	}
	for _, key := range []string{"raw", "work", "entries", "units", "body"} {
		bad := o
		switch key {
		case "raw":
			bad.inputBytes--
		case "work":
			bad.renderCells--
		case "entries":
			bad.arrayEntries--
		case "units":
			bad.stringUnits--
		case "body":
			bad.bodyBytes = int64(len(body)) - 1
		}
		marshals := 0
		marshal := func(v any) ([]byte, error) { marshals++; return json.Marshal(v) }
		if b, e := buildNavAsksResponseWith(context.Background(), s, bad, webMarkdownLinesChecked, marshal); b != nil || e != errNavAsksResponseTooLarge || marshals != 0 {
			t.Fatalf("one-short%s: %v marshal%d", key, e, marshals)
		}
	}
	for _, credit := range []string{"reference entry", "reference unit"} {
		bad := o
		if credit == "reference entry" {
			bad.arrayEntries = 6
		} else {
			bad.stringUnits = 81
		}
		renders, marshals := 0, 0
		render := func(string, int, bool) ([]webMarkdownLine, bool) { renders++; return nil, true }
		marshal := func(v any) ([]byte, error) { marshals++; return json.Marshal(v) }
		if b, e := buildNavAsksResponseWith(context.Background(), s, bad, render, marshal); b != nil || e != errNavAsksResponseTooLarge || renders != 0 || marshals != 0 {
			t.Fatalf("metadata admission%s: %v render%d marshal%d", credit, e, renders, marshals)
		}
	}
}
