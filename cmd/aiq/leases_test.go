package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/fleet"
)

func TestLeasesDocument(t *testing.T) {
	path := quotaStore(t)
	host := func(context.Context) (string, error) { return "invented-host", nil }
	doc, err := leasesDoc(context.Background(), path, host, func(pid int) bool { return pid <= 2 }, time.Unix(1933160400, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The live long session only: the worker stays local.
	if doc.Schema != fleet.Schema || doc.GeneratedAt != 1933160400 || len(doc.Leases) != 1 {
		t.Fatalf("%+v", doc)
	}
	l := doc.Leases[0]
	if l.Account != "claude/one" || l.Provider != "claude" || l.Identity != "one@invented.test" || l.Mode != "long" || l.Pane != "%7" || l.PID != 1 {
		t.Fatalf("%+v", l)
	}
	var out bytes.Buffer
	if err := writeLeases(&out, doc); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"HIDDEN", "invented-host", "DEAD-LEASE"} {
		if strings.Contains(out.String(), hidden) {
			t.Fatalf("reply carries %q: %s", hidden, out.String())
		}
	}
	if back, err := fleet.Decode(out.Bytes()); err != nil || len(back.Leases) != 1 {
		t.Fatalf("reader refuses the reply: %v", err)
	}
}

func TestLeasesWithoutStateOrIdentity(t *testing.T) {
	alive := func(int) bool { return true }
	doc, err := leasesDoc(context.Background(), filepath.Join(t.TempDir(), "state.db"), nil, alive, time.Unix(1, 0))
	if err != nil || doc.Leases == nil || len(doc.Leases) != 0 {
		t.Fatalf("no state: %+v %v", doc, err)
	}
	noID := func(context.Context) (string, error) { return "", errors.New("invented") }
	if _, err := leasesDoc(context.Background(), quotaStore(t), noID, alive, time.Unix(1, 0)); err == nil {
		t.Fatal("answered without knowing which leases are this machine's")
	}
	if err := cmdLeases(nil); err == nil || err.Error() != "usage: aiq leases --json" {
		t.Fatal(err)
	}
}

func TestWriteLeasesFitsTheReadersLimit(t *testing.T) {
	doc := fleet.Doc{Schema: fleet.Schema, Leases: make([]fleet.Lease, fleet.MaxLeases)}
	for i := range doc.Leases {
		doc.Leases[i] = fleet.Lease{Account: "claude/a", Provider: "claude", Mode: "long", Why: strings.Repeat("w", fleet.MaxTextBytes)}
	}
	var out bytes.Buffer
	if err := writeLeases(&out, doc); err != nil {
		t.Fatal(err)
	}
	back, err := fleet.Decode(out.Bytes())
	if err != nil || len(back.Leases) == 0 || len(back.Leases) == fleet.MaxLeases {
		t.Fatalf("%d bytes, %v", out.Len(), err)
	}
	var check fleet.Doc
	if json.Unmarshal(out.Bytes(), &check) != nil {
		t.Fatal("not JSON")
	}
}
