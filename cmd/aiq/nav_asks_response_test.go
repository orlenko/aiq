package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/diagnostic"
	"github.com/orlenko/aiq/internal/state"
)

// Invented allowances only; the implementation has no production defaults.
func responseOptions() navAsksResponseOptions {
	return navAsksResponseOptions{scope: navAsksScope{"all", ""}, now: time.Date(2031, 4, 6, 0, 0, 0, 0, time.UTC), width: 1,
		rows: 100, inputBytes: 1 << 20, renderCells: 1 << 30, arrayEntries: 10000, stringUnits: 20000, bodyBytes: 1 << 20}
}
func responseSnapshot() state.AsksSnapshot {
	return state.AsksSnapshot{Items: []state.Ask{}, UTCDay: "2031-04-06"}
}
func responseRows() state.AsksSnapshot {
	s := responseSnapshot()
	s.Items = []state.Ask{{ID: 1, State: state.AskOpen, FirstQuote: "z", Refs: []string{}}, {ID: 2, State: state.AskOpen, FirstQuote: "z", Refs: []string{}}}
	return s
}

const responseEmptyJSON = `{"scope":{"mode":"all","dir":""},"sections":[{"key":"asks","items":[]},{"key":"warnings","items":[]},{"key":"other_open","items":[]},{"key":"probably_done","items":[]},{"key":"closed_dismissed","items":[]}],"report":{"known":false},"totals":{"utc_day":"2031-04-06","skipped":0,"calls_today":0}}`

func responseJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func responseItems(v map[string]any, group int) []any {
	return v["sections"].([]any)[group].(map[string]any)["items"].([]any)
}

func TestNavAsksResponseLiteralQuotasAndRoots(t *testing.T) {
	o := responseOptions()
	o.rows, o.inputBytes, o.renderCells, o.arrayEntries, o.stringUnits, o.bodyBytes = 1, 13, 1, 5, 64, int64(len(responseEmptyJSON))
	for _, present := range []bool{false, true} {
		s := responseSnapshot()
		s.ReportPresent = present
		want := responseEmptyJSON
		if present {
			want = strings.Replace(want, `"report":{"known":false}`, `"report":{"known":true,"pass_at":{"ms":0,"known":false},"ok_at":{"ms":0,"known":false},"error_at":{"ms":0,"known":false},"next_try_at":{"ms":0,"known":false},"watched":0,"waiting":0,"skipped":0,"calls_today":0,"last_error":"","not_watched":[]}`, 1)
		}
		o.bodyBytes = int64(len(want))
		body, err := buildNavAsksResponse(context.Background(), s, o)
		if err != nil || string(body) != want {
			t.Fatalf("literal root present%v: %s %v", present, body, err)
		}
		for _, mutate := range []func(*navAsksResponseOptions){func(o *navAsksResponseOptions) { o.inputBytes-- }, func(o *navAsksResponseOptions) { o.arrayEntries-- }, func(o *navAsksResponseOptions) { o.stringUnits-- }, func(o *navAsksResponseOptions) { o.bodyBytes-- }} {
			bad := o
			mutate(&bad)
			calls := 0
			marshal := func(v any) ([]byte, error) { calls++; return json.Marshal(v) }
			if b, e := buildNavAsksResponseWith(context.Background(), s, bad, webMarkdownLinesChecked, marshal); b != nil || e != errNavAsksResponseTooLarge || calls != 0 {
				t.Fatalf("short header/body: %v marshal%d", e, calls)
			}
		}
	}
	ignored := responseSnapshot()
	ignored.Status.LastError, ignored.Status.NotWatched, ignored.Status.PassAt = "\xff", []string{"\xff"}, math.MaxInt64
	o.bodyBytes = int64(len(responseEmptyJSON))
	if b, e := buildNavAsksResponse(context.Background(), ignored, o); e != nil || string(b) != responseEmptyJSON {
		t.Fatal("absent watcher fields were consumed", e)
	}
	s := responseRows()
	o = responseOptions()
	o.rows, o.inputBytes, o.renderCells, o.arrayEntries, o.stringUnits = 2, 23, 70, 11, 100
	body, err := buildNavAsksResponse(context.Background(), s, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range responseItems(responseJSON(t, body), 2) {
		r := entry.(map[string]any)
		if r["first"].(map[string]any)["copy"] != "> z\n\n" || r["latest"].(map[string]any)["copy"] != ">\n\n" {
			t.Fatal("literal copy")
		}
	}
	for i, mutate := range []func(*navAsksResponseOptions){func(o *navAsksResponseOptions) { o.inputBytes-- }, func(o *navAsksResponseOptions) { o.renderCells-- }, func(o *navAsksResponseOptions) { o.arrayEntries-- }, func(o *navAsksResponseOptions) { o.stringUnits-- }} {
		bad := o
		mutate(&bad)
		calls := 0
		render := func(s string, w int, m bool) ([]webMarkdownLine, bool) {
			calls++
			return webMarkdownLinesChecked(s, w, m)
		}
		if b, e := buildNavAsksResponseWith(context.Background(), s, bad, render, json.Marshal); b != nil || e != errNavAsksResponseTooLarge || i < 2 && calls != 0 {
			t.Fatalf("short shared credits%d: %v calls%d", i, e, calls)
		}
	}
	// Each draft pair costs24 units/5 entries; retained output costs only13/2.
	tail := func(string, int, bool) ([]webMarkdownLine, bool) {
		return []webMarkdownLine{{kind: "text", spans: []webMarkdownSpan{{text: "z"}}}, {kind: "code", spans: []webMarkdownSpan{{text: "   ", classes: []string{"code"}}}}}, true
	}
	o.arrayEntries, o.stringUnits = 17, 122
	if _, e := buildNavAsksResponseWith(context.Background(), s, o, tail, json.Marshal); e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*navAsksResponseOptions){func(o *navAsksResponseOptions) { o.arrayEntries = 16 }, func(o *navAsksResponseOptions) { o.stringUnits = 121 }} {
		bad := o
		mutate(&bad)
		if b, e := buildNavAsksResponseWith(context.Background(), s, bad, tail, json.Marshal); b != nil || e != errNavAsksResponseTooLarge {
			t.Fatal("discarded tails escaped shared credits", e)
		}
	}
}

