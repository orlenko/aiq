package longrun

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/state"
)

func TestNativeUnchangedDoesNotRefreshSavedAuthority(t *testing.T) {
	family := "herdr:" + strings.Repeat("a", 32) + "-" + strings.Repeat("b", 32)
	for _, targets := range [][2]string{{family + "@1", family + "@2"}, {"%1", family + "@1"}, {family + "@1", "%1"}, {family + "@1", family + "@1"}} {
		t.Run(targets[0]+"-"+targets[1], func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			old := state.Lease{ID: 9, AccountID: "codex/fiction", PID: 101, StartedAt: time.Now().Unix(), Hostname: "herdr-identity-fixture.example.invalid", Mode: state.ModeLong, Pane: targets[0], Drain: state.DrainRequested, DrainAt: 100, TurnStartedAt: 0}
			if _, err := st.AddLease(old); err != nil {
				t.Fatal(err)
			}
			moved := false
			sup := &Supervisor{Pool: &pool.Pool{Cfg: config.Default(), St: st}, move: func(state.Lease, state.Account, time.Time) { moved = true }}
			if _, ok := sup.unchanged(old); !ok {
				t.Fatal("unchanged current lease refused")
			}
			st.ReleaseLease(old.ID)
			newer := old
			newer.Pane = targets[1]
			if targets[0] == targets[1] {
				newer.PID = 202
			} // Otherwise PID aliases; only the target changed.
			if _, err := st.AddLease(newer); err != nil {
				t.Fatal(err)
			}
			if got, ok := sup.unchanged(old); ok || got.Pane != old.Pane || got.PID != old.PID {
				t.Fatalf("stale decision acquired new authority: %+v %v", got, ok)
			}
			// Replace the row inside the real Refresh boundary as well: the
			// old blocked decision must not receive the newer authority.
			if err := st.ReleaseLease(newer.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.AddLease(old); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := st.AddAccount(state.Account{ID: old.AccountID, Provider: "codex", Name: "fiction", Enabled: true, Native: true}); err != nil {
				t.Fatal(err)
			}
			if err := st.ReplaceWindows(old.AccountID, "fixture", []state.Window{{Key: "weekly", Kind: state.KindWeekly, UsedPct: 100, ResetsAt: now.Add(time.Hour).Unix(), ObservedAt: now.Unix()}}); err != nil {
				t.Fatal(err)
			}
			refreshed := false
			sup.Refresh = func(string) {
				refreshed = true
				if err := st.ReleaseLease(old.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := st.AddLease(newer); err != nil {
					t.Fatal(err)
				}
			}
			sup.takeoverTo(old, state.Account{ID: "codex/other", Provider: "codex"}, time.Now())
			if !refreshed {
				t.Fatal("refresh boundary not exercised")
			}
			if moved {
				t.Fatal("stale takeover acted on newer lease")
			}
			cur, err := st.GetLease(newer.ID)
			if err != nil || cur.PID != newer.PID || cur.Pane != newer.Pane {
				t.Fatalf("stale takeover changed successor: %+v %v", cur, err)
			}
		})
	}
}
