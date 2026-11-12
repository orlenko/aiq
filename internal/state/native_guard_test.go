package state

import (
	"testing"
	"time"
)

func TestOldNativeCallbackAndCleanupCannotChangeReusedLease(t *testing.T) {
	st := openTemp(t)
	old := Lease{ID: 9, AccountID: "codex/fiction-a", PID: 101, Mode: ModeLong, Pane: "herdr:fiction", StartedAt: 100}
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
	newer.PID = 202
	newer.StartedAt = 101
	newer.AccountID = "codex/fiction-b"
	if _, err := st.AddLease(newer); err != nil {
		t.Fatal(err)
	}
	guard.SetLeaseSession(9, "old-thread")
	guard.SetLeaseTranscript(9, "/invented/old.jsonl")
	guard.MarkTurn(9, true, time.Unix(200, 0))
	guard.SetLeaseDrain(9, DrainReady, time.Unix(200, 0))
	guard.RequestDrainByUser(9, time.Unix(200, 0))
	if err := guard.ReleaseLease(9); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetLease(9)
	if err != nil || got.PID != 202 || got.SessionID != "" || got.Transcript != "" || got.InTurn() || got.Drain != "" {
		t.Fatalf("old callback changed successor: %+v %v", got, err)
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
