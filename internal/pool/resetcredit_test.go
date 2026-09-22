package pool

import (
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestResetCreditTarget(t *testing.T) {
	now := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return now.Add(d).Unix() }
	short := func(used float64) state.Window {
		return state.Window{Key: "plan:primary_window", Kind: state.KindShort, UsedPct: used, ResetsAt: at(3 * time.Hour)}
	}
	weekly := func(used float64) state.Window {
		return state.Window{Key: "plan:secondary_window", Kind: state.KindWeekly, UsedPct: used, ResetsAt: at(5 * 24 * time.Hour)}
	}
	cases := []struct {
		name   string
		u      state.Usage
		ws     []state.Window
		want   bool
		wantWk bool
	}{
		{"no credits", state.Usage{}, []state.Window{weekly(100)}, false, false},
		{"not blocked", state.Usage{ResetCredits: 2}, []state.Window{short(80), weekly(99)}, false, false},
		{"weekly block", state.Usage{ResetCredits: 1}, []state.Window{short(40), weekly(100)}, true, true},
		{"weekly block wins over short", state.Usage{ResetCredits: 1}, []state.Window{short(100), weekly(100)}, true, true},
		{"short block, credit keeps", state.Usage{ResetCredits: 1, ResetCreditExpiry: at(4 * 24 * time.Hour)}, []state.Window{short(100), weekly(50)}, false, false},
		{"short block, expiry unknown", state.Usage{ResetCredits: 1}, []state.Window{short(100), weekly(50)}, false, false},
		{"short block, credit expires first", state.Usage{ResetCredits: 1, ResetCreditExpiry: at(2 * time.Hour)}, []state.Window{short(100), weekly(50)}, true, false},
		{"short block, credit expires today", state.Usage{ResetCredits: 1, ResetCreditExpiry: at(20 * time.Hour)}, []state.Window{short(100), weekly(50)}, true, false},
		{"rolled over", state.Usage{ResetCredits: 1}, []state.Window{{Kind: state.KindWeekly, UsedPct: 100, ResetsAt: at(-time.Minute)}}, false, false},
	}
	for _, c := range cases {
		w, ok := ResetCreditTarget(c.u, c.ws, "", now)
		if ok != c.want {
			t.Errorf("%s: spend=%v, want %v", c.name, ok, c.want)
			continue
		}
		if ok && (w.Kind == state.KindWeekly) != c.wantWk {
			t.Errorf("%s: target %s", c.name, w.Kind)
		}
	}
}
