package state

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadOpenAskCounts(t *testing.T) {
	s, path := snapshotFixture(t)
	snapshotExec(t, s, `INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats) VALUES
		(2,'c2','/w','codex','source',110,'open',210,'secret quote','secret latest',111,'[]',0),
		(3,'c3','/x','claude','other',120,'open',220,'q','q',121,'[]',0),
		(4,'c4','/x','claude','other',130,'dismissed',230,'q','q',131,'[]',0),
		(5,'c5','/y','claude','done',140,'resolved',240,'q','q',141,'[]',0)`)
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	got, err := ro.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []AskCount{{"claude", "other", "/x", 1}, {"codex", "source", "/w", 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	// Three open rows of 13 identifier bytes each (codex+source+/w twice, claude+other+/x once).
	if _, err := ro.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{Rows: 2, TextBytes: 1 << 10}); !errors.Is(err, ErrAsksSnapshotTooLarge) {
		t.Fatalf("row limit: err = %v", err)
	}
	if _, err := ro.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{Rows: 10, TextBytes: 3*13 - 1}); !errors.Is(err, ErrAsksSnapshotTooLarge) {
		t.Fatalf("text limit: err = %v", err)
	}
	if _, err := ro.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{Rows: 10, TextBytes: 3 * 13}); err != nil {
		t.Fatalf("exact text limit: err = %v", err)
	}
	if _, err := ro.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{}); !errors.Is(err, ErrAsksSnapshotInvalid) {
		t.Fatalf("zero limits: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ro.ReadOpenAskCounts(ctx, AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 10}); err != context.Canceled {
		t.Fatalf("cancelled: err = %v", err)
	}
}

func TestReadOpenAskCountsRefusesMistypedRows(t *testing.T) {
	s, _ := snapshotFixture(t)
	snapshotExec(t, s, `INSERT INTO asks(id,chain,workspace,provider,session_id,raised_at,state,state_at,first_quote,last_quote,last_seen,refs,repeats)
		VALUES(9,'c9',X'2F77','codex','blob',100,'open',200,'q','q',101,'[]',0)`)
	if _, err := s.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{Rows: 10, TextBytes: 1 << 10}); !errors.Is(err, ErrAsksSnapshotUnavailable) {
		t.Fatalf("blob workspace: err = %v", err)
	}
}

func TestReadOpenAskCountsWithoutAsksTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	got, err := s.ReadOpenAskCounts(context.Background(), AsksSnapshotLimits{Rows: 1, TextBytes: 1})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no asks table: %v, %v", got, err)
	}
}