func TestNavAsksResponseValidationAndScope(t *testing.T) {
	for _, mutate := range []func(*state.AsksSnapshot){
		func(s *state.AsksSnapshot) { s.Items[1].ID = 1 }, func(s *state.AsksSnapshot) { s.Items[0].ID = 0 },
		func(s *state.AsksSnapshot) { s.Items[0].State = "other" }, func(s *state.AsksSnapshot) { s.Items[0].StateText = "\xff" },
		func(s *state.AsksSnapshot) { s.Items[0].Refs = []string{"\xff"} }, func(s *state.AsksSnapshot) { s.Items[0].RaisedAt = math.MinInt64 },
		func(s *state.AsksSnapshot) { s.Items[0].StateAt = navConversationSafeInteger/1000 + 1 }, func(s *state.AsksSnapshot) { s.Items[0].Risk = math.NaN() },
		func(s *state.AsksSnapshot) { s.Skipped = 3 }, func(s *state.AsksSnapshot) { s.UTCDay = "2031-04-05" },
		func(s *state.AsksSnapshot) { s.Items[0].Repeats = -1 },
		func(s *state.AsksSnapshot) {
			s.CallsToday = navConversationSafeInteger + 1
			s.Status.CallsToday = int(s.CallsToday)
		},
	} {
		s := responseRows()
		mutate(&s)
		calls := 0
		r := func(string, int, bool) ([]webMarkdownLine, bool) { calls++; return nil, true }
		if b, e := buildNavAsksResponseWith(context.Background(), s, responseOptions(), r, json.Marshal); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) || calls != 0 {
			t.Fatalf("invalid facts published/rendered: %v calls%d", e, calls)
		}
	}
	s := responseRows()
	s.Items = s.Items[:1]
	s.Items[0].Workspace = "/physical/sub"
	o := responseOptions()
	o.scope = navAsksScope{"directory", "/link/"}
	for _, aliases := range []map[string]string{nil, {"/link/": "/physical"}, {"/link/": "", "/physical/sub": ""}, {"/link/": "/physical", "/physical/sub": "/physical/sub", "/extra": "/extra"}} {
		o.aliases = aliases
		if b, e := buildNavAsksResponse(context.Background(), s, o); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) {
			t.Fatal("bad alias facts", e)
		}
	}
	o.aliases = map[string]string{"/link/": "/physical", "/physical/sub": "/physical/sub"}
	b, e := buildNavAsksResponse(context.Background(), s, o)
	if e != nil || responseJSON(t, b)["scope"].(map[string]any)["dir"] != "/link/" || len(responseItems(responseJSON(t, b), 2)) != 1 {
		t.Fatal("alias match/scope spelling", e)
	}
	o.aliases = map[string]string{"/link/": "/link/", "/physical/sub": "/physical/sub"}
	b, e = buildNavAsksResponse(context.Background(), s, o)
	if e != nil || len(responseItems(responseJSON(t, b), 2)) != 0 {
		t.Fatal("two unresolved paths falsely matched", e)
	}
	s.Items[0].Workspace, s.Items[0].StateText = "/outside", "\xff"
	o.aliases = map[string]string{"/link/": "/link/", "/outside": "/outside"}
	if b, e := buildNavAsksResponse(context.Background(), s, o); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) {
		t.Fatal("invalid excluded metadata survived", e)
	}
	s.Items[0].Workspace = "/link/sub"
	s.Items[0].StateText = ""
	o.aliases = nil
	s.Items[0].FirstQuote = "a\xff"
	b, e = buildNavAsksResponse(context.Background(), s, o)
	if e != nil || responseItems(responseJSON(t, b), 2)[0].(map[string]any)["first"].(map[string]any)["copy"] != "> a�\n\n" {
		t.Fatal("legacy quote replacement changed", e)
	}
	for _, mutate := range []func(*navAsksResponseOptions){func(o *navAsksResponseOptions) { o.width = 0 }, func(o *navAsksResponseOptions) { o.rows = 0 }, func(o *navAsksResponseOptions) { o.inputBytes = 0 }, func(o *navAsksResponseOptions) { o.renderCells = 0 }, func(o *navAsksResponseOptions) { o.arrayEntries = 0 }, func(o *navAsksResponseOptions) { o.stringUnits = 0 }, func(o *navAsksResponseOptions) { o.bodyBytes = 0 }, func(o *navAsksResponseOptions) { o.scope = navAsksScope{"all", "/unexpected"} }} {
		bad := responseOptions()
		mutate(&bad)
		if b, e := buildNavAsksResponse(context.Background(), responseSnapshot(), bad); b != nil || e != errNavAsksResponseInvalid {
			t.Fatal("invalid config", e)
		}
	}
}

