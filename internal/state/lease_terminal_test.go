package state

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLeaseTerminalWriteIsGuardedAndReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(l Lease) int64 {
		id, err := s.AddLease(l)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	inter := add(Lease{AccountID: "claude/invented", PID: 2001, Hostname: "invented-host", Mode: ModeInteractive, Cwd: "/invented/w", StartedAt: 100})
	long := add(Lease{AccountID: "claude/invented", PID: 2002, Hostname: "invented-host", Mode: ModeLong, Cwd: "/invented/w", StartedAt: 100, Pane: "%1"})
	other := add(Lease{AccountID: "codex/invented", PID: 2003, Hostname: "other-host", Mode: ModeInteractive, Cwd: "/invented/w", StartedAt: 100})
	rec := LeaseTerminal{LeaseID: inter, PID: 2001, Socket: "/invented/core.sock", Core: "invented-core", Address: "t3", TerminalPID: 1400}
	for _, bad := range []LeaseTerminal{
		{LeaseID: inter, PID: 2001, Socket: "/s", Core: "c", Address: "t3"},  // no root pid
		{LeaseID: inter, PID: 2001, Socket: "/s", Core: "c", TerminalPID: 1}, // no address
		{LeaseID: inter, PID: 2001, Socket: string(make([]byte, 2000)), Core: "c", Address: "t3", TerminalPID: 1},
	} {
		if err := s.SetLeaseTerminal(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	// Another process's lease, a long lease: no change.
	wrongPID := rec
	wrongPID.PID = 9999
	longRec := rec
	longRec.LeaseID, longRec.PID = long, 2002
	for _, r := range []LeaseTerminal{wrongPID, longRec, rec} {
		if err := s.SetLeaseTerminal(r); err != nil {
			t.Fatal(err)
		}
	}
	// Once only: a second record never replaces the first.
	second := rec
	second.Address = "t9"
	if err := s.SetLeaseTerminal(second); err != nil {
		t.Fatal(err)
	}
	otherRec := rec
	otherRec.LeaseID, otherRec.PID = other, 2003
	if err := s.SetLeaseTerminal(otherRec); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.ReadLeaseTerminals(context.Background(), "invented-host", 10)
	want := rec
	want.StartedAt = 100
	if err != nil || !reflect.DeepEqual(got, map[int64]LeaseTerminal{inter: want}) {
		t.Fatalf("read %v %#v", err, got)
	}
	if _, err := ro.ReadLeaseTerminals(context.Background(), "other-host", 0); !errors.Is(err, ErrLeaseTerminalUnavailable) {
		t.Fatal("zero limit", err)
	}
	add(Lease{AccountID: "claude/invented", PID: 2004, Hostname: "invented-host", Mode: ModeInteractive, Cwd: "/invented/w", StartedAt: 100})
	if _, err := s.db.Exec(`UPDATE leases SET term_socket='/s', term_core='c', term_address='t5', term_pid=1 WHERE pid=2004`); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.ReadLeaseTerminals(context.Background(), "invented-host", 1); !errors.Is(err, ErrLeaseTerminalUnavailable) {
		t.Fatal("row limit", err)
	}
	if _, err := s.db.Exec(`UPDATE leases SET term_pid=1.5 WHERE pid=2004`); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.ReadLeaseTerminals(context.Background(), "invented-host", 10); !errors.Is(err, ErrLeaseTerminalUnavailable) {
		t.Fatal("mistyped row", err)
	}
	// A store from before these columns has no recorded terminals.
	for _, col := range []string{"term_socket", "term_core", "term_address", "term_pid"} {
		if _, err := s.db.Exec(`ALTER TABLE leases DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ro.ReadLeaseTerminals(context.Background(), "invented-host", 10); err != nil || len(got) != 0 {
		t.Fatalf("old store: %v %v", got, err)
	}
}
