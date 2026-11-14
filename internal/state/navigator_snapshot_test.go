package state

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func navigatorFixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metadata.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	snapshotExec(t, s, `INSERT INTO launches(id,started_at_ms,hostname,provider,account_id,launcher,cwd,mode,session_id,lease_id,args) VALUES
	 (1,1000,'fixture-host','claude','account-A','fixture-launcher','/fixture/A','interactive','fixture-id',7,'INVENTED-HIDDEN-ARGS'),
	 (2,2000,'fixture-host','claude','account-B','','/fixture/A','long','other-id',7,'INVENTED-HIDDEN-ARGS'),
	 (3,3000,'fixture-host','claude','account-C','','/fixture/A','long','fixture-id',7,'INVENTED-HIDDEN-ARGS'),
	 (4,9000,'different-host','codex','foreign-account','','/fixture/foreign','long','foreign-id',8,'INVENTED-HIDDEN-ARGS')`)
	snapshotExec(t, s, `INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider,launcher,drain,turn_started_at,turn_ended_at,drain_manual,args,transcript) VALUES
	 (7,'account-C',1234,'fixture-host','long','/fixture/A',0,3,'/fixture/A','opaque-herdr-generation','fixture-id','claude','fixture-launcher','requested',4,2,1,'INVENTED-HIDDEN-ARGS','/invented/hidden-transcript')`)
	return s, path
}
func navigatorLimits() NavigatorSnapshotLimits {
	return NavigatorSnapshotLimits{Rows: 20, TextBytes: 1 << 20}
}
func navigatorRead(t *testing.T, s *Store) *NavigatorSnapshot {
	t.Helper()
	v, err := s.ReadNavigatorSnapshot(context.Background(), "fixture-host", navigatorLimits())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNavigatorSnapshotClosedReadOnlyProjection(t *testing.T) {
	s, path := navigatorFixture(t)
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	v := navigatorRead(t, ro)
	if len(v.Launches) != 3 || len(v.Leases) != 1 {
		t.Fatalf("wrong host/count: %#v", v)
	}
	if v.Launches[0].AccountID != "account-A" || v.Launches[1].SessionID != "other-id" || v.Launches[2].AccountID != "account-C" {
		t.Fatal("nonadjacent account history lost")
	}
	want := NavigatorLease{ID: 7, StartedAt: 3, PID: 1234, TurnStartedAt: 4, TurnEndedAt: 2, Provider: "claude", AccountID: "account-C", Launcher: "fixture-launcher", Cwd: "/fixture/A", Mode: ModeLong, Workspace: "/fixture/A", Pane: "opaque-herdr-generation", SessionID: "fixture-id", Drain: "requested", DrainManual: true}
	if !reflect.DeepEqual(v.Leases[0], want) {
		t.Fatalf("lease projection: %#v", v.Leases[0])
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"INVENTED-HIDDEN-ARGS", "hidden-transcript", "foreign-account", "different-host"} {
		if strings.Contains(string(encoded), excluded) {
			t.Fatal("excluded data exposed")
		}
	}
	if _, err = ro.db.Exec(`UPDATE leases SET pid=99`); err == nil {
		t.Fatal("read-only store permitted write")
	}
	// Text admission must not inspect args/transcript payloads at all.
	snapshotExec(t, s, `UPDATE leases SET args=zeroblob(1048576),transcript=zeroblob(1048576); UPDATE launches SET args=zeroblob(1048576)`)
	if _, err = ro.ReadNavigatorSnapshot(context.Background(), "fixture-host", NavigatorSnapshotLimits{Rows: 4, TextBytes: 4096}); err != nil {
		t.Fatal("excluded payload consumed admission:", err)
	}
	v.Launches[0].AccountID = "changed"
	if navigatorRead(t, ro).Launches[0].AccountID != "account-A" {
		t.Fatal("returned aliases")
	}
}

func TestNavigatorSnapshotWholeAdmissionAndStoredTypes(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		limits      NavigatorSnapshotLimits
		want        error
	}{
		{"rows", "", NavigatorSnapshotLimits{Rows: 3, TextBytes: 1 << 20}, ErrNavigatorSnapshotTooLarge},
		{"fixed bytes", "", NavigatorSnapshotLimits{Rows: 4, TextBytes: 1023}, ErrNavigatorSnapshotTooLarge},
		{"utf8 bytes", `UPDATE launches SET cwd='界界界界' WHERE id=1`, NavigatorSnapshotLimits{Rows: 4, TextBytes: 1030}, ErrNavigatorSnapshotTooLarge},
		{"large column", `UPDATE leases SET workspace=printf('%20000s','')`, NavigatorSnapshotLimits{Rows: 4, TextBytes: 4096}, ErrNavigatorSnapshotTooLarge},
		{"blob text", `UPDATE leases SET workspace=x'ff'`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"invalid utf8", `UPDATE leases SET workspace=CAST(x'ff' AS TEXT)`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"nul", `UPDATE launches SET account_id=char(0) WHERE id=1`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"fractional number", `UPDATE leases SET pid=1.5`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"bad number", `UPDATE leases SET pid='not-a-pid'`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"negative number", `UPDATE leases SET pid=-1`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"unsafe number", `UPDATE launches SET started_at_ms=9007199254740992 WHERE id=1`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"unsafe seconds", `UPDATE leases SET started_at=9007199254741`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
		{"bad boolean", `UPDATE leases SET drain_manual=2`, navigatorLimits(), ErrNavigatorSnapshotUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, path := navigatorFixture(t)
			if tc.query != "" {
				snapshotExec(t, s, tc.query)
			}
			p := &asksSnapshotProbe{fullPrefix: "SELECT id,"}
			ro := asksSnapshotWrapped(t, path, p)
			v, err := ro.ReadNavigatorSnapshot(context.Background(), "fixture-host", tc.limits)
			if v != nil || err != tc.want {
				t.Fatalf("partial/coerced result: %#v %v", v, err)
			}
			if tc.want == ErrNavigatorSnapshotTooLarge && p.full.Load() != 0 {
				t.Fatal("materialized payload before admission")
			}
			if ro.db.Stats().InUse != 0 {
				t.Fatal("connection stranded")
			}
		})
	}
	s, _ := navigatorFixture(t)
	snapshotExec(t, s, `UPDATE leases SET pid=NULL,drain_manual=NULL,workspace=NULL,launcher=NULL`)
	v := navigatorRead(t, s)
	if v.Leases[0].PID != 0 || v.Leases[0].Workspace != "" || v.Leases[0].DrainManual {
		t.Fatal("historical optional nulls")
	}
}