func TestNavAsksResponseTimeAndClassification(t *testing.T) {
	for _, tc := range []struct {
		risk, reader float64
		age          int64
		key          string
	}{{.8, .59, 0, "warnings"}, {.799, .59, 0, "other_open"}, {.8, .6, 172800, "asks"}, {0, .6, 172801, "other_open"}, {0, .6, -1, "asks"}} {
		o := responseOptions()
		s := responseSnapshot()
		s.Items = []state.Ask{{ID: 1, State: state.AskOpen, RaisedAt: o.now.Unix() - tc.age, Risk: tc.risk, AsksReader: tc.reader}}
		b, e := buildNavAsksResponse(context.Background(), s, o)
		if e != nil {
			t.Fatal(e)
		}
		total, actualKey := 0, ""
		for _, entry := range responseJSON(t, b)["sections"].([]any) {
			g := entry.(map[string]any)
			n := len(g["items"].([]any))
			total += n
			if n > 0 {
				actualKey = g["key"].(string)
			}
		}
		if total != 1 || actualKey != tc.key {
			t.Fatalf("literal threshold %+v: rows%d key%s", tc, total, actualKey)
		}
	}
	o := responseOptions()
	s := responseRows()
	s.Items = s.Items[:1]
	s.Items[0].RaisedAt = 0
	s.Items[0].StateAt = -62135596800
	s.ReportPresent = true
	s.Status.ErrorAt = -1
	s.Status.NextTryAt = navConversationSafeInteger / 1000
	b, e := buildNavAsksResponse(context.Background(), s, o)
	if e != nil {
		t.Fatal(e)
	}
	v := responseJSON(t, b)
	item := responseItems(v, 2)[0].(map[string]any)
	if !reflect.DeepEqual(item["raised_at"], map[string]any{"ms": float64(0), "known": true}) || !reflect.DeepEqual(item["state_at"], map[string]any{"ms": float64(-62135596800000), "known": true}) {
		t.Fatal("epoch/year-one lost", item)
	}
	report := v["report"].(map[string]any)
	if !reflect.DeepEqual(report["pass_at"], map[string]any{"ms": float64(0), "known": false}) || !reflect.DeepEqual(report["error_at"], map[string]any{"ms": float64(-1000), "known": true}) || report["next_try_at"].(map[string]any)["ms"] != float64(9007199254740000) {
		t.Fatal("report clocks", report)
	}
	o.now = time.Unix(1<<62, 0)
	if b, e := buildNavAsksResponse(context.Background(), s, o); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) {
		t.Fatal("unsafe now", e)
	}
}

