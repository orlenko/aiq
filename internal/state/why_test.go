package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The read-only openers never add columns, so the why line must not be in
// the lease queries they run.
func TestLeasesReadOnlyBeforeTheWhyColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddLease(Lease{AccountID: "codex/acct-a", Mode: ModeLong, StartedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`ALTER TABLE leases DROP COLUMN why`); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ls, err := ro.ListLeases(); err != nil || len(ls) != 1 {
		t.Fatalf("leases = %v, %v", ls, err)
	}
}

func TestCleanLine(t *testing.T) {
	if got := CleanLine("a\tb\nc\x7f\u0085é"); got != "a?b\nc??é" {
		t.Fatalf("CleanLine = %q", got)
	}
	long := CleanLine(strings.Repeat("é", MaxWhy))
	if len(long) > MaxWhy || !strings.HasSuffix(long, "é") {
		t.Fatalf("cut to %d bytes, ending %q", len(long), long[len(long)-2:])
	}
}
