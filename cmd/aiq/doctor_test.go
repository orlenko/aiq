package main

import "testing"

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"claude/work": "claude/work",
		"user@x.com":  "user@x.com",
		"team&prod":   "'team&prod'",
		"$(rm -rf ~)": "'$(rm -rf ~)'",
		"it's":        `'it'\''s'`,
		"":            "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
