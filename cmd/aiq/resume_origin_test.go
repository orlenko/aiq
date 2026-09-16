package main

import (
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/state"
	"github.com/orlenko/aiq/internal/transcript"
)

func TestMatchOrigins(t *testing.T) {
	t0 := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	launches := []state.Launch{
		{ID: 1, StartedAt: at(0), Provider: "codex", Launcher: "safe-codex", Cwd: "/link/w"},
		{ID: 2, StartedAt: at(time.Minute), Provider: "claude", Launcher: "safe-claude", Cwd: "/w", SessionID: "known"},
		{ID: 3, StartedAt: at(2 * time.Hour), Provider: "claude", Cwd: "/w", SessionID: "known"}, // resumed --bare once
		{ID: 6, StartedAt: at(5 * time.Hour), Provider: "claude", Cwd: "/w", SessionID: "plain"},
		{ID: 7, StartedAt: at(6 * time.Hour), Provider: "claude", Cwd: "/w", SessionID: "plain"},
		{ID: 4, StartedAt: at(3 * time.Hour), Provider: "claude", Launcher: "safe-claude", Cwd: "/w", SessionID: "other"},
		{ID: 5, StartedAt: at(4 * time.Hour), Provider: "codex", Cwd: "/w"},
	}
	canon := func(p string) string { return strings.TrimPrefix(p, "/link") }
	sessions := []transcript.Session{
		{Provider: "claude", ID: "known", Cwd: "/w", Started: at(90 * time.Second)},
		{Provider: "codex", ID: "t1", Cwd: "/w", Started: at(2 * time.Second)},
		// Opened with /clear in the process of launch 4.
		{Provider: "claude", ID: "cleared", Cwd: "/w", Started: at(3*time.Hour + 10*time.Minute)},
		// Started just before launch 5, so launch 1 is the latest before it.
		{Provider: "codex", ID: "t2", Cwd: "/w", Started: at(4*time.Hour - 10*time.Second)},
		{Provider: "codex", ID: "t3", Cwd: "/w", Started: at(4*time.Hour - time.Second)}, // clock skew
		{Provider: "codex", ID: "stale", Cwd: "/w", Started: at(30 * time.Hour)},         // beyond the gap
		{Provider: "claude", ID: "before", Cwd: "/w", Started: at(-time.Hour)},
		{Provider: "claude", ID: "plain", Cwd: "/w", Started: at(5 * time.Hour)},
		{Provider: "codex", ID: "far", Cwd: "/x", Started: at(time.Second)},
	}
	got := matchOrigins(sessions, launches, canon)
	want := map[string]struct {
		id    int64
		exact bool
	}{
		"claude/known":   {2, true}, // the sandbox sticks
		"claude/plain":   {7, true},
		"codex/t1":       {1, false},
		"claude/cleared": {4, false},
		"codex/t2":       {1, false},
		"codex/t3":       {5, false},
	}
	if len(got) != len(want) {
		t.Errorf("matched %d sessions, want %d: %+v", len(got), len(want), got)
	}
	for key, w := range want {
		g, ok := got[key]
		if !ok || g.launch.ID != w.id || g.exact != w.exact {
			t.Errorf("%s: got launch %d exact=%v (found %v), want %d exact=%v", key, g.launch.ID, g.exact, ok, w.id, w.exact)
		}
	}
}

