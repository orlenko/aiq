package main

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestNavAsksHTTPRealSQLiteMidnightToConsumer(t *testing.T) {
	node, e := exec.LookPath("node")
	if e != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(e)
		}
		t.Skip("node unavailable: HTTP real consumer NOT RUN")
	}
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent report", true: "present report"}[known], func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "fixture.db")
			store, e := state.Open(p)
			if e != nil {
				t.Fatal(e)
			}
			defer store.Close()
			if e = store.EnsureAsks(); e != nil {
				t.Fatal(e)
			}
			setup, e := sql.Open("sqlite", "file:"+p)
			if e != nil {
				t.Fatal(e)
			}
			defer setup.Close()
			captured := time.Date(2031, 4, 6, 23, 59, 59, 0, time.UTC)
			if _, e = setup.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,asks_reader,risk,refs) VALUES(9007199254740993,'fixture','/fixture','agy','ABCDEF01-0000-4000-8000-000000000001',?,'open',0,?,?,?,0.6,0,'["r","r",""]');`, captured.Unix(), "first **word**", "latest\n\ntext", captured.Unix()); e != nil {
				t.Fatal(e)
			}
			if _, e = setup.Exec(`INSERT INTO asks_sessions(provider,session_id,skipped) VALUES('agy','fixture',3); INSERT INTO asks_usage(day,calls) VALUES('2031-04-06',7); INSERT INTO asks_usage(day,calls) VALUES('2031-04-07',99);`); e != nil {
				t.Fatal(e)
			}
			if known {
				if _, e = setup.Exec(`INSERT INTO asks_watch(id,last_error,not_watched) VALUES(1,'stored fixture','[]')`); e != nil {
					t.Fatal(e)
				}
			}
			ro, e := state.OpenReadOnly(p)
			if e != nil {
				t.Fatal(e)
			}
			defer ro.Close()
			now, clocks, reads := captured, 0, 0
			req := asksHTTPGet("/api/nav/asks")
			h, e := newNavAsksHandler(navAsksHTTPDeps{now: func() time.Time { clocks++; v := now; now = now.Add(2 * time.Second); return v }, read: func(ctx context.Context, at time.Time, l state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
				reads++
				if ctx != req.Context() || !at.Equal(captured) {
					t.Fatal("reader lost exact captured time/context")
				}
				return ro.ReadAsksSnapshot(ctx, at, l)
			}, resolve: func(context.Context, string) (navAsksAlias, error) {
				t.Fatal("all mode resolved a path")
				return navAsksAlias{}, nil
			}}, navAsksHTTPLimits{response: responseOptions(), reader: state.AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 20}, requests: 1})
			if e != nil {
				t.Fatal(e)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != 200 || clocks != 1 || reads != 1 || now.Day() != 7 {
				t.Fatalf("midnight HTTP status%d clocks%d reads%d", w.Code, clocks, reads)
			}
			module, e := filepath.Abs("../../internal/daemon/ui/asks_data.js")
			if e != nil {
				t.Fatal(e)
			}
			script := `import {readFileSync} from 'node:fs';import assert from 'node:assert/strict';const {captureAsks}=await import(process.argv[1]);const raw=JSON.parse(readFileSync(0,'utf8'));const out=captureAsks(raw,{mode:'all',dir:''},{rows:10,arrayEntries:10000,stringUnits:20000});
assert.deepEqual(out.sections.map(s=>s.key),['asks','warnings','other_open','probably_done','closed_dismissed']);assert.deepEqual(out.totals,{utc_day:'2031-04-06',skipped:3,calls_today:7});const r=out.sections[0].items[0];assert.equal(r.id,'9007199254740993');assert.equal(r.session_id,'ABCDEF01-0000-4000-8000-000000000001');assert.equal(r.selectable,true);assert.deepEqual(r.refs,['r','r','']);assert.equal(r.first.copy,'> first **word**\n\n');assert.equal(r.latest.copy,'> latest\n>\n> text\n\n');assert.deepEqual(r.first.lines,[{kind:'text',table_rows:0,spans:[{c:[],d:false,t:'first '},{c:['b'],d:false,t:'word'}]}]);assert.deepEqual(r.latest.lines,[{kind:'text',table_rows:0,spans:[{c:[],d:false,t:'latest'}]},{kind:'text',table_rows:0,spans:[]},{kind:'text',table_rows:0,spans:[{c:[],d:false,t:'text'}]}]);assert.deepEqual(r.state_at,{ms:0,known:true});assert.equal(out.report.known,raw.report.known);if(out.report.known){assert.equal(out.report.skipped,3);assert.equal(out.report.calls_today,7);}else{assert.deepEqual(out.report,{known:false});}console.log('HTTP actual reader capture OK');`
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, (&url.URL{Scheme: "file", Path: module}).String())
			cmd.WaitDelay = 2 * time.Second
			cmd.Stdin = strings.NewReader(w.Body.String())
			out, e := cmd.CombinedOutput()
			if e != nil || string(out) != "HTTP actual reader capture OK\n" {
				t.Fatalf("real consumer: %s %v", out, e)
			}
		})
	}
}
