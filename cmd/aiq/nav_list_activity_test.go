package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/terminal"
	"github.com/orlenko/aiq/internal/transcript"
)

func activityRun(t *testing.T, limit int64) *navListRun {
	t.Helper()
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &navListRun{limit: limit, cancel: cancel}
}
func TestNavListActivityVerifiedIdentityScopeAndUnknowns(t *testing.T) {
	for _, kind := range []string{"running", "ended", "unknown", "witness mismatch", "replaced", "ambiguous", "outside displayed rows", "all", "canonical alias", "metadata unavailable", "worker"} {
		t.Run(kind, func(t *testing.T) {
			x, in, db := navLiveFixture(t)
			rows := []transcript.Summary{{Provider: "claude", ID: fullIDFixtureID, Cwd: x.work}}
			q := navListQuery{dir: x.work}
			switch kind {
			case "ended":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Ended }
			case "unknown":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation { return terminal.Unknown }
			case "witness mismatch":
				in.witness = func(context.Context, string, state.NavigatorLease) (bool, error) { return false, nil }
			case "replaced":
				in.observe = func(context.Context, terminal.ObservationTarget) terminal.Observation {
					if _, err := db.Exec("UPDATE leases SET pid=9999"); err != nil {
						t.Fatal(err)
					}
					return terminal.Running
				}
			case "ambiguous":
				if _, err := db.Exec(`INSERT INTO leases(id,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider) SELECT 8,account_id,pid,hostname,mode,cwd,depth,started_at,workspace,pane,session_id,provider FROM leases`); err != nil {
					t.Fatal(err)
				}
			case "outside displayed rows":
				rows = nil
				q.dir = filepath.Join(x.root, "other-directory")
			case "all":
				q = navListQuery{all: true}
			case "canonical alias":
				alias := filepath.Join(x.root, "alias")
				if err := os.Symlink(x.work, alias); err != nil {
					t.Fatal(err)
				}
				q.dir = alias
				if _, err := db.Exec("UPDATE leases SET cwd=?", alias); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("UPDATE launches SET cwd=?", alias); err != nil {
					t.Fatal(err)
				}
			case "metadata unavailable":
				in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
					return nil, errors.New("invented unavailable")
				}
			case "worker":
				rows[0].Worker = true
			}
			f, err := newNavFactory(in, navProductionPolicy())
			if err != nil {
				t.Fatal(err)
			}
			values, counts, err := f.listActivity(context.Background(), x.roots, q, rows, activityRun(t, 1<<24))
			if err != nil {
				t.Fatal(err)
			}
			known := kind == "running" || kind == "ended" || kind == "outside displayed rows" || kind == "all" || kind == "canonical alias" || kind == "worker"
			if counts.Known != known {
				t.Fatal("incomplete observation became exact total", counts)
			}
			live := 0
			if known && kind != "ended" {
				live = 1
			}
			if counts.Live != live {
				t.Fatal("wrong global count", counts)
			}
			elsewhere := 0
			if kind == "outside displayed rows" {
				elsewhere = 1
			}
			if counts.Elsewhere != elsewhere {
				t.Fatal("wrong canonical elsewhere count", counts)
			}
			if len(values) > 0 {
				want := "unknown"
				if kind == "running" || kind == "all" || kind == "canonical alias" {
					want = "live"
				}
				if kind == "ended" {
					want = "ended"
				}
				if values[0].Live != want {
					t.Fatal("wrong row activity", values[0])
				}
				if want == "live" && (values[0].Workspace != x.work || values[0].LeaseID != "7") {
					t.Fatal("lost managed target", values[0])
				}
			}
		})
	}
}

func TestNavListActivityRecentIsNotLiveAndWholeBudgetRefuses(t *testing.T) {
	x, in, _ := navLiveFixture(t)
	in.metadata = func(context.Context, state.NavigatorSnapshotLimits) (*state.NavigatorSnapshot, error) {
		return nil, state.ErrNavigatorSnapshotTooLarge
	}
	f, err := newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.listActivity(context.Background(), x.roots, navListQuery{dir: x.work}, nil, activityRun(t, 1<<24)); !errors.Is(err, errNavAuxTooLarge) {
		t.Fatal("oversize metadata became incomplete success", err)
	}
	in.metadata = nil
	f, err = newNavFactory(in, navProductionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	rows := []transcript.Summary{{Provider: "codex", Updated: in.now().Add(-time.Minute)}, {Provider: "agy", Updated: in.now().Add(-31 * time.Minute)}}
	values, counts, err := f.listActivity(context.Background(), x.roots, navListQuery{all: true}, rows, activityRun(t, 1<<24))
	if err != nil || values[0].Live != "recent" || values[1].Live != "unknown" || counts.Known || counts.Live != 0 {
		t.Fatal("recent promoted to proof", values, counts, err)
	}
	if _, _, err := f.listActivity(context.Background(), x.roots, navListQuery{all: true}, rows, activityRun(t, 1)); !errors.Is(err, errNavAuxTooLarge) {
		t.Fatal("derived text escaped budget", err)
	}
}

func TestNavClaudeRegistrationsBatchesCountsAndConflicts(t *testing.T) {
	for _, kind := range []string{"valid", "duplicate", "dead", "malformed", "oversize", "discarded entries"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			projects := filepath.Join(root, "projects")
			dir := filepath.Join(root, "sessions")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			text := `{"pid":1234,"sessionId":"` + fullIDFixtureID + `"}`
			if kind == "malformed" {
				text += "invalid trailing data"
			}
			if kind == "oversize" {
				text += strings.Repeat(" ", 4096)
			}
			if err := os.WriteFile(filepath.Join(dir, "1234.json"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			roots := []string{projects}
			if kind == "duplicate" {
				other := t.TempDir()
				if err := os.Mkdir(filepath.Join(other, "sessions"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(other, "sessions", "1234.json"), []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
				roots = append(roots, filepath.Join(other, "projects"))
			}
			maxEntries := int64(1000)
			if kind == "discarded entries" {
				maxEntries = 64
				for i := 0; i < 80; i++ {
					if err := os.WriteFile(filepath.Join(dir, strings.Repeat("x", i+1)), nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			budget := int64(1 << 20)
			if kind == "oversize" {
				budget = 2048
			}
			entries := int64(0)
			seen, known, err := navClaudeRegistrations(context.Background(), roots, activityRun(t, budget), &entries, maxEntries, func(int) bool { return kind != "dead" })
			if kind == "oversize" || kind == "discarded entries" {
				if !errors.Is(err, errNavAuxTooLarge) {
					t.Fatal("admission did not refuse entire scan", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "malformed" {
				if known {
					t.Fatal("malformed registration became complete")
				}
				return
			}
			v := seen[fullIDFixtureID]
			if kind == "valid" && v.pid != 1234 {
				t.Fatal(v)
			}
			if (kind == "duplicate" || kind == "dead") && v.pid != 0 {
				t.Fatal("uncertain registration became positive", v)
			}
		})
	}
}