func TestResumeLauncher(t *testing.T) {
	cfg := config.Default()
	cfg.Launchers = map[string]config.Launcher{
		"safe-claude": {Provider: "claude"},
		"safe-codex":  {Provider: "codex"},
	}
	s := transcript.Session{Provider: "claude", ID: "s1"}
	ran := func(l string, exact bool) *origin {
		return &origin{launch: state.Launch{Launcher: l}, exact: exact}
	}
	cases := []struct {
		name    string
		org     *origin
		o       resumeOpts
		want    string
		why     string
		errWant string
	}{
		{name: "no record", want: ""},
		{name: "bare launch", org: ran("", true), want: ""},
		{name: "as it ran", org: ran("safe-claude", true), want: "safe-claude", why: "as it ran"},
		{name: "inferred", org: ran("safe-claude", false), want: "safe-claude", why: "last aiq launch before it"},
		{name: "bare wins", org: ran("safe-claude", true), o: resumeOpts{bare: true}, want: "", why: "without launcher safe-claude"},
		{name: "override", org: ran("safe-claude", true), o: resumeOpts{launcher: "safe-claude"}, want: "safe-claude"},
		{name: "gone", org: ran("removed", true), errWant: "no longer registered"},
		{name: "wrong provider", o: resumeOpts{launcher: "safe-codex"}, errWant: "starts codex"},
		{name: "unknown override", o: resumeOpts{launcher: "nope"}, errWant: "no launcher"},
	}
	for _, c := range cases {
		got, why, err := resumeLauncher(cfg, s, c.org, c.o)
		if c.errWant != "" {
			if err == nil || !strings.Contains(err.Error(), c.errWant) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.errWant)
			}
			continue
		}
		if err != nil || got != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: got %q %q %v", c.name, got, why, err)
		}
	}
	if _, err := parseResumeArgs([]string{"--bare", "--launcher", "x"}); err == nil {
		t.Error("--bare with --launcher should be refused")
	}
}

func TestBrowserBareKey(t *testing.T) {
	b := browserFixture()
	// No launcher recorded: b does nothing.
	if _, done := b.handle(keypress{r: 'b'}); done {
		t.Fatal("b without a launcher should not resume")
	}
	b.origins = map[string]origin{"codex/long3333": {launch: state.Launch{Launcher: "safe-codex"}, exact: true}}
	b.handle(keypress{code: keyDown})
	if keys := b.resumeKeys(); !strings.Contains(keys, "via safe-codex") || !strings.Contains(keys, "b resume bare") {
		t.Fatalf("keys: %q", keys)
	}
	rows := b.render()
	var screen []string
	for _, r := range rows {
		screen = append(screen, r.text)
	}
	if !strings.Contains(strings.Join(screen, "\n"), "[via safe-codex] named") {
		t.Fatalf("list lacks the launcher tag:\n%s", strings.Join(screen, "\n"))
	}
	if pick, done := b.handle(keypress{r: 'b'}); !done || pick.ID != "long3333" || !b.bare {
		t.Fatalf("b: %v %v bare=%v", pick, done, b.bare)
	}
	// An open session asks again, for the same key only.
	b = browserFixture()
	b.origins = map[string]origin{"claude/open1111": {launch: state.Launch{Launcher: "safe-claude"}}}
	b.handle(keypress{r: 'b'})
	if _, done := b.handle(keypress{r: 'r'}); done {
		t.Fatal("r after a b question should ask its own question")
	}
	if pick, done := b.handle(keypress{r: 'r'}); !done || pick == nil || b.bare {
		t.Fatalf("second r: %v %v bare=%v", pick, done, b.bare)
	}
}

func TestResumeBypass(t *testing.T) {
	s := transcript.Session{Provider: "codex", ID: "t1", Bypass: true}
	via := func(l string, args ...string) *origin {
		return &origin{launch: state.Launch{Launcher: l, Args: args}}
	}
	cases := []struct {
		name     string
		s        transcript.Session
		org      *origin
		launcher string
		want     bool
	}{
		{"bare follows the transcript", s, via("safe-codex"), "", true},
		{"bare, no bypass recorded", transcript.Session{Provider: "codex"}, nil, "", false},
		{"launcher added it itself", s, via("safe-codex"), "safe-codex", false},
		{"user asked for it", s, via("safe-codex", "--yolo"), "safe-codex", true},
		{"claude permission mode", s, via("safe-claude", "--permission-mode", "bypassPermissions"), "safe-claude", true},
		{"flag after -- is a prompt", s, via("safe-codex", "--", "--yolo"), "safe-codex", false},
		{"other launcher decides", s, via("safe-codex", "--yolo"), "boxed", false},
		{"no record", s, nil, "safe-codex", false},
	}
	for _, c := range cases {
		if got := resumeBypass(c.s, c.org, c.launcher); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
