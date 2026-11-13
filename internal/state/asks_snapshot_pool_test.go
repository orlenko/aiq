package state

import (
	"context"
	"database/sql/driver"
	"testing"

	"modernc.org/sqlite"
)

func TestAsksSnapshotPoolInterfaces(t *testing.T) {
	_, path := snapshotFixture(t)
	raw, err := (&sqlite.Driver{}).Open("file:" + path + "?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	check := func(label string, conn driver.Conn) {
		closed := false
		defer func() {
			if !closed {
				conn.Close()
			}
		}()
		reset, hasReset := conn.(driver.SessionResetter)
		valid, hasValid := conn.(driver.Validator)
		if !hasReset || !hasValid {
			t.Fatalf("%s missing production pool interfaces: reset=%t validator=%t", label, hasReset, hasValid)
		}
		if !valid.IsValid() || reset.ResetSession(context.Background()) != nil {
			t.Fatal(label, "open driver results changed")
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		closed = true
		if valid.IsValid() || reset.ResetSession(context.Background()) != driver.ErrBadConn {
			t.Fatal(label, "closed driver results changed")
		}
	}
	check("raw", raw)
	wrapped, err := (asksSnapshotConnector{path: path, p: &asksSnapshotProbe{}}).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	check("wrapped", wrapped)
}
