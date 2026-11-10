package state

import (
	"path/filepath"
	"testing"
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