func TestNavAsksResponseOwnedCaptureAndConcurrency(t *testing.T) {
	s := responseRows()
	s.Items = s.Items[:1]
	s.Items[0].Workspace = "/physical/sub"
	s.Items[0].Refs = []string{"ref"}
	s.ReportPresent = true
	s.Status.NotWatched = []string{"cold"}
	o := responseOptions()
	o.scope = navAsksScope{"directory", "/link/"}
	o.aliases = map[string]string{"/link/": "/physical", "/physical/sub": "/physical/sub"}
	render := func(text string, w int, m bool) ([]webMarkdownLine, bool) {
		s.Items[0].StateText = "changed"
		s.Items[0].FirstQuote = "changed"
		s.Items[0].Refs[0] = "changed"
		s.Status.NotWatched[0] = "changed"
		o.aliases["/link/"] = "/changed"
		return webMarkdownLinesChecked(text, w, m)
	}
	b, e := buildNavAsksResponseWith(context.Background(), s, o, render, json.Marshal)
	if e != nil {
		t.Fatal(e)
	}
	v := responseJSON(t, b)
	item := responseItems(v, 2)[0].(map[string]any)
	if item["state_text"] != "" || item["first"].(map[string]any)["copy"] != "> z\n\n" || item["refs"].([]any)[0] != "ref" || v["report"].(map[string]any)["not_watched"].([]any)[0] != "cold" {
		t.Fatal("callback changed captured graph")
	}
	s = responseRows()
	o = responseOptions()
	o.rows, o.inputBytes, o.renderCells, o.arrayEntries, o.stringUnits = 2, 23, 70, 11, 100
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, e := buildNavAsksResponse(context.Background(), s, o); e != nil || len(b) == 0 {
				t.Errorf("credits shared across requests: %v", e)
			}
		}()
	}
	wg.Wait()
}

