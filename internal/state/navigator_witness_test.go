package state

import (
	"context"
	"testing"
)

func TestNavigatorLeaseWitnessRejectsEveryRecordedIdentityChange(t *testing.T) {
	for _, change := range []string{"id=8", "started_at=4", "pid=9999", "depth=1", "turn_started_at=9", "turn_ended_at=8", "provider='codex'", "account_id='other'", "launcher='other'", "cwd='/invented/other'", "mode='interactive'", "workspace='/invented/other'", "pane='different-generation'", "session_id='other'", "drain='other'", "drain_manual=0", "hostname='different-host'", "pid='malformed'"} {
		t.Run(change, func(t *testing.T) {
			s, path := navigatorFixture(t)
			snapshot := navigatorRead(t, s)
			if snapshot.HostIdentity() != "fixture-host" {
				t.Fatal("missing snapshot identity")
			}
			v := snapshot.Leases[0]
			ro, err := OpenReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer ro.Close()
			if match, err := ro.NavigatorLeaseMatches(context.Background(), snapshot.HostIdentity(), v); err != nil || !match {
				t.Fatal("unchanged lease did not match")
			}
			snapshotExec(t, s, "UPDATE leases SET "+change)
			if match, err := ro.NavigatorLeaseMatches(context.Background(), snapshot.HostIdentity(), v); err != nil || match {
				t.Fatal("changed lease reused observation")
			}
		})
	}
}
func TestNavigatorLeaseWitnessCancellationAndUnavailable(t *testing.T) {
	s, _ := navigatorFixture(t)
	snapshot := navigatorRead(t, s)
	v := snapshot.Leases[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if match, err := s.NavigatorLeaseMatches(ctx, snapshot.HostIdentity(), v); match || err != context.Canceled {
		t.Fatal("cancellation lost")
	}
	snapshotExec(t, s, `DELETE FROM leases`)
	if match, err := s.NavigatorLeaseMatches(context.Background(), snapshot.HostIdentity(), v); match || err != nil {
		t.Fatal("missing lease matched")
	}
	snapshotExec(t, s, `DROP TABLE leases`)
	if match, err := s.NavigatorLeaseMatches(context.Background(), snapshot.HostIdentity(), v); match || err != ErrNavigatorSnapshotUnavailable {
		t.Fatal("unavailable store became a match")
	}
}