func TestNavigatorSnapshotPinnedAcrossLaunchesAndLeases(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "replacement", true: "empty"}[empty], func(t *testing.T) {
			s, path := navigatorFixture(t)
			if empty {
				snapshotExec(t, s, `DELETE FROM launches;DELETE FROM leases`)
			}
			p := &asksSnapshotProbe{pinQuery: "SELECT COUNT(*) FROM launches WHERE hostname = ?", pinned: make(chan struct{}), resume: make(chan struct{})}
			ro := asksSnapshotWrapped(t, path, p)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			type result struct {
				v   *NavigatorSnapshot
				err error
			}
			done := make(chan result, 1)
			go func() {
				v, err := ro.ReadNavigatorSnapshot(ctx, "fixture-host", navigatorLimits())
				done <- result{v, err}
			}()
			select {
			case <-p.pinned:
			case <-ctx.Done():
				t.Fatal("reader did not pin")
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if empty {
				_, err = tx.ExecContext(ctx, `INSERT INTO launches(id,started_at_ms,hostname,provider,cwd,mode) VALUES(1,5000,'fixture-host','claude','/fixture/new','long');INSERT INTO leases(id,account_id,hostname,provider,cwd,mode) VALUES(1,'new-account','fixture-host','claude','/fixture/new','long')`)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE launches SET account_id='new-account'; UPDATE leases SET account_id='new-account',pid=9999`)
			}
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			close(p.resume)
			old := <-done
			if old.err != nil {
				t.Fatal(old.err)
			}
			if empty {
				if len(old.v.Launches) != 0 || len(old.v.Leases) != 0 {
					t.Fatal("mixed empty pin")
				}
			} else if old.v.Launches[0].AccountID != "account-A" || old.v.Leases[0].AccountID != "account-C" || old.v.Leases[0].PID != 1234 {
				t.Fatal("mixed generations")
			}
			if newer := navigatorRead(t, s); len(newer.Launches) == 0 || newer.Leases[0].AccountID != "new-account" {
				t.Fatal("new committed tuple absent")
			}
		})
	}
}

func TestNavigatorSnapshotFailuresAndCancellation(t *testing.T) {
	s, path := navigatorFixture(t)
	for _, fault := range []string{"iteration", "close", "commit", "after-commit"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := &asksSnapshotProbe{fault: fault, cancel: cancel, fullPrefix: "SELECT id,"}
			ro := asksSnapshotWrapped(t, path, p)
			v, err := ro.ReadNavigatorSnapshot(ctx, "fixture-host", navigatorLimits())
			want := ErrNavigatorSnapshotUnavailable
			if fault == "after-commit" {
				want = context.Canceled
			}
			if v != nil || err != want {
				t.Fatalf("partial or wrong failure: %#v %v", v, err)
			}
			p.fault = ""
			if ro.db.Stats().InUse != 0 {
				t.Fatal("stranded connection")
			}
			navigatorRead(t, ro)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v, err := s.ReadNavigatorSnapshot(ctx, "fixture-host", navigatorLimits()); v != nil || err != context.Canceled {
		t.Fatal("entry cancellation")
	}
	for _, limits := range []NavigatorSnapshotLimits{{0, 1}, {1, 0}, {-1, 1}, {1, -1}} {
		if v, err := s.ReadNavigatorSnapshot(context.Background(), "fixture-host", limits); v != nil || err != ErrNavigatorSnapshotInvalid {
			t.Fatal("invalid limit")
		}
	}
	if _, err := s.ReadNavigatorSnapshot(nil, "fixture-host", navigatorLimits()); err != ErrNavigatorSnapshotInvalid {
		t.Fatal("nil context")
	}
	snapshotExec(t, s, `DROP TABLE leases`)
	if v, err := s.ReadNavigatorSnapshot(context.Background(), "fixture-host", navigatorLimits()); v != nil || err != ErrNavigatorSnapshotUnavailable {
		t.Fatal("missing schema became empty")
	}
}

func TestNavigatorDirectoriesRecencyAndWorkerExclusion(t *testing.T) {
	s := &NavigatorSnapshot{Launches: []NavigatorLaunch{
		{Provider: "claude", Mode: ModeInteractive, Cwd: "/fixture/A/./", StartedMS: 1000},
		{Provider: "codex", Mode: ModeLong, Cwd: "/fixture/B", StartedMS: 2000},
		{Provider: "agy", Mode: ModeInteractive, Cwd: "/fixture/C", StartedMS: 2000},
		{Provider: "claude", Mode: ModeWorker, Cwd: "/fixture/worker", StartedMS: 9000},
		{Provider: "unsupported", Mode: ModeInteractive, Cwd: "/fixture/other", StartedMS: 9000},
		{Provider: "claude", Mode: ModeInteractive, Cwd: "relative", StartedMS: 9000},
		{Provider: "claude", Mode: ModeInteractive, Cwd: "/fixture/zero", StartedMS: 0},
	}, Leases: []NavigatorLease{{Provider: "claude", Mode: ModeLong, Cwd: "/fixture/A", StartedAt: 3}, {Provider: "codex", Mode: ModeInteractive, Cwd: "/fixture/child", Depth: 1, StartedAt: 20}}}
	want := []NavigatorDirectory{{"/fixture/A", 3000}, {"/fixture/B", 2000}, {"/fixture/C", 2000}}
	if got := NavigatorDirectories(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog: %#v", got)
	}
	if len(NavigatorDirectories(nil)) != 0 {
		t.Fatal("nil catalog")
	}
}
