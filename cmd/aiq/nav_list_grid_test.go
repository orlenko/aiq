package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

const gridOtherID = "b2000000-1111-4222-8333-000000000002"

// gridFixture adds, beside the live fixture's managed workspace with a readable
// transcript, a second managed workspace whose conversation has no transcript,
// and open Asks in both.
func gridFixture(t *testing.T) (*navAppFixture, navFactoryInput, string) {
	t.Helper()
	x, in, db := navLiveFixture(t)
	other := filepath.Join(x.root, "invented-other")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO launches(id,started_at_ms,hostname,provider,account_id,launcher,cwd,mode,session_id,lease_id) VALUES(2,?,'invented-host','claude','invented-account','fixture',?,'long',?,8)`, x.at.UnixMilli()-500, other, gridOtherID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider,launcher,drain) VALUES(8,'invented-account',4321,'invented-host','long',?,0,?,?,'%8',?,'claude','fixture','none')`, other, x.at.Unix()-1, other, gridOtherID); err != nil {
		t.Fatal(err)
	}
	in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Running }
	gridAsks(t, x, []gridAsk{
		{101, x.work, fullIDFixtureID, "open"}, {102, x.work, fullIDFixtureID, "open"},
		{103, other, gridOtherID, "open"}, {104, other, gridOtherID, "dismissed"},
		// The listed conversation's own Ask, raised in the other workspace, counts on its row only.
		{105, other, fullIDFixtureID, "open"},
	})
	return x, in, other
}

type gridAsk struct {
	id                         int
	workspace, session, status string
}

