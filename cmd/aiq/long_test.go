package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/tmux"
)

func TestParseLongArgs(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		account string
		rest    []string
		wantErr bool
	}{
		{name: "explicit account", args: []string{"--account", "bjola", "--", "--yolo"}, account: "bjola", rest: []string{"--yolo"}},
		{name: "equals", args: []string{"--account=bjola", "--yolo"}, account: "bjola", rest: []string{"--yolo"}},
		{name: "bare CLI", args: []string{"--full-auto", "hello"}, rest: []string{"--full-auto", "hello"}},
		{name: "separator", args: []string{"--", "--account", "literal"}, rest: []string{"--account", "literal"}},
		{name: "stop at CLI", args: []string{"resume", "--account", "literal"}, rest: []string{"resume", "--account", "literal"}},
		{name: "no args"},
		{name: "missing name", args: []string{"--account"}, wantErr: true},
		{name: "empty name", args: []string{"--account="}, wantErr: true},
		{name: "separator is not a name", args: []string{"--account", "--", "--yolo"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account, rest, err := parseLongArgs(tt.args)
			if (err != nil) != tt.wantErr || account != tt.account || !reflect.DeepEqual(rest, tt.rest) {
				t.Fatalf("got (%q, %v, %v), want (%q, %v, error=%v)", account, rest, err, tt.account, tt.rest, tt.wantErr)
			}
		})
	}
}

func TestLongStartAccountForwarding(t *testing.T) {
	// Capture the real tmux launch boundary without starting a session or CLI.
	dir := t.TempDir()
	capture := filepath.Join(dir, "command")
	script := `#!/bin/sh
case "$1" in
  has-session) exit 1 ;;
  new-session) printf '%s' "$7" > "$AIQ_TEST_COMMAND" ;;
  list-panes) printf '%%1\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIQ_TEST_COMMAND", capture)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, launcher := range []string{"", "test-launcher"} {
		t.Run("launcher="+launcher, func(t *testing.T) {
			cfg := config.Default()
			cfg.Long.Fallback = nil
			cfg.Launchers = map[string]config.Launcher{"test-launcher": {Provider: "codex"}}
			a := &app{cfg: cfg}
			if err := a.longStart("codex", launcher, []string{"--account", "bjola", "--", "--yolo"}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{self, "run", "codex", "--long", "--fallback", "codex"}
			if launcher != "" {
				want = append(want, "--launcher", launcher)
			}
			want = append(want, "--account", "bjola", "--", "--yolo")
			if string(got) != tmux.Quote(want) {
				t.Fatalf("tmux command = %s; want %s", got, tmux.Quote(want))
			}
			f := parseRunFlags(want[3:])
			if f.account != "bjola" || !f.long || f.launcher != launcher || strings.Join(f.rest, " ") != "--yolo" {
				t.Fatalf("incorrect run flags: %+v", f)
			}
		})
	}
}
