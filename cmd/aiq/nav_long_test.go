package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
)

func longRead(t *testing.T, f *navFactory, dir string, history bool) navLongBody {
	t.Helper()
	path := "/api/nav/long"
	if dir != "" {
		path += "?dir=" + url.QueryEscape(dir)
		if !history {
			path += "&history=0"
		}
	}
	w := factoryGet(t, f, path)
	var body navLongBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("workspace response: %d %s", w.Code, w.Body.String())
	}
	return body
}

func TestNavLongCurrentRequiresWitnessAndTranscript(t *testing.T) {
	for _, tc := range []string{"running", "ended", "unknown", "replaced", "missing transcript", "other workspace", "unknown session", "ambiguous lease", "draining", "future drain"} {
		t.Run(tc, func(t *testing.T) {
			x, in, db := navLiveFixture(t)
			in.workspace = func(_ context.Context, dir string) (string, bool) { return dir, true }
			in.notes = func(context.Context, string, state.NavigatorSnapshotLimits) (navLongNote, error) {
				return navLongNote{Available: true, Exists: true, Archived: 2}, nil
			}
			query := ""
			switch tc {
			case "ended":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Ended }
			case "unknown":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Unknown }
			case "replaced":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation {
					if _, err := db.Exec(`UPDATE leases SET pid=9999`); err != nil {
						t.Fatal(err)
					}
					return terminal.Running
				}
			case "missing transcript":
				query = `UPDATE launches SET session_id='00000009-0000-4000-8000-000000000009'; UPDATE leases SET session_id='00000009-0000-4000-8000-000000000009'`
			case "other workspace":
				query = `UPDATE launches SET cwd='/invented/another'; UPDATE leases SET cwd='/invented/another',workspace='/invented/another'`
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Running }
			case "unknown session":
				query = `UPDATE launches SET session_id=''; UPDATE leases SET session_id=''`
			case "ambiguous lease":
				query = `INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider) SELECT 8,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider FROM leases`
			case "draining":
				query = `UPDATE leases SET drain='draining'`
			case "future drain":
				query = `UPDATE leases SET drain='invented-future-state'`
			}
			if query != "" {
				if _, err := db.Exec(query); err != nil {
					t.Fatal(err)
				}
			}
			f, err := newNavFactory(in, navProductionPolicy())
			if err != nil {
				t.Fatal(err)
			}
			dir := x.work
			if tc == "other workspace" {
				dir = "/invented/another"
			}
			body := longRead(t, f, dir, true)
			wantCurrent := tc == "running" || tc == "draining" || tc == "future drain"
			if (body.Current != nil) != wantCurrent {
				t.Fatalf("current guessed or lost: %+v", body)
			}
			if tc == "ended" && (body.State != "none" || len(body.Earlier) != 1 || len(body.CurrentSpans) != 0) {
				t.Fatal("ended session hidden from history", body)
			}
			if tc == "unknown" || tc == "replaced" || tc == "ambiguous lease" {
				if body.State != "unknown" {
					t.Fatal("failed observation became ended", body)
				}
			}
			if tc == "draining" && body.Lease.Drain != state.DrainDraining {
				t.Fatal("real drain state lost")
			}
			if tc == "future drain" && body.Lease.Drain != "unknown" {
				t.Fatal("open drain vocabulary")
			}
			if !body.Note.Available || body.Note.Archived != 2 {
				t.Fatal("note metadata lost")
			}
			if tc == "other workspace" && len(body.Earlier) != 0 {
				t.Fatal("transcript contradicting launch remained in workspace")
			}
		})
	}
}

func TestNavLongGroupingAndSubdirectoryMapping(t *testing.T) {
	x := newNavAppFixture(t, 0, "Invented answer")
	in := x.input(nil)
	otherID := "00000002-0000-4000-8000-000000000002"
	rows := []state.NavigatorLaunch{}
	for i, sid := range []string{fullIDFixtureID, fullIDFixtureID, otherID, fullIDFixtureID, "", ""} {
		rows = append(rows, state.NavigatorLaunch{ID: int64(i + 1), StartedMS: int64(i+1) * 1000, Provider: "claude", AccountID: "Invented A", Launcher: "fixture", Cwd: x.work + "/sub", Mode: state.ModeLong, SessionID: sid})
	}
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return &state.NavigatorSnapshot{Launches: rows}, nil
	}
	probes := map[string]int{}
	in.workspace = func(_ context.Context, dir string) (string, bool) { probes[dir]++; return x.work, true }
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	body := longRead(t, f, x.work, true)
	if body.Workspace != x.work || body.State != "none" || body.Partial || len(body.Earlier) != 4 {
		t.Fatalf("incorrect workspace groups: %+v", body)
	}
	var group *navLongSession
	for i := range body.Earlier {
		if body.Earlier[i].SessionID == fullIDFixtureID {
			group = &body.Earlier[i]
		}
	}
	if group == nil || len(group.Spans) != 2 || group.Spans[0].From.MS != 1000 || group.Spans[0].Until.MS != 3000 || group.Spans[1].From.MS != 4000 || !group.TranscriptAvailable {
		t.Fatal("nonadjacent identity or adjacent span merge lost", group)
	}
	if probes[x.work+"/sub"] != 1 || probes[x.work] != 1 {
		t.Fatal("workspace probe not memoized", probes)
	}
	list := longRead(t, f, "", true)
	if len(list.Workspaces) != 1 || list.Workspaces[0].Sessions != 4 {
		t.Fatal("counted launches instead of sessions", list)
	}
	// Missing directories do not turn a recorded launch into 'never'.
	f.input.workspace = func(context.Context, string) (string, bool) { return "", false }
	unknown := longRead(t, f, x.work+"/sub", true)
	if !unknown.Partial || unknown.WorkspaceKnown || unknown.State == "never" || len(unknown.Earlier) != 4 || unknown.UnmappedLaunches != 6 {
		t.Fatal("unknown mapping guessed away records", unknown)
	}
}