func gridAsks(t *testing.T, x *navAppFixture, asks []gridAsk) {
	t.Helper()
	db, err := sql.Open("sqlite", x.asksPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, a := range asks {
		if _, err := db.Exec(`INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats)
			VALUES(?,?,?,'claude',?,1,?,1,'invented quote','invented quote',1,'[]',0)`, a.id, fmt.Sprintf("invented-chain-%d", a.id), a.workspace, a.session, a.status); err != nil {
			t.Fatal(err)
		}
	}
}

func gridList(t *testing.T, f *navFactory, query string) navSessionsBody {
	t.Helper()
	w := factoryGet(t, f, "/api/nav/sessions?"+query)
	var body navSessionsBody
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal("sessions list failed", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "invented quote") {
		t.Fatal("Ask text reached the Sessions list")
	}
	return body
}

func TestNavSessionsGridRowsAndCounts(t *testing.T) {
	x, in, other := gridFixture(t)
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	all := gridList(t, f, "all=1")
	if len(all.Sessions) != 2 || all.Partial {
		t.Fatalf("all scope rows: %+v", all)
	}
	s, w := all.Sessions[0], all.Sessions[1]
	if s.Kind != "session" || s.ID != fullIDFixtureID || s.Workspace != x.work || !s.WorkspaceKnown || s.PendingAsks != 3 || !s.PendingAsksKnown || s.Activity.Live != "live" {
		t.Fatalf("conversation row: %+v", s)
	}
	if w.Kind != "workspace" || w.ID != "" || w.Provider != "claude" || w.Cwd != other || w.Workspace != other || !w.WorkspaceKnown ||
		len(w.Commands) != 0 || w.Label != "" || w.Title != "" || w.PendingAsks != 1 || !w.PendingAsksKnown {
		t.Fatalf("workspace row: %+v", w)
	}
	if a := w.Activity; a.Live != "live" || !a.Managed || a.LeaseID != "8" || a.Pane != "%8" || a.Account != "invented-account" || a.Workspace != other || a.Drain != "none" {
		t.Fatalf("workspace row activity: %+v", a)
	}
	if w.Started != navGridMillis((x.at.Unix()-1)*1000) || w.Updated != navGridMillis(x.at.UnixMilli()-500) {
		t.Fatalf("workspace row times: %s %s", w.Started, w.Updated)
	}

	here := gridList(t, f, "dir="+url.QueryEscape(x.work))
	if len(here.Sessions) != 1 || here.Sessions[0].Kind != "session" {
		t.Fatalf("directory scope admitted another workspace: %+v", here.Sessions)
	}
	// A workspace with no readable conversation is still reachable in its own scope.
	there := gridList(t, f, "dir="+url.QueryEscape(other))
	if len(there.Sessions) != 1 || there.Sessions[0].Kind != "workspace" || there.Sessions[0].Cwd != other {
		t.Fatalf("metadata-only workspace unreachable in scope: %+v", there.Sessions)
	}
}

func TestNavSessionsGridLaunchOnlyAndCoverage(t *testing.T) {
	x, in, db := navLiveFixture(t)
	launchDir := filepath.Join(x.root, "invented-launch-only")
	if _, err := db.Exec(`INSERT INTO launches(id,started_at_ms,hostname,provider,account_id,launcher,cwd,mode,session_id,lease_id) VALUES
		(2,?,'invented-host','codex','invented-account','fixture',?,'long','',0),
		(3,?,'invented-host','claude','invented-account','fixture',?,'interactive','',0)`,
		x.at.UnixMilli()-200, launchDir, x.at.UnixMilli()-100, filepath.Join(x.root, "invented-interactive")); err != nil {
		t.Fatal(err)
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rows := []transcript.Summary{{Provider: "claude", ID: fullIDFixtureID, Cwd: x.work}}
	out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, rows, activityRun(t, 1<<24))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.rows) != 1 || out.partial {
		t.Fatalf("rows: %+v", out.rows)
	}
	r := out.rows[0]
	if r.Cwd != launchDir || r.WorkspaceKnown || r.Provider != "codex" || r.Activity.Live != "unknown" || r.Activity.Managed || r.Updated != navGridMillis(x.at.UnixMilli()-200) {
		t.Fatalf("launch-only row claimed more than recorded: %+v", r)
	}
	if !out.workspaces[0].known || out.workspaces[0].path != x.work || !out.asks[0].known || out.asks[0].count != 0 {
		t.Fatalf("conversation extras: %+v %+v", out.workspaces, out.asks)
	}

	// A listed conversation recorded by the workspace's launch covers it, even
	// when its transcript directory differs.
	if _, err := db.Exec(`UPDATE launches SET session_id=? WHERE id=2`, gridOtherID); err != nil {
		t.Fatal(err)
	}
	rows = append(rows, transcript.Summary{Provider: "codex", ID: gridOtherID, Cwd: filepath.Join(x.root, "elsewhere")})
	out, err = f.listExtras(context.Background(), x.roots, navListQuery{all: true}, rows, activityRun(t, 1<<24))
	if err != nil || len(out.rows) != 0 {
		t.Fatalf("covered workspace repeated: %+v %v", out.rows, err)
	}
	if out.workspaces[1].known || out.workspaces[1].path != "" {
		t.Fatalf("workspace guessed from a cwd: %+v", out.workspaces[1])
	}
}

