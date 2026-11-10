package state

import (
	"path/filepath"
	"testing"
)

func TestDeleteWindowsBeforeKeepsNewerAndOtherSources(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const acc, other = "claude/acct-a", "claude/acct-b"
	for _, w := range []Window{
		{AccountID: acc, Key: "session", Kind: KindShort, Source: "statusline", ObservedAt: 100},
		{AccountID: acc, Key: "late", Kind: KindShort, Source: "statusline", ObservedAt: 300},
		{AccountID: acc, Key: "weekly", Kind: KindWeekly, Source: "aiq", ObservedAt: 100},
		{AccountID: other, Key: "session", Kind: KindShort, Source: "statusline", ObservedAt: 100},
	} {
		if err := st.UpsertWindow(w); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DeleteWindowsBefore(acc, "statusline", 200); err != nil {
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
	if got := keys(acc); got["session"] || !got["late"] || !got["weekly"] || len(got) != 2 {
		t.Errorf("account windows after delete: %v", got)
	}
	if got := keys(other); !got["session"] {
		t.Errorf("another account's window went too: %v", got)
	}
}
