package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/pool"
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

// Telemetry comes from a poll: a poll time and a window the poll stored.
func TestPolledNeedsAPollTimeAndAPolledWindow(t *testing.T) {
	line := state.Window{Key: "session", Source: "statusline", ObservedAt: 900}
	poll := state.Window{Key: "weekly", Source: pool.SourceAIQ, ObservedAt: 100}
	for _, c := range []struct {
		name string
		u    state.Usage
		ok   bool
		ws   []state.Window
		want bool
	}{
		{"poll and window", state.Usage{ObservedAt: 300}, true, []state.Window{line, poll}, true},
		{"no usage row", state.Usage{}, false, []state.Window{line, poll}, false},
		{"no poll time", state.Usage{}, true, []state.Window{line, poll}, false},
		{"status line only", state.Usage{ObservedAt: 300}, true, []state.Window{line}, false},
		{"no windows", state.Usage{ObservedAt: 300}, true, nil, false},
	} {
		if got := polled(c.u, c.ok, c.ws); got != c.want {
			t.Errorf("%s: polled = %v, want %v", c.name, got, c.want)
		}
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
	// A poll that stored no windows, with a status-line window still running.
	empty := add("codex/empty", now,
		state.Window{Key: "weekly", Kind: state.KindWeekly, Source: "statusline", ObservedAt: old.Unix(), ResetsAt: now.Add(48 * time.Hour).Unix()})
	if ok, text := telemetry(empty); ok || !strings.Contains(text, "no telemetry yet") {
		t.Errorf("empty poll: %v %s", ok, text)
	}
	// Never polled: a status line and an exhaustion mark wrote the records.
	never := add("codex/never", time.Time{},
		state.Window{Key: "session", Kind: state.KindShort, Source: "statusline", ObservedAt: now.Unix()})
	if ok, text := telemetry(never); ok || !strings.Contains(text, "no telemetry yet") {
		t.Errorf("never polled: %v %s", ok, text)
	}
}
