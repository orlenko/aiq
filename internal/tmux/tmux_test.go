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
