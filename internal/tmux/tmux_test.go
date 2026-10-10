package tmux

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string][]string{
		`aiq run claude --long -- -p`:                  {"aiq", "run", "claude", "--long", "--", "-p"},
		`'a b' 'it'\''s' ''`:                           {"a b", "it's", ""},
		`'{"hooks":{"Stop":[]}}' '$HOME' 'x;y' '*.go'`: {`{"hooks":{"Stop":[]}}`, "$HOME", "x;y", "*.go"},
	}
	for want, args := range cases {
		if got := Quote(args); got != want {
			t.Errorf("Quote(%q) = %s, want %s", args, got, want)
		}
	}
}

func TestParsePanes(t *testing.T) {
	out := "%97:3176984:0:aiq-ops-9d58:/x/aiq run auto --long\n" +
		"%80:455204:0:aiq-ops2-3bd1:/x/aiq run codex --nudge 'note: read it' --\n" +
		"%3:77:1:fleet:\n" +
		"garbage\n"
	got := parsePanes(out)
	if len(got) != 3 {
		t.Fatalf("got %d panes: %+v", len(got), got)
	}
	if p := got[1]; p.ID != "%80" || p.PID != 455204 || p.Dead || p.Session != "aiq-ops2-3bd1" ||
		p.StartCommand != "/x/aiq run codex --nudge 'note: read it' --" {
		t.Fatalf("got %+v", p)
	}
	if !got[2].Dead || got[2].Session != "fleet" {
		t.Fatalf("got %+v", got[2])
	}
}

func TestParseClientThemes(t *testing.T) {
	got := parseClientThemes("/dev/ttys021 dark\n/dev/ttys004 \n/dev/pts/3 light\n\n")
	want := []ClientTheme{{"/dev/ttys021", "dark"}, {"/dev/ttys004", ""}, {"/dev/pts/3", "light"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %+v, want %+v", got[i], want[i])
		}
	}
}

// tmux 3.7 exits 0 with empty formats for a missing pane; PaneState must not
// read that as a dead pane that still exists.
func TestPaneStateMissingPane(t *testing.T) {
	if _, err := run("list-sessions"); err != nil {
		t.Skip("no tmux server")
	}
	exists, running, err := PaneState("%999999")
	if err != nil {
		t.Skipf("tmux reported the missing pane as an error: %v", err)
	}
	if exists || running {
		t.Fatalf("PaneState(missing) = exists %v, running %v; want false, false", exists, running)
	}
}
