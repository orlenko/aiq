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

// A window the provider stopped reporting keeps its old time and sorts first;
// the telemetry is as fresh as the newest window.
func TestNewestObservationIgnoresAWindowThatStoppedReporting(t *testing.T) {
	ws := []state.Window{
		{Key: "session", Kind: state.KindShort, ObservedAt: 100},
		{Key: "weekly", Kind: state.KindWeekly, ObservedAt: 900},
		{Key: "weekly:fable", Kind: state.KindWeekly, ObservedAt: 500},
	}
	if got := newestObservation(ws); got != 900 {
		t.Errorf("newestObservation = %d, want 900", got)
	}
	if got := newestObservation(nil); got != 0 {
		t.Errorf("newestObservation(nil) = %d, want 0", got)
	}
}

func TestDoctorTelemetryIsFreshWhenTheNewestWindowIs(t *testing.T) {
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
	acc := state.Account{ID: "codex/acct-a", Provider: "codex", Name: "acct-a", Enabled: true, Home: filepath.Join(root, "home")}
	if err := a.st.AddAccount(acc); err != nil {
		t.Fatal(err)
	}
	for _, w := range []state.Window{
		{AccountID: acc.ID, Key: "session", Kind: state.KindShort, Source: "statusline", ObservedAt: now.Add(-240 * time.Hour).Unix()},
		{AccountID: acc.ID, Key: "weekly", Kind: state.KindWeekly, Source: "aiq", ObservedAt: now.Unix()},
	} {
		if err := a.st.UpsertWindow(w); err != nil {
			t.Fatal(err)
		}
	}
	r := &doctorReport{byKey: map[string]*doctorFix{}}
	a.checkAccount(r, acc, now, nil)
	found := false
	for _, c := range r.checks {
		if strings.Contains(c.text, "telemetry") {
			found = true
			if !c.ok {
				t.Errorf("stale: %s", c.text)
			}
		}
	}
	if !found {
		t.Fatal("no telemetry check")
	}
}
