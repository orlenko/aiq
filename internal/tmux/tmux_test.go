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