func TestNavAsksResponseFailureOriginsAndFinalCancellation(t *testing.T) {
	for _, prior := range []string{"none", "known", "unknown"} {
		var seed diagnostic.Origin
		if prior != "none" {
			func() {
				defer func() { seed = diagnostic.Capture(recover()) }()
				if prior == "unknown" {
					regexp.MustCompile("[")
				}
				panic("invented prior")
			}()
		}
		faults := &diagnostic.Latch{}
		faults.Note(seed)
		o := responseOptions()
		o.faults = faults
		ctx, cancel := context.WithCancel(context.Background())
		methods, calls := 0, 0
		render := func(string, int, bool) ([]webMarkdownLine, bool) { cancel(); panic(asksWordHostile{&methods}) }
		marshal := func(any) ([]byte, error) { calls++; panic("must not reach encoding") }
		b, e := buildNavAsksResponseWith(ctx, responseRows(), o, render, marshal)
		cancel()
		origin := diagnostic.FromError(e)
		if b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) || !origin.Present() || methods != 0 || calls != 0 || seed.Present() && origin != seed {
			t.Fatalf("inner origin/rebase: %v methods%d calls%d", e, methods, calls)
		}
		if !seed.Present() {
			if file, _, ok := origin.Location(); !ok || file != "cmd/aiq/nav_asks_response_test.go" {
				t.Fatal("inner literal origin lost")
			}
		}
		// A later outer panic preserves an already latched first-known/unknown.
		if seed.Present() {
			marshal = func(any) ([]byte, error) { panic("invented later fault") }
			b, e = buildNavAsksResponseWith(context.Background(), responseRows(), o, webMarkdownLinesChecked, marshal)
			if b != nil || diagnostic.FromError(e) != seed {
				t.Fatal("outer replaced first origin")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marshal := func(v any) ([]byte, error) { b, e := json.Marshal(v); cancel(); return b, e }
	if b, e := buildNavAsksResponseWith(ctx, responseSnapshot(), responseOptions(), webMarkdownLinesChecked, marshal); b != nil || e != context.Canceled {
		t.Fatal("published after encoding cancellation", e)
	}
	ctx, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	render := func(string, int, bool) ([]webMarkdownLine, bool) { cancel2(); return nil, false }
	if b, e := buildNavAsksResponseWith(ctx, responseRows(), responseOptions(), render, json.Marshal); b != nil || e != context.Canceled {
		t.Fatal("ordinary cancellation lost", e)
	}
	methods := 0
	marshal = func(any) ([]byte, error) { return nil, asksWordHostile{&methods} }
	if b, e := buildNavAsksResponseWith(context.Background(), responseSnapshot(), responseOptions(), webMarkdownLinesChecked, marshal); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) || methods != 0 {
		t.Fatal("hostile marshal error inspected", e)
	}
	marshal = func(any) ([]byte, error) { return []byte(`{}`), nil }
	if b, e := buildNavAsksResponseWith(context.Background(), responseSnapshot(), responseOptions(), webMarkdownLinesChecked, marshal); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) {
		t.Fatal("wrong encoding length", e)
	}
	renders, encodes := 0, 0
	render = func(text string, width int, markup bool) ([]webMarkdownLine, bool) {
		renders++
		if renders == 2 {
			return []webMarkdownLine{{kind: "unsupported"}}, true
		}
		return webMarkdownLinesChecked(text, width, markup)
	}
	marshal = func(v any) ([]byte, error) { encodes++; return json.Marshal(v) }
	if b, e := buildNavAsksResponseWith(context.Background(), responseRows(), responseOptions(), render, marshal); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) || renders != 2 || encodes != 0 {
		t.Fatal("late pair failure published/encoded prefix", e)
	}
}

