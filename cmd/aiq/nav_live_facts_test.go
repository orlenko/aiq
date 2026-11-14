package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

// All observation callbacks and the database belong to this invented fixture.
func navLiveFixture(t *testing.T) (*navAppFixture, navFactoryInput, *sql.DB) {
	t.Helper()
	x := newNavAppFixture(t, 0, "Invented response")
	path := filepath.Join(x.root, "metadata.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`INSERT INTO launches(id,started_at_ms,hostname,provider,account_id,launcher,cwd,mode,session_id,lease_id) VALUES(1,?,'invented-host','claude','invented-account','fixture',?,'long',?,7)`, x.at.UnixMilli()-1000, x.work, fullIDFixtureID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider,launcher,turn_started_at,turn_ended_at) VALUES(7,'invented-account',1234,'invented-host','long',?,0,?,?,'%9',?,'claude','fixture',?,?)`, x.work, x.at.Unix()-1, x.work, fullIDFixtureID, x.at.Unix(), x.at.Unix()-1)
	if err != nil {
		t.Fatal(err)
	}
	in := x.input(nil)
	in.metadata = func(ctx context.Context, limits state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		ro, err := state.OpenReadOnly(path)
		if err != nil {
			return nil, err
		}
		defer ro.Close()
		return ro.ReadNavigatorSnapshot(ctx, "invented-host", limits)
	}
	in.witness = navProductionWitness(path)
	in.observe = func(_ context.Context, target terminal.ObservationTarget) terminal.Observation {
		if target.Ref != "%9" || target.PID != 1234 || target.Workspace != x.work || target.LeaseID != 7 || target.LaunchID != 1 {
			t.Error("wrong selected terminal identity")
		}
		return terminal.Running
	}
	return x, in, db
}

func TestNavLiveFactsNeedsPositiveBackendAndUnchangedLease(t *testing.T) {
	for _, tc := range []string{"running", "ended", "unknown", "lease changes during observation", "observer failure", "launch start differs", "unmanaged", "ambiguous", "agy"} {
		t.Run(tc, func(t *testing.T) {
			x, in, db := navLiveFixture(t)
			s := &transcript.Session{Provider: "claude", ID: fullIDFixtureID, Cwd: x.work}
			query := ""
			switch tc {
			case "ended":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Ended }
			case "unknown":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Unknown }
			case "observer failure":
				in.witness = func(context.Context, string, state.NavigatorLease) (bool, error) {
					return false, errors.New("invented unavailable")
				}
			case "lease changes during observation":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation {
					if _, err := db.Exec(`UPDATE leases SET pid=9999`); err != nil {
						t.Fatal(err)
					}
					return terminal.Running
				}
			case "launch start differs":
				query = `UPDATE launches SET started_at_ms=started_at_ms+1000`
			case "unmanaged":
				query = `DELETE FROM leases`
			case "ambiguous":
				query = `INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider) SELECT 8,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider FROM leases`
			case "agy":
				s.Provider = "agy"
				query = `UPDATE launches SET provider='agy'; UPDATE leases SET provider='agy'`
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
			facts, err := f.liveFacts(context.Background(), s, false)
			if err != nil {
				t.Fatal(err)
			}
			want := terminal.Unknown
			if tc == "running" || tc == "agy" {
				want = terminal.Running
			}
			if tc == "ended" {
				want = terminal.Ended
			}
			if facts.observation != want {
				t.Fatalf("got observation %d want %d", facts.observation, want)
			}
			if tc == "running" {
				if !facts.workingKnown || !facts.working || facts.elapsedMS != time.Hour.Milliseconds()+123 {
					t.Fatal("working clock not correlated", facts)
				}
			} else if facts.workingKnown || facts.working || facts.elapsedMS != 0 {
				t.Fatal("unknown/ended/agy reported working clock")
			}
		})
	}
}

func TestNavStateManagedContextCannotRetargetRetainedDetail(t *testing.T) {
	x, in, db := navLiveFixture(t)
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	detail := selectionDetail(t, f, fullIDFixtureID)
	w := factoryGet(t, f, statePath(detail.Selection))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"live":"live"`) {
		t.Fatal("positive state missing", w.Body.String())
	}
	if _, err = db.Exec(`UPDATE leases SET pane='%10'`); err != nil {
		t.Fatal(err)
	}
	// A changed reference is a different selection context even if parsing did
	// not change. No live facts from the replacement may label the old view.
	in.observe = nil
	f.input.observe = nil
	w = factoryGet(t, f, statePath(detail.Selection))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"match":false`) || strings.Contains(w.Body.String(), `"live"`) {
		t.Fatal("replacement facts leaked into retained view")
	}
	newDetail := selectionDetail(t, f, fullIDFixtureID)
	if newDetail.Selection == detail.Selection || newDetail.Session.Cwd != x.work {
		t.Fatal("context replacement reused selection")
	}
}

func TestNavAccountFactsRequireUnambiguousCompleteTimeSpan(t *testing.T) {
	at := time.Date(2031, 1, 1, 12, 0, 0, 0, time.UTC)
	s := &transcript.Session{Provider: "claude", ID: fullIDFixtureID, Cwd: "/invented/work"}
	launch := func(delta time.Duration, account string) state.NavigatorLaunch {
		return state.NavigatorLaunch{StartedMS: at.Add(delta).UnixMilli(), Provider: s.Provider, SessionID: s.ID, Cwd: s.Cwd, Mode: state.ModeLong, AccountID: account}
	}
	snapshot := &state.NavigatorSnapshot{Launches: []state.NavigatorLaunch{launch(-time.Minute, "A"), launch(10*time.Second, "B"), launch(20*time.Second, "A")}}
	s.Turns = []transcript.Turn{{Started: at, Ended: at.Add(time.Second)}, {Started: at.Add(9 * time.Second), Ended: at.Add(11 * time.Second)}, {Started: at.Add(21 * time.Second), Ended: at.Add(22 * time.Second)}, {Ended: at}}
	facts, err := navAccountsFromSnapshot(context.Background(), s, snapshot, at.Add(time.Hour), 100, 100, 100)
	if err != nil || len(facts) != 4 || !facts[0].known || facts[0].label != "A" || facts[1].known || !facts[2].known || facts[2].label != "A" || facts[3].known {
		t.Fatal("account spans mislabelled", facts, err)
	}
	if _, err = navAccountsFromSnapshot(context.Background(), s, snapshot, at, 100, 1, 100); err != errNavAuxTooLarge {
		t.Fatal("repeated account labels escaped aggregate admission")
	}
	snapshot.Launches = append(snapshot.Launches, launch(-time.Minute, "different"))
	facts, err = navAccountsFromSnapshot(context.Background(), s, snapshot, at, 100, 100, 100)
	if err != nil || facts[0].known {
		t.Fatal("same-time contradictory accounts guessed")
	}
	// Display sanitization must not merge distinct recorded identities.
	snapshot.Launches = []state.NavigatorLaunch{launch(-time.Minute, "A\nB"), launch(-time.Minute, "A B")}
	facts, err = navAccountsFromSnapshot(context.Background(), s, snapshot, at, 100, 100, 100)
	if err != nil || facts != nil {
		t.Fatal("sanitized account collision became known", facts, err)
	}
}
