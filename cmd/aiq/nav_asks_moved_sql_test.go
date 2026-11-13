package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestNavAsksMovedSQLiteHTTPActualCapture(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(err)
		}
		t.Skip("node unavailable: moved HTTP actual capture NOT RUN")
	}
	for _, known := range []bool{false, true} {
		for _, directory := range []bool{false, true} {
			name := "unknown/all"
			if known {
				name = "known/all"
			}
			if directory {
				name += "/alias"
			}
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "fixture.db")
				store, err := state.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				if err = store.EnsureAsks(); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", "file:"+path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				captured := time.Date(2031, 4, 5, 21, 0, 0, 0, time.FixedZone("fixture-west", -5*3600))
				const start int64 = 1933131600
				rows := []state.Ask{movedAsk(7, state.AskClosed, "rule", start), movedAsk(9007199254740992, state.AskProbablyDone, "rule", start+3600),
					movedAsk(6, state.AskClosed, "rule", start-1), movedAsk(9007199254740993, state.AskClosed, "rule", start+3600),
					movedAsk(5, state.AskClosed, state.AskPerson, captured.Unix()), movedAsk(4, state.AskDismissed, "rule", captured.Unix()),
					movedAsk(3, state.AskOpen, "rule", captured.Unix()), movedAsk(8, state.AskProbablyDone, "rule", captured.Unix()+3600)}
				for _, row := range rows {
					ws := "/fixture/project/sub"
					if row.ID == 8 {
						ws = "/outside"
					}
					_, err = db.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,state_reason,first_quote,last_quote,last_seen,refs) VALUES(?,?,?,'agy','ABCDEF01-0000-4000-8000-000000000001',?,?,?,?,?,?,?,'[]')`, row.ID, "fixture-"+strconv.FormatInt(row.ID, 10), ws, captured.Unix(), row.State, row.StateAt, row.StateReason, "first **word**", "latest", captured.Unix())
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err = db.Exec(`INSERT INTO asks_sessions(provider,session_id,skipped) VALUES('agy','fixture',3); INSERT INTO asks_usage(day,calls) VALUES('2031-04-06',7); INSERT INTO asks_usage(day,calls) VALUES('2031-04-07',99)`); err != nil {
					t.Fatal(err)
				}
				if known {
					if _, err = db.Exec(`INSERT INTO asks_watch(id,last_error,not_watched) VALUES(1,'stored fixture','[]')`); err != nil {
						t.Fatal(err)
					}
				}
				ro, err := state.OpenReadOnly(path)
				if err != nil {
					t.Fatal(err)
				}
				defer ro.Close()
				clocks, reads := 0, 0
				now := captured
				resolves := map[string]int{}
				target := "/api/nav/asks"
				scope := navAsksScope{"all", ""}
				want := []string{"8", "9007199254740993", "9007199254740992", "7"}
				if directory {
					target += "?dir=%2Flink%2F"
					scope = navAsksScope{"directory", "/link/"}
					want = want[1:]
				}
				request := asksHTTPGet(target)
				handler, err := newNavAsksHandler(navAsksHTTPDeps{
					now: func() time.Time { clocks++; at := now; now = now.Add(26 * time.Hour); return at },
					read: func(ctx context.Context, at time.Time, l state.AsksSnapshotLimits) (*state.AsksSnapshot, error) {
						reads++
						if ctx != request.Context() || !at.Equal(captured) {
							t.Fatal("lost exact captured now/context")
						}
						return ro.ReadAsksSnapshot(ctx, at, l)
					},
					resolve: func(_ context.Context, path string) (navAsksAlias, error) {
						if !directory {
							t.Fatal("all mode resolved")
						}
						resolves[path]++
						switch path {
						case "/link/":
							return navAsksAlias{path: "/fixture/project", resolved: true}, nil
						case "/fixture/project/sub":
							return navAsksAlias{path: path, resolved: true}, nil
						case "/outside":
							return navAsksAlias{}, nil
						default:
							t.Fatal("unexpected alias path")
							return navAsksAlias{}, nil
						}
					},
				}, navAsksHTTPLimits{response: responseOptions(), reader: state.AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 20}, requests: 1})
				if err != nil {
					t.Fatal(err)
				}
				writer := httptest.NewRecorder()
				handler.ServeHTTP(writer, request)
				if writer.Code != 200 || clocks != 1 || reads != 1 || !now.After(captured.Add(24*time.Hour)) {
					t.Fatalf("captured read status%d clock%d read%d", writer.Code, clocks, reads)
				}
				if directory {
					if len(resolves) != 3 {
						t.Fatal("alias memo cardinality", resolves)
					}
					for _, n := range resolves {
						if n != 1 {
							t.Fatal("repeated alias resolution", resolves)
						}
					}
				}
				module, err := filepath.Abs("../../internal/daemon/ui/asks_data.js")
				if err != nil {
					t.Fatal(err)
				}
				scopeJSON, _ := json.Marshal(scope)
				wantJSON, _ := json.Marshal(want)
				script := `import{readFileSync}from'node:fs';import assert from'node:assert/strict';const{captureAsks}=await import(process.argv[1]);const raw=JSON.parse(readFileSync(0,'utf8')),scope=JSON.parse(process.argv[2]),ids=JSON.parse(process.argv[3]);const out=captureAsks(raw,scope,{rows:10,arrayEntries:10000,stringUnits:20000});
assert.deepEqual(out.scope,scope);assert.deepEqual(out.moved_today,{local_day:'2031-04-05',start_at:{ms:1933131600000,known:true},ids});assert.deepEqual(out.totals,{utc_day:'2031-04-06',skipped:3,calls_today:7});assert.equal(out.report.known,raw.report.known);if(out.report.known){assert.equal(out.report.skipped,3);assert.equal(out.report.calls_today,7);}else assert.deepEqual(out.report,{known:false});
const rows=out.sections.flatMap(s=>s.items);assert.equal(rows.length,scope.mode==='all'?8:7);const row=rows.find(r=>r.id==='9007199254740993');assert.equal(row.session_id,'ABCDEF01-0000-4000-8000-000000000001');assert.equal(row.selectable,true);assert.deepEqual(row.state_at,{ms:1933135200000,known:true});assert.equal(row.first.copy,'> first **word**\n\n');assert.equal(row.latest.copy,'> latest\n\n');assert.deepEqual(row.first.lines,[{kind:'text',table_rows:0,spans:[{c:[],d:false,t:'first '},{c:['b'],d:false,t:'word'}]}]);raw.moved_today.ids.shift();assert.throws(()=>captureAsks(raw,scope,{rows:10,arrayEntries:10000,stringUnits:20000}),{name:'TypeError',message:'could not capture asks snapshot'});assert.deepEqual(out.moved_today.ids,ids);console.log('moved coherent actual capture OK');`
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", script, (&url.URL{Scheme: "file", Path: module}).String(), string(scopeJSON), string(wantJSON))
				cmd.WaitDelay = 2 * time.Second
				cmd.Stdin = strings.NewReader(writer.Body.String())
				out, err := cmd.CombinedOutput()
				if err != nil || string(out) != "moved coherent actual capture OK\n" {
					t.Fatalf("actual moved capture: %s %v", out, err)
				}
			})
		}
	}
}
