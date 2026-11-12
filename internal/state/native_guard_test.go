package state

import (
	"testing"
	"time"
)

func TestOldNativeCallbackAndCleanupCannotChangeReusedLease(t *testing.T) {
	st := openTemp(t)
	old := Lease{ID: 9, AccountID: "codex/fiction-a", PID: 101, Mode: ModeLong, Pane: "herdr:fiction@1", StartedAt: 100}
	if _, err := st.AddLease(old); err != nil {
		t.Fatal(err)
	}
	launch, err := st.AddLaunch(Launch{StartedAt: time.Unix(100, 0), Provider: "codex", AccountID: old.AccountID, LeaseID: old.ID, Mode: ModeLong})
	if err != nil {
		t.Fatal(err)
	}
	guard := st.ForLease(old, launch)
	if err := st.ReleaseLease(old.ID); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Pane = "herdr:fiction@2" // ID, start second and PID deliberately alias.
	if _, err := st.AddLease(newer); err != nil {
		t.Fatal(err)
	}
	guard.SetLeaseWhy(9, old.StartedAt, old.PID, "stale why")
	guard.SetLeaseSession(9, "old-thread")
	guard.SetLeaseTranscript(9, "/invented/old.jsonl")
	guard.MarkTurn(9, true, time.Unix(200, 0))
	guard.SetLeaseDrain(9, DrainReady, time.Unix(200, 0))
	guard.RequestDrainByUser(9, time.Unix(200, 0))
	if err := guard.ReleaseLease(9); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetLease(9)
	if err != nil || got.Pane != newer.Pane || got.SessionID != "" || got.Transcript != "" || got.InTurn() || got.Drain != "" {
		t.Fatalf("old callback changed successor: %+v %v", got, err)
	}
	if whys, err := st.LeaseWhys(); err != nil || len(whys) != 0 {
		t.Fatalf("stale why changed successor: %v %v", whys, err)
	}
	if l, err := st.GetLaunch(launch); err != nil || l.SessionID != "" {
		t.Fatalf("stale callback changed launch history: %+v %v", l, err)
	}
	current := st.ForLease(got, launch)
	if err := current.SetLeaseSession(9, "current-thread"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetLease(9); err != nil || got.SessionID != "current-thread" {
		t.Fatalf("current callback did not apply: %+v %v", got, err)
	}
}

func TestNativeAdoptionTurnIsInsertedAtomically(t *testing.T) {
	st := openTemp(t)
	for _, pane := range []string{"herdr:fiction@1", "%1"} {
		id, err := st.AddLease(Lease{Pane: pane, Mode: ModeLong, TurnStartedAt: 200, TurnEndedAt: 100})
		if err != nil {
			t.Fatal(err)
		}
		l, err := st.GetLease(id)
		if err != nil {
			t.Fatal(err)
		}
		if l.InTurn() != (pane != "%1") {
			t.Fatalf("insert lost native busy state or changed legacy: %+v", l)
		}
	}
}