func TestNavAsksResponseRealSQLiteToConsumer(t *testing.T) {
	node, e := exec.LookPath("node")
	if e != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(e)
		}
		t.Skip("node unavailable: real consumer NOT RUN")
	}
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent report", true: "present report"}[known], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture.db")
			s, e := state.Open(path)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			if e = s.EnsureAsks(); e != nil {
				t.Fatal(e)
			}
			// A separate setup connection owns invented schema/data writes.
			setup, e := sql.Open("sqlite", "file:"+path)
			if e != nil {
				t.Fatal(e)
			}
			defer setup.Close()
			for _, r := range []struct {
				id, at       int64
				st           string
				risk, reader float64
				provider     string
			}{{9007199254740993, 827200, "open", .8, .6, "agy"}, {2, 1000001, "open", 0, .6, "agy"}, {3, 827199, "open", .8, .59, "unsupported"}, {4, 827199, "open", 0, .6, "agy"}, {5, 999999, "probably-done", 0, 0, "agy"}, {6, 999999, "closed", 0, 0, "agy"}, {7, 999999, "dismissed", 0, 0, "agy"}} {
				if _, e := setup.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,asks_reader,risk,refs) VALUES(?,?,?,?,?,?,?,0,?,?,?, ?,?,?)`, r.id, r.provider+":"+r.st, "/fixture", r.provider, "ABCDEF01-0000-4000-8000-000000000001", r.at, r.st, "first **word**", "latest\n\ntext", r.at, r.reader, r.risk, `["r","r",""]`); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := setup.Exec(`INSERT INTO asks_sessions(provider,session_id,skipped) VALUES('agy','fixture',3); INSERT INTO asks_usage(day,calls) VALUES('1970-01-12',7)`); e != nil {
				t.Fatal(e)
			}
			if known {
				if _, e := setup.Exec(`INSERT INTO asks_watch(id,error_at,last_error,not_watched) VALUES(1,-1,'stored error','["cold"]')`); e != nil {
					t.Fatal(e)
				}
			}
			ro, e := state.OpenReadOnly(path)
			if e != nil {
				t.Fatal(e)
			}
			defer ro.Close()
			o := responseOptions()
			o.now = time.Unix(1000000, 0)
			raw, e := ro.ReadAsksSnapshot(context.Background(), o.now, state.AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 20})
			if e != nil {
				t.Fatal(e)
			}
			b, e := buildNavAsksResponse(context.Background(), *raw, o)
			if e != nil {
				t.Fatal(e)
			}
			module, e := filepath.Abs("../../internal/daemon/ui/asks_data.js")
			if e != nil {
				t.Fatal(e)
			}
			script := `import {readFileSync} from 'node:fs';import assert from 'node:assert/strict';const {captureAsks}=await import(process.argv[1]);const raw=JSON.parse(readFileSync(0,'utf8'));const out=captureAsks(raw,{mode:'all',dir:''},{rows:10,arrayEntries:10000,stringUnits:20000});
assert.deepEqual(out.sections.map(g=>g.items.map(r=>r.id)),[['2','9007199254740993'],['3'],['4'],['5'],['7','6']]);assert.deepEqual(out.totals,{utc_day:'1970-01-12',skipped:3,calls_today:7});
for(const r of out.sections.flatMap(g=>g.items)){assert.equal(r.first.copy,'> first **word**\n\n');assert.equal(r.latest.copy,'> latest\n>\n> text\n\n');assert.deepEqual(r.refs,['r','r','']);assert.deepEqual(r.state_at,{ms:0,known:true});}assert.equal(out.sections[0].items[1].session_id,'ABCDEF01-0000-4000-8000-000000000001');assert.equal(out.sections[0].items[1].selectable,true);assert.equal(out.sections[1].items[0].selectable,false);
if(raw.report.known){assert.equal(out.report.known,true);assert.deepEqual(out.report.pass_at,{ms:0,known:false});assert.deepEqual(out.report.error_at,{ms:-1000,known:true});assert.equal(out.report.skipped,3);assert.equal(out.report.calls_today,7);}else{assert.deepEqual(raw.report,{known:false});assert.deepEqual(out.report,{known:false});}console.log('actual reader response capture OK');`
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, (&url.URL{Scheme: "file", Path: module}).String())
			cmd.WaitDelay = 2 * time.Second
			cmd.Stdin = strings.NewReader(string(b))
			out, e := cmd.CombinedOutput()
			if e != nil || string(out) != "actual reader response capture OK\n" {
				t.Fatalf("consumer: %s %v", out, e)
			}
			if _, e := setup.Exec(`UPDATE asks SET state_text=? WHERE id=2`, "\xff"); e != nil {
				t.Fatal(e)
			}
			bad, e := ro.ReadAsksSnapshot(context.Background(), o.now, state.AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 20})
			if e != nil {
				t.Fatal(e)
			}
			o.scope = navAsksScope{"directory", "/other"}
			o.aliases = map[string]string{"/other": "/other", "/fixture": "/fixture"}
			if b, e := buildNavAsksResponse(context.Background(), *bad, o); b != nil || !errors.Is(e, diagnostic.Base(diagnostic.Unshowable)) {
				t.Fatal("real-reader excluded invalid metadata silently repaired/dropped", e)
			}
		})
	}
}
