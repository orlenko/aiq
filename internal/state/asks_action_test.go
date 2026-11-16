package state

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAskPersonRevisionStateAndABA(t *testing.T) {
	s, _ := snapshotFixture(t)
	before := snapshotRead(t, s).Items[0]
	revision := AskActionRevision(before)
	changed, err := s.SetAskByPersonRevision(context.Background(), before.ID, revision, AskDismissed, 200, 1<<20)
	if err != nil || changed.State != AskDismissed || changed.ActionSequence <= 0 || AskActionRevision(changed) == revision {
		t.Fatal("conditional dismissal", err)
	}
	if _, err = s.SetAskByPersonRevision(context.Background(), before.ID, revision, AskDismissed, 200, 1<<20); !errors.Is(err, ErrAskActionChanged) {
		t.Fatal("replayed stale action", err)
	}
	restored, err := s.SetAskByPersonRevision(context.Background(), before.ID, AskActionRevision(changed), AskOpen, 200, 1<<20)
	if err != nil || restored.State != AskOpen || AskActionRevision(restored) == revision {
		t.Fatal("same-second state ABA", err)
	}
	if _, err = s.SetAskByPersonRevision(context.Background(), before.ID, revision, AskDismissed, 200, 1<<20); !errors.Is(err, ErrAskActionChanged) {
		t.Fatal("old action survived restore", err)
	}
	read := snapshotRead(t, s).Items[0]
	if AskActionRevision(read) != AskActionRevision(restored) {
		t.Fatal("read and mutation revision differ")
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM asks_events WHERE ask_id = 1`).Scan(&events); err != nil || events != 2 {
		t.Fatal("unexpected action history", err, events)
	}
}

func TestAskPersonRevisionQuestionAndIdentityFence(t *testing.T) {
	for _, column := range []string{"last_quote", "provider", "session_id", "workspace", "chain", "state_text"} {
		t.Run(column, func(t *testing.T) {
			s, _ := snapshotFixture(t)
			before := snapshotRead(t, s).Items[0]
			snapshotExec(t, s, `UPDATE asks SET `+column+` = 'changed invented value' WHERE id = 1`)
			if _, err := s.SetAskByPersonRevision(context.Background(), 1, AskActionRevision(before), AskDismissed, 300, 1<<20); !errors.Is(err, ErrAskActionChanged) {
				t.Fatal("changed question/identity accepted", err)
			}
			var events int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM asks_events`).Scan(&events); err != nil || events != 0 {
				t.Fatal("failed action changed history", err)
			}
		})
	}
}

func TestAskPersonRevisionObservationDoesNotInvalidate(t *testing.T) {
	s, _ := snapshotFixture(t)
	before := snapshotRead(t, s).Items[0]
	snapshotExec(t, s, `UPDATE asks SET last_seen = 9999 WHERE id = 1`)
	if _, err := s.SetAskByPersonRevision(context.Background(), 1, AskActionRevision(before), AskDismissed, 300, 1<<20); err != nil {
		t.Fatal("observation invalidated action", err)
	}
}

func TestAskPersonRevisionConcurrentAndRefusal(t *testing.T) {
	s, path := snapshotFixture(t)
	before := snapshotRead(t, s).Items[0]
	writer, err := OpenHook(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, store := range []*Store{s, writer} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			_, e := store.SetAskByPersonRevision(context.Background(), 1, AskActionRevision(before), AskDismissed, 300, 1<<20)
			errs <- e
		}(store)
	}
	wg.Wait()
	close(errs)
	success, changed := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if errors.Is(e, ErrAskActionChanged) {
			changed++
		} else {
			t.Fatal("unexpected concurrent failure", e)
		}
	}
	if success != 1 || changed != 1 {
		t.Fatal("duplicate update", success, changed)
	}
	current := snapshotRead(t, s).Items[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.SetAskByPersonRevision(ctx, 1, AskActionRevision(current), AskOpen, 301, 1<<20); e == nil {
		t.Fatal("cancelled write committed")
	}
	if _, e := s.SetAskByPersonRevision(context.Background(), 1, AskActionRevision(current), AskOpen, 301, 1); e == nil {
		t.Fatal("oversized question materialized")
	}
	if snapshotRead(t, s).Items[0].State != AskDismissed {
		t.Fatal("refused restore changed state")
	}
}