func TestNavSessionsGridBoundsAndUnknowns(t *testing.T) {
	t.Run("row cap", func(t *testing.T) {
		x, in, db := navLiveFixture(t)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i <= transcript.ListRows; i++ {
			if _, err := tx.Exec(`INSERT INTO launches(id,started_at_ms,hostname,provider,account_id,launcher,cwd,mode,session_id,lease_id) VALUES(?,?,'invented-host','claude','invented-account','fixture',?,'long','',0)`,
				100+i, int64(1000+i), fmt.Sprintf("/invented-cap/%04d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		f, err := newNavFactory(in, navProductionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, nil, activityRun(t, 1<<24))
		if err != nil || !out.partial || len(out.rows) != transcript.ListRows {
			t.Fatal("cap not applied", len(out.rows), out.partial, err)
		}
		// Newest first, so the dropped rows are the oldest; the fixture's own workspace is newest.
		if out.rows[0].Cwd != x.work || out.rows[len(out.rows)-1].Cwd != "/invented-cap/0002" {
			t.Fatal("cap order", out.rows[0].Cwd, out.rows[len(out.rows)-1].Cwd)
		}
		run := activityRun(t, 1<<24)
		if _, _, err := f.listActivity(context.Background(), x.roots, navListQuery{all: true}, nil, run); err != nil {
			t.Fatal(err)
		}
		before := run.used
		run = activityRun(t, 1<<24)
		if _, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, nil, run); err != nil || run.used-before < int64(transcript.ListRows)*1024 {
			t.Fatal("workspace rows were not charged to the list allowance", run.used-before, err)
		}
		run = activityRun(t, before+1024)
		if _, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, nil, run); !errors.Is(err, errNavAuxTooLarge) {
			t.Fatal("workspace rows escaped the list allowance", err)
		}
	})
	t.Run("metadata unavailable", func(t *testing.T) {
		x, in, _ := navLiveFixture(t)
		in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
			return nil, errors.New("invented unavailable")
		}
		f, err := newNavFactory(in, navProductionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, nil, activityRun(t, 1<<24))
		if err != nil || !out.partial || len(out.rows) != 0 {
			t.Fatal("missing metadata read as a complete grid", out, err)
		}
	})
	t.Run("asks slot busy", func(t *testing.T) {
		x, in, _ := gridFixture(t)
		f, err := newNavFactory(in, navProductionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		f.askSlots <- struct{}{}
		defer func() { <-f.askSlots }()
		rows := []transcript.Summary{{Provider: "claude", ID: fullIDFixtureID, Cwd: x.work}}
		out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, rows, activityRun(t, 1<<24))
		if err != nil || out.asks[0].known || len(out.rows) != 1 || out.rows[0].PendingAsksKnown {
			t.Fatal("busy Ask slot failed the list or published counts", out, err)
		}
	})
	t.Run("asks unreadable", func(t *testing.T) {
		x, in, _ := gridFixture(t)
		if err := os.WriteFile(x.asksPath, []byte("not a database"), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := newNavFactory(in, navProductionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, nil, activityRun(t, 1<<24))
		if err != nil || len(out.rows) != 2 || out.rows[0].PendingAsksKnown || out.rows[1].PendingAsksKnown {
			t.Fatal("unreadable Asks published counts", out, err)
		}
	})
	t.Run("asks not set up", func(t *testing.T) {
		x, in, _ := navLiveFixture(t)
		in.asksPath = filepath.Join(x.root, "missing", "asks.db")
		f, err := newNavFactory(in, navProductionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		rows := []transcript.Summary{{Provider: "claude", ID: fullIDFixtureID, Cwd: x.work}}
		out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, rows, activityRun(t, 1<<24))
		if err != nil || !out.asks[0].known || out.asks[0].count != 0 {
			t.Fatal("missing Asks store is not zero open Asks", out.asks, err)
		}
		if _, err := os.Stat(in.asksPath); !os.IsNotExist(err) {
			t.Fatal("count read created the Asks store", err)
		}
	})
	t.Run("asks over limits", func(t *testing.T) {
		x, in, _ := gridFixture(t)
		f, err := newNavFactory(in, navProductionPolicy())
		if err != nil {
			t.Fatal(err)
		}
		run := activityRun(t, 1<<24)
		run.used = run.limit - 1
		if !run.tryReserve(1) || run.tryReserve(1) || run.flags().resource {
			t.Fatal("tryReserve latched or overspent")
		}
		run = activityRun(t, 1<<24)
		rows := []transcript.Summary{{Provider: "claude", ID: fullIDFixtureID, Cwd: x.work}}
		f.policy.asks.reader.Rows = 1
		out, err := f.listExtras(context.Background(), x.roots, navListQuery{all: true}, rows, run)
		if err != nil || out.asks[0].known || run.flags().resource {
			t.Fatal("oversize Ask counts failed the list or were published", out.asks, err)
		}
	})
}
