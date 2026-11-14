package state

import (
	"errors"
	"testing"
	"time"
)

func TestRequestDrainTargetRejectsChangedIdentityBeforePreparation(t *testing.T) {
	for _, field := range []string{"id", "start", "pid", "host", "account", "workspace", "pane", "mode", "depth", "missing"} {
		t.Run(field, func(t *testing.T) {
			st := openTemp(t)
			lease := Lease{ID: 7, StartedAt: 123, PID: 2468, Hostname: "fixture-host", AccountID: "codex/fixture", Workspace: "/invented/work", Pane: "%17", Mode: ModeLong}
			if _, err := st.AddLease(lease); err != nil {
				t.Fatal(err)
			}
			target, err := st.GetLeaseTarget(7)
			if err != nil {
				t.Fatal(err)
			}
			switch field {
			case "id":
				target.ID++
			case "start":
				target.StartedAt++
			case "pid":
				target.PID++
			case "host":
				target.Hostname = "other-host"
			case "account":
				target.AccountID = "codex/other"
			case "workspace":
				target.Workspace = "/invented/other"
			case "pane":
				target.Pane = "herdr:fixture@2"
			case "mode":
				target.Mode = ModeWorker
			case "depth":
				_, err = st.db.Exec("UPDATE leases SET depth=1 WHERE id=7")
			case "missing":
				err = st.ReleaseLease(7)
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			matched, _ := st.RequestDrainTarget(target, time.Unix(500, 0), func() error { called = true; return nil })
			if matched || called {
				t.Fatal("changed lease retired its note", matched, called)
			}
			if field != "missing" {
				got, err := st.GetLease(7)
				if err != nil || got.Drain != "" || got.DrainManual {
					t.Fatal("mismatch changed drain", got, err)
				}
			}
		})
	}
}

func TestRequestDrainTargetPublishesOnlyAfterPreparationAndRollsBackFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			st := openTemp(t)
			if _, err := st.AddLease(Lease{ID: 7, StartedAt: 123, PID: 2468, Hostname: "fixture-host", AccountID: "codex/fixture", Workspace: "/invented/work", Pane: "%17", Mode: ModeLong}); err != nil {
				t.Fatal(err)
			}
			target, err := st.GetLeaseTarget(7)
			if err != nil {
				t.Fatal(err)
			}
			var filename string
			if err := st.db.QueryRow("SELECT file FROM pragma_database_list WHERE name='main'").Scan(&filename); err != nil {
				t.Fatal(err)
			}
			reader, err := OpenReadOnly(filename)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			calls := 0
			matched, err := st.RequestDrainTarget(target, time.Unix(500, 0), func() error {
				calls++
				got, err := reader.GetLease(7)
				if err != nil || got.Drain != "" {
					t.Fatal("drain visible before note retirement", got, err)
				}
				if fail {
					return errors.New("invented note changed")
				}
				return nil
			})
			if calls != 1 || matched == fail || (err != nil) != fail {
				t.Fatal("transaction result", calls, matched, err)
			}
			got, err := reader.GetLease(7)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if got.Drain != "" || got.DrainManual {
					t.Fatal("failed preparation persisted drain", got)
				}
			} else if got.Drain != DrainRequested || !got.DrainManual || got.DrainAt != 500 {
				t.Fatal("successful request lost state", got)
			}
		})
	}
}
