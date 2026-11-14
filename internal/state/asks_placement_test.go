package state

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestAskPlacementReadOnlyBoundedPointRead(t *testing.T) {
	s, path := snapshotFixture(t)
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	want := AskPlacement{Provider: "codex", SessionID: "source", RaisedAt: 100, FirstQuote: "old", LatestQuote: "latest"}
	// Literal20 UTF8 bytes across the four selected strings, not runes or JSON.
	p, err := ro.ReadAskPlacement(context.Background(), 1, 20)
	if err != nil || !reflect.DeepEqual(p, want) {
		t.Fatal("point-read literal differs", p, err)
	}
	if _, err = ro.ReadAskPlacement(context.Background(), 1, 19); err != ErrAsksSnapshotTooLarge {
		t.Fatal("one-short allowance admitted", err)
	}
	if p, err = ro.ReadAskPlacement(context.Background(), 2, 20); err != ErrAskPlacementMissing || p != (AskPlacement{}) {
		t.Fatal("missing point fabricated", err)
	}
	var events int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM asks_events`).Scan(&events); err != nil || events != 0 {
		t.Fatal("point read changed events")
	}
	if got := snapshotRead(t, ro).Items[0]; got.FirstQuote != "old" || got.LastQuote != "latest" || got.State != AskOpen {
		t.Fatal("point read mutated item")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ro.ReadAskPlacement(ctx, 1, 20); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost")
	}
	if _, err = ro.ReadAskPlacement(context.Background(), 1, 20); err != nil {
		t.Fatal("connection not reusable", err)
	}
}

func TestAskPlacementBytesAndFixedFailures(t *testing.T) {
	s, _ := snapshotFixture(t)
	snapshotExec(t, s, `UPDATE asks SET first_quote=?,last_quote=? WHERE id=1`, "é", "😀")
	if _, err := s.ReadAskPlacement(context.Background(), 1, 17); err != nil {
		t.Fatal("UTF8 exact bytes refused", err)
	}
	if _, err := s.ReadAskPlacement(context.Background(), 1, 16); err != ErrAsksSnapshotTooLarge {
		t.Fatal("runes used instead of bytes")
	}
	for _, id := range []int64{0, -1} {
		if _, err := s.ReadAskPlacement(context.Background(), id, 20); err != ErrAsksSnapshotInvalid {
			t.Fatal("invalid ID")
		}
	}
	if _, err := s.ReadAskPlacement(nil, 1, 20); err != ErrAsksSnapshotInvalid {
		t.Fatal("nil context")
	}
	if _, err := s.ReadAskPlacement(context.Background(), 1, 0); err != ErrAsksSnapshotInvalid {
		t.Fatal("invalid allowance")
	}
	snapshotExec(t, s, `DROP TABLE asks`)
	if _, err := s.ReadAskPlacement(context.Background(), 1, 20); err != ErrAsksSnapshotUnavailable {
		t.Fatal("driver cause escaped", err)
	}
}
