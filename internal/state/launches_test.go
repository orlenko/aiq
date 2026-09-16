package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLaunches(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.UnixMilli(1789500000123)
	add := func(l Launch) {
		t.Helper()
		if _, err := st.AddLaunch(l); err != nil {
			t.Fatal(err)
		}
	}
	add(Launch{StartedAt: t0, Hostname: "h", Provider: "codex", Launcher: "safe-codex", Cwd: "/w", Mode: ModeLong, LeaseID: 7, Args: []string{"--yolo", "a b"}})
	add(Launch{StartedAt: t0.Add(time.Second), Hostname: "h", Provider: "claude", Cwd: "/w", SessionID: "c1"})
	add(Launch{StartedAt: t0.Add(2 * time.Second), Hostname: "h", Provider: "claude", Cwd: "/elsewhere", SessionID: "c2"})
	add(Launch{StartedAt: t0.Add(3 * time.Second), Hostname: "other", Provider: "claude", Cwd: "/w"})

	// A hook reporting the session fills in the launch that did not know it.
	if err := st.SetLeaseSession(7, "t1"); err != nil {
		t.Fatal(err)
	}
	st.SetLeaseSession(7, "t2") // already known: not overwritten

	got, err := st.ListLaunches("h", []string{"/w"}, []string{"c2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 launches, got %+v", got)
	}
	if got[0].SessionID != "t1" || got[0].Launcher != "safe-codex" || !got[0].StartedAt.Equal(t0) || got[0].LeaseID != 7 || strings.Join(got[0].Args, "|") != "--yolo|a b" {
		t.Fatalf("first launch: %+v", got[0])
	}
	if got[1].SessionID != "c1" || got[2].SessionID != "c2" || got[1].Args == nil || len(got[1].Args) != 0 {
		t.Fatalf("order: %+v", got)
	}
	if none, _ := st.ListLaunches("h", nil, nil); none != nil {
		t.Fatalf("no filters should return nothing, got %+v", none)
	}

	st.TrimLaunches(2)
	got, _ = st.ListLaunches("h", []string{"/w", "/elsewhere"}, nil)
	if len(got) != 1 || got[0].SessionID != "c2" {
		t.Fatalf("after trim: %+v", got)
	}
}