func TestNavLongLightweightPollAvoidsHistoryAndNotes(t *testing.T) {
	x, in, _ := navLiveFixture(t)
	in.workspace = func(context.Context, string) (string, bool) {
		t.Fatal("lightweight poll mapped launch history")
		return "", false
	}
	in.notes = func(context.Context, string, state.NavigatorSnapshotLimits) (navLongNote, error) {
		t.Fatal("lightweight poll scanned notes")
		return navLongNote{}, nil
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// A transcript cwd equal to the already-recorded workspace needs no git probe.
	body := longRead(t, f, x.work, false)
	if body.HistoryPresent || len(body.Earlier) != 0 || body.Lease == nil {
		t.Fatal("lightweight shape changed", body)
	}
}

func TestNavLongStrictRequestAndWholeResponseFailures(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	in := x.input(nil)
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return &state.NavigatorSnapshot{}, nil
	}
	in.workspace = func(_ context.Context, dir string) (string, bool) { return dir, true }
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/nav/long?", "/api/nav/long?dir=", "/api/nav/long?dir=relative", "/api/nav/long?history=0", "/api/nav/long?dir=%2Finvented&history=true", "/api/nav/long?dir=%2Finvented&dir=%2Finvented", "/api/nav/long?history=1&dir=%2Finvented", "/api/nav/long?unknown=1"} {
		if w := factoryGet(t, f, path); w.Code != 400 {
			t.Fatal("noncanonical query admitted", path, w.Code)
		}
	}
	for _, tc := range []string{"unavailable", "nil snapshot", "row limit", "JSON limit", "panic", "cancelled"} {
		t.Run(tc, func(t *testing.T) {
			f.policy = navProductionPolicy()
			f.input = in
			switch tc {
			case "unavailable":
				f.input.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
					return nil, errors.New("INVENTED-PRIVATE-ERROR")
				}
			case "nil snapshot":
				f.input.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
					return nil, nil
				}
			case "row limit":
				f.policy.sessions.rowBytes = 1
			case "JSON limit":
				f.policy.sessions.bodyBytes = 1
			case "panic":
				f.input.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
					panic("INVENTED-PRIVATE-ERROR")
				}
			}
			r := httptest.NewRequest("GET", "/api/nav/long", nil)
			if tc == "cancelled" {
				ctx, cancel := context.WithCancel(r.Context())
				cancel()
				r = r.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			f.api.ServeHTTP(w, r)
			want := 503
			if tc == "panic" {
				want = 500
			}
			if w.Code != want || strings.Contains(w.Body.String(), "PRIVATE") || strings.Contains(w.Body.String(), "workspaces") {
				t.Fatal("failure escaped whole-response boundary", w.Code, w.Body.String())
			}
		})
	}
}

func TestNavLongBuilderHeldThroughWriteAndCancelableWait(t *testing.T) {
	x := newNavAppFixture(t, 0, "")
	in := x.input(nil)
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return &state.NavigatorSnapshot{}, nil
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	w := &navDirectoryWriter{header: make(http.Header), write: func() {
		if len(f.sessionSlots) != 1 {
			t.Fatal("builder released before signed write")
		}
	}}
	f.api.ServeHTTP(w, httptest.NewRequest("GET", "/api/nav/long", nil))
	if w.status != 200 || len(f.sessionSlots) != 0 {
		t.Fatal("builder leak")
	}
	f.sessionSlots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	out := httptest.NewRecorder()
	f.api.ServeHTTP(out, httptest.NewRequest("GET", "/api/nav/long", nil).WithContext(ctx))
	<-f.sessionSlots
	if out.Code != 503 {
		t.Fatal("wait ignored cancellation")
	}
}

func TestNavLongJSONExactBoundaryForNullableClosedObjects(t *testing.T) {
	for _, live := range []bool{false, true} {
		body := navLongBody{CurrentSpans: []navLongSpan{}, Earlier: []navLongSession{}, Workspaces: []navLongWorkspace{}}
		if live {
			body.Current = &navLongScope{Provider: "claude", ID: fullIDFixtureID}
			body.Lease = &navLongLease{ID: "7", Account: "Invented <&>\naccount"}
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if size, ok := navJSONSize(body, int64(len(encoded))); !ok || size != int64(len(encoded)) {
			t.Fatal("nullable DTO size differs", size, ok)
		}
		if _, ok := navJSONSize(body, int64(len(encoded)-1)); ok {
			t.Fatal("encoded boundary admitted a partial response")
		}
	}
	value := "invented"
	if _, ok := navJSONSize(&value, 1024); ok {
		t.Fatal("nullable DTO support admitted arbitrary pointers")
	}
}
