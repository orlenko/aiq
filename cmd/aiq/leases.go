package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/orlenko/aiq/internal/fleet"
	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/pool"
	"github.com/orlenko/aiq/internal/proc"
	"github.com/orlenko/aiq/internal/state"
)

// cmdLeases prints this machine's live long and interactive sessions for
// another machine's Quotas page ([fleet] in config.toml). It reads state.db
// read-only and needs no config, so it writes nothing.
func cmdLeases(args []string) error {
	if len(args) != 1 || args[0] != "--json" {
		return fmt.Errorf("usage: aiq leases --json")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	doc, err := leasesDoc(ctx, paths.StateDB(), pool.HostnameContext, proc.Alive, time.Now())
	if err != nil {
		return err
	}
	return writeLeases(os.Stdout, doc)
}

func leasesDoc(ctx context.Context, path string, host func(context.Context) (string, error), alive func(int) bool, now time.Time) (doc fleet.Doc, err error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fleet.Collect(nil, "", alive, now.Unix()), nil
	}
	h, err := host(ctx)
	if err != nil || h == "" {
		return fleet.Doc{}, errors.New("this machine's identity is unavailable")
	}
	store, err := state.OpenReadOnly(path)
	if err != nil {
		return fleet.Doc{}, fmt.Errorf("read state: %w", err)
	}
	defer func() {
		if e := store.Close(); e != nil && err == nil {
			err = fmt.Errorf("read state: %w", e)
		}
	}()
	snap, err := store.ReadQuotaSnapshot(ctx, state.QuotaSnapshotLimits{Rows: 20000, TextBytes: 16 << 20})
	if err != nil {
		return fleet.Doc{}, fmt.Errorf("read state: %w", err)
	}
	return fleet.Collect(snap, h, alive, now.Unix()), nil
}

// writeLeases writes doc as one line of JSON, dropping the newest sessions
// until it fits the size a reader admits.
func writeLeases(w io.Writer, doc fleet.Doc) error {
	for {
		b, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if len(b) < fleet.MaxReplyBytes || len(doc.Leases) == 0 {
			_, err = w.Write(append(b, '\n'))
			return err
		}
		doc.Leases = doc.Leases[:len(doc.Leases)/2]
	}
}
