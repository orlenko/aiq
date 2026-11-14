//go:build darwin || linux

package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/paths"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestConvoParameterWiringPreservesNativeTerminal(t *testing.T) {
	record := nativeRecordFixture(t, t.TempDir())
	nativeInfoFixture(t, record)
	t.Setenv("AIQ_TERMINAL", record.Ref)
	t.Setenv("AIQ_TERMINAL_GENERATION", strconv.FormatUint(record.Generation, 10))
	t.Setenv("TMUX_PANE", "%fictional-other-pane")
	_, ref, err := convoHere()
	if err != nil || ref != record.Target() {
		t.Fatalf("native current terminal: %q, %v", ref, err)
	}
	resolver := newConvoResolver(nil, transcript.Roots{})
	pane, err := resolver.paneInfo(ref)
	if err != nil || pane.ID != ref || pane.PID != record.PID {
		t.Fatalf("native terminal lookup: %+v, %v", pane, err)
	}
}

func TestLiveSessionsParametersPreserveNativeNotes(t *testing.T) {
	record := nativeRecordFixture(t, t.TempDir())
	st, err := state.Open(filepath.Join(t.TempDir(), "fiction.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.AddLease(state.Lease{AccountID: "claude/fiction", Mode: state.ModeLong, Workspace: record.Workspace, Pane: record.Target(), SessionID: "fiction-session", PID: record.PID, StartedAt: time.Now().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	note := liveSessions(st, paths.RealClaudeHome(), "fiction-prefix")["fiction-session"]
	if note.native != record.Target() || note.tmux != "" || !strings.Contains(note.note, "Herdr") {
		t.Fatalf("native lease lost: %+v", note)
	}
}
