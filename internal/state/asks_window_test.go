package state

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func windowRead(t *testing.T, s *Store, limits AsksWindowLimits, choose func([]AskIndex) []int64) (*AsksWindow, error) {
	t.Helper()
	return s.ReadAsksWindow(context.Background(), limits, choose)
}

func TestAsksWindowReadsChosenRowsFromOneView(t *testing.T) {
	s, path := snapshotFixture(t)
	snapshotExec(t, s, `INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats,asks_reader,risk)
		VALUES(2,'c2','/w','claude','s2',300,'closed',300,'second','second',300,'[]',0,0.7,0.1)`)
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	var seen []AskIndex
	w, err := windowRead(t, ro, AsksWindowLimits{Rows: 10, TextBytes: 1 << 20}, func(index []AskIndex) []int64 {
		seen = append(seen, index...)
		return []int64{1} // older one only, by choice
	})
	if err != nil {
		t.Fatal(err)
	}
	wantIndex := []AskIndex{{ID: 2, RaisedAt: 300, State: AskClosed, AsksReader: 0.7, Risk: 0.1}, {ID: 1, RaisedAt: 100, State: AskOpen}}
	if !reflect.DeepEqual(seen, wantIndex) || !reflect.DeepEqual(w.Index, wantIndex) {
		t.Fatalf("index %#v", seen)
	}
	want := Ask{ID: 1, Chain: "c", Workspace: "/w", Provider: "codex", SessionID: "source", RaisedAt: 100,
		State: AskOpen, StateAt: 200, FirstQuote: "old", LastQuote: "latest", Refs: []string{"ref"}, Repeats: 2, LastSeen: 101}
	if !reflect.DeepEqual(w.Items, []Ask{want}) || !w.Complete || !w.ReportPresent || w.Status.LastError != "oops" || w.Status.Watched != 4 {
		t.Fatalf("window %#v", w)
	}
	// The revision a reader computes matches the one an action checks.
	full := snapshotRead(t, ro)
	for _, a := range full.Items {
		if a.ID == 1 && AskActionRevision(a) != AskActionRevision(w.Items[0]) {
			t.Fatal("window revision differs from snapshot revision")
		}
	}
}

func TestAsksWindowBounds(t *testing.T) {
	s, _ := snapshotFixture(t)
	snapshotExec(t, s, `INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats)
		VALUES(2,'c2','/w','claude','s2',300,'open',300,?,'x',300,'[]',0)`, strings.Repeat("q", 1000))
	all := func(index []AskIndex) []int64 {
		ids := []int64{}
		for _, v := range index {
			ids = append(ids, v.ID)
		}
		return ids
	}
	if _, err := windowRead(t, s, AsksWindowLimits{Rows: 1, TextBytes: 1 << 20}, all); !errors.Is(err, ErrAsksSnapshotTooLarge) {
		t.Fatal("row limit", err)
	}
	// Text allowance ends the rows early without refusing; rows stay a prefix.
	w, err := windowRead(t, s, AsksWindowLimits{Rows: 10, TextBytes: 500}, all)
	if err != nil || len(w.Items) != 0 || w.Complete || len(w.Index) != 2 {
		t.Fatalf("text allowance: %v %#v", err, w)
	}
	w, err = windowRead(t, s, AsksWindowLimits{Rows: 10, TextBytes: 1040}, func([]AskIndex) []int64 { return []int64{1, 2} })
	if err != nil || len(w.Items) != 1 || w.Items[0].ID != 1 || w.Complete {
		t.Fatalf("prefix: %v %#v", err, w)
	}
	// A chosen id outside the view is a broken caller, never a silent skip.
	if _, err := windowRead(t, s, AsksWindowLimits{Rows: 10, TextBytes: 1 << 20}, func([]AskIndex) []int64 { return []int64{99} }); !errors.Is(err, ErrAsksSnapshotUnavailable) {
		t.Fatal("unknown chosen id", err)
	}
	snapshotExec(t, s, `UPDATE asks SET first_quote = x'71' WHERE id = 2`)
	if _, err := windowRead(t, s, AsksWindowLimits{Rows: 10, TextBytes: 1 << 20}, all); !errors.Is(err, ErrAsksSnapshotUnavailable) {
		t.Fatal("mistyped chosen row", err)
	}
	if _, err := windowRead(t, s, AsksWindowLimits{Rows: 10, TextBytes: 1 << 20}, func([]AskIndex) []int64 { return []int64{1} }); err != nil {
		t.Fatal("unchosen mistyped row refused the window", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadAsksWindow(ctx, AsksWindowLimits{Rows: 10, TextBytes: 1 << 20}, all); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled", err)
	}
	if _, err := s.ReadAsksWindow(context.Background(), AsksWindowLimits{}, all); !errors.Is(err, ErrAsksSnapshotInvalid) {
		t.Fatal("invalid limits", err)
	}
}
