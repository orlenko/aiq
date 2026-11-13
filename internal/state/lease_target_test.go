package state

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLeaseTargetMinimalReadOnlyProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invented.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately omit args, transcript and every other lease column: a
	// whole-Lease query cannot satisfy this independent projection oracle.
	_, err = db.Exec(`CREATE TABLE leases (id INTEGER PRIMARY KEY, started_at INTEGER, account_id TEXT, hostname TEXT, mode TEXT, workspace TEXT, pane TEXT, pid INTEGER);
	INSERT INTO leases VALUES (18,1234,'codex/fixture','fixture-host','long','/fixture/work','%7',2468);
	INSERT INTO leases(id) VALUES (19);`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			ro.Close()
		}
	})
	want := LeaseTarget{ID: 18, StartedAt: 1234, AccountID: "codex/fixture", Hostname: "fixture-host", Mode: "long", Workspace: "/fixture/work", Pane: "%7", PID: 2468}
	if got, err := ro.GetLeaseTarget(18); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("projection: %+v, %v", got, err)
	}
	if got, err := ro.GetLeaseTarget(19); err != nil || got != (LeaseTarget{ID: 19}) {
		t.Fatalf("NULL projection: %+v, %v", got, err)
	}
	for _, id := range []int64{0, -1, 20} {
		if got, err := ro.GetLeaseTarget(id); !errors.Is(err, ErrLeaseTargetNotFound) || got != (LeaseTarget{}) {
			t.Fatal("missing target did not use typed empty result")
		}
	}
	if _, err := ro.db.Exec(`UPDATE leases SET pid=99 WHERE id=18`); err == nil {
		t.Fatal("projection store accepted write")
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("read-only projection changed database bytes")
	}
	if got, err := ro.GetLeaseTarget(18); got != (LeaseTarget{}) || err != ErrLeaseTargetUnavailable {
		t.Fatal("closed-store error exposed implementation details")
	}
}

func TestLeaseTargetMissingAndCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if st, err := OpenReadOnly(path); err == nil {
		st.Close()
		t.Fatal("missing state initialized")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing database created")
	}
	if err := os.WriteFile(path, []byte("invented corrupt state PRIVATE"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := OpenReadOnly(path)
	if err != nil {
		return
	}
	defer st.Close()
	if got, err := st.GetLeaseTarget(18); got != (LeaseTarget{}) || err != ErrLeaseTargetUnavailable || err.Error() != "lease target unavailable" {
		t.Fatal("corrupt state did not fail closed")
	}
}
