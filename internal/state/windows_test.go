package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteRolledWindowsKeepsLiveOnesAndOtherSources(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const acc, other, now = "claude/acct-a", "claude/acct-b", 200
	for _, w := range []Window{
		{AccountID: acc, Key: "rolled", Kind: KindShort, Source: "statusline", ResetsAt: 100},
		{AccountID: acc, Key: "at-now", Kind: KindShort, Source: "statusline", ResetsAt: now},
		{AccountID: acc, Key: "live", Kind: KindShort, Source: "statusline", ResetsAt: now + 1},
		{AccountID: acc, Key: "no-reset", Kind: KindShort, Source: "statusline"},
		{AccountID: acc, Key: "polled", Kind: KindWeekly, Source: "aiq", ResetsAt: 100},
		{AccountID: other, Key: "rolled", Kind: KindShort, Source: "statusline", ResetsAt: 100},
	} {
		if err := st.UpsertWindow(w); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteRolledWindows(acc, "statusline", now); err != nil {
		t.Fatal(err)
	}
	keys := func(id string) map[string]bool {
		ws, err := st.ListWindows(id)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, w := range ws {
			out[w.Key] = true
		}
		return out
	}
	got := keys(acc)
	for k, want := range map[string]bool{"rolled": false, "at-now": false, "live": true, "no-reset": true, "polled": true} {
		if got[k] != want {
			t.Errorf("%s kept = %v, want %v (left %v)", k, got[k], want, got)
		}
	}
	if !keys(other)["rolled"] {
		t.Error("another account's window went too")
	}
}

// An exhaustion mark or a ready mark is not a poll: a usage row either one
// creates has no poll time, and one it updates keeps the poll's.
func TestMarksSetNoPollTime(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	st.MarkExhausted("codex/a", "limit", now.Add(time.Hour), now)
	st.MarkReady("codex/b", now)
	for _, id := range []string{"codex/a", "codex/b"} {
		if u, ok, _ := st.GetUsage(id); !ok || u.ObservedAt != 0 {
			t.Errorf("%s: ok=%v observed_at=%d, want a row with 0", id, ok, u.ObservedAt)
		}
	}
	polled := now.Add(-time.Minute)
	st.SetUsageMeta("codex/c", "", 0, "", polled)
	st.MarkExhausted("codex/c", "limit", now.Add(time.Hour), now)
	st.MarkReady("codex/c", now)
	if u, _, _ := st.GetUsage("codex/c"); u.ObservedAt != polled.Unix() {
		t.Errorf("marks moved the poll time: %d, want %d", u.ObservedAt, polled.Unix())
	}
}
