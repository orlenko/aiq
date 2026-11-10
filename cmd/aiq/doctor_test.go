package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"claude/work": "claude/work",
		"user@x.com":  "user@x.com",
		"team&prod":   "'team&prod'",
		"$(rm -rf ~)": "'$(rm -rf ~)'",
		"it's":        `'it'\''s'`,
		"":            "''",
		"=ls":         "'=ls'",
		"a=b":         "'a=b'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// The telemetry is as fresh as the last successful poll. A window the
// provider stopped reporting keeps its old time and sorts first; a status line
// can be newer than a stalled poll. Neither decides.
func TestTelemetryTimeIsTheLastPoll(t *testing.T) {
	ws := []state.Window{
		{Key: "session", Kind: state.KindShort, ObservedAt: 100},
		{Key: "weekly", Kind: state.KindWeekly, ObservedAt: 900},
		{Key: "weekly:fable", Kind: state.KindWeekly, ObservedAt: 500},
	}
	if got := telemetryTime(state.Usage{ObservedAt: 300}, true, ws); got != 300 {
		t.Errorf("with a poll record = %d, want 300", got)
	}
	if got := telemetryTime(state.Usage{}, false, ws); got != 900 {
		t.Errorf("without one = %d, want the newest window, 900", got)
	}
	if got := telemetryTime(state.Usage{}, false, nil); got != 0 {
		t.Errorf("nothing = %d, want 0", got)
	}
}

func TestDoctorTelemetryAgeIsTheLastPolls(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("AIQ_DATA_DIR", filepath.Join(root, "data"))
	t.Setenv("AIQ_CONFIG", filepath.Join(root, "config.toml"))
	a, err := openApp()
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	now := time.Now()
	old := now.Add(-240 * time.Hour)
	telemetry := func(acc state.Account) (bool, string) {
		r := &doctorReport{byKey: map[string]*doctorFix{}}
		a.checkAccount(r, acc, now, nil)
		for _, c := range r.checks {
			if strings.Contains(c.text, "telemetry") {
				return c.ok, c.text
			}
		}
		t.Fatal("no telemetry check")
		return false, ""
	}
	add := func(id string, polled time.Time, ws ...state.Window) state.Account {
		acc := state.Account{ID: id, Provider: "codex", Name: strings.TrimPrefix(id, "codex/"), Enabled: true, Home: filepath.Join(root, id)}
		if err := a.st.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
		a.st.SetUsageMeta(id, "", 0, "", polled)
		for _, w := range ws {
			w.AccountID = id
			if err := a.st.UpsertWindow(w); err != nil {
				t.Fatal(err)
			}
		}
		return acc
	}
	// Polled just now; the status line's session window is days old.
	fresh := add("codex/fresh", now,
		state.Window{Key: "session", Kind: state.KindShort, Source: "statusline", ObservedAt: old.Unix()},
		state.Window{Key: "weekly", Kind: state.KindWeekly, Source: "aiq", ObservedAt: now.Unix()})
	if ok, text := telemetry(fresh); !ok {
		t.Errorf("fresh poll reported stale: %s", text)
	}
	// Last polled days ago; a running status line keeps one window new.
	stalled := add("codex/stalled", old,
		state.Window{Key: "session", Kind: state.KindShort, Source: "statusline", ObservedAt: now.Unix()},
		state.Window{Key: "weekly", Kind: state.KindWeekly, Source: "aiq", ObservedAt: old.Unix()})
	if ok, text := telemetry(stalled); ok {
		t.Errorf("stalled poll reported fresh: %s", text)
	}
}
