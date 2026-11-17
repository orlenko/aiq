package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/orlenko/aiq/internal/state"
)

// Outside a direct Herdr terminal an interactive launch records nothing,
// and a recorded-looking environment without a core records nothing either.
func TestRecordInteractiveTerminalNeedsAVerifiedCore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lease := state.Lease{AccountID: "claude/invented", PID: 2001, Hostname: "invented-host", Mode: state.ModeInteractive, Cwd: "/invented/w", StartedAt: 1}
	if lease.ID, err = st.AddLease(lease); err != nil {
		t.Fatal(err)
	}
	for _, env := range []map[string]string{
		{"HC_TERMINAL_SOCKET": "", "HC_TERMINAL_ID": "", "HC_SOCKET": ""},
		{"HC_TERMINAL_SOCKET": filepath.Join(t.TempDir(), "absent.sock"), "HC_TERMINAL_ID": "t3", "TMUX": "", "TMUX_PANE": "", "STY": ""},
	} {
		for k, v := range env {
			t.Setenv(k, v)
		}
		recordInteractiveTerminal(st, lease)
	}
	got, err := st.ReadLeaseTerminals(context.Background(), "invented-host", 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("recorded %v %v", got, err)
	}
}
