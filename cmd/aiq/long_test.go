package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/orlenko/aiq/internal/config"
	"github.com/orlenko/aiq/internal/state"
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

func TestLongAuto(t *testing.T) {
	// Capture the tmux launch; no session or CLI starts.
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  has-session) exit 1 ;;
  new-session) printf '%s' "$6" > "$AIQ_TEST_OUT/dir"; printf '%s' "$7" > "$AIQ_TEST_OUT/command" ;;
  list-panes) printf '%%1\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIQ_TEST_OUT", bin)
	root := t.TempDir()
	t.Setenv("AIQ_DATA_DIR", root)
	claudeHome := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", t.TempDir())
	work, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	t.Setenv("PWD", work)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.Launchers = map[string]config.Launcher{
		"box":  {Provider: "claude", Fallback: []string{"cbox"}},
		"cbox": {Provider: "codex"},
	}
	a := &app{cfg: cfg, st: st}
	captured := func() (dir string, cmd string) {
		d, err := os.ReadFile(filepath.Join(bin, "dir"))
		if err != nil {
			t.Fatal(err)
		}
		c, err := os.ReadFile(filepath.Join(bin, "command"))
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(bin, "dir"))
		os.Remove(filepath.Join(bin, "command"))
		return string(d), string(c)
	}

	if err := a.longStart("auto", "", []string{"--model-tier", "0", "--effort", "3"}); err != nil {
		t.Fatal(err)
	}
	if _, got := captured(); got != tmux.Quote([]string{self, "run", "auto", "--long", "--model-tier", "0", "--effort", "3"}) {
		t.Fatalf("auto command = %s", got)
	}
	for _, bad := range [][]string{{"--account", "x"}, {"--", "a prompt"}, {"-p", "a prompt"}, {"--launcher", "box"}} {
		if err := a.longStart("auto", "", bad); err == nil {
			t.Errorf("long auto accepted %q", bad)
		}
	}

	// A Claude session in this directory, started once through launcher box.
	c := `"cwd":"` + work + `","sessionId":"s1"`
	lines := `{"type":"user",` + c + `,"timestamp":"2026-09-15T10:01:00Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"fix the bug"}}
{"type":"assistant",` + c + `,"timestamp":"2026-09-15T10:01:05Z","message":{"id":"m1","model":"claude-opus-5","role":"assistant","content":[{"type":"text","text":"Fixed."}]}}
`
	project := filepath.Join(claudeHome, "projects", regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-"))
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "s1.jsonl"), []byte(lines), 0600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{"bare", []string{"--bare", "s1"}, []string{"run", "claude", "--long", "--fallback", "claude,claude,codex", "--model-tier", "1",
			"--resume-session", "s1", "--", "--dangerously-skip-permissions"}},
		{"tier and extra", []string{"--effort=2", "--bare", "--model-tier", "3", "s1", "--", "--add-dir", "/x"}, []string{"run", "claude", "--long", "--fallback", "claude,claude,codex",
			"--effort=2", "--model-tier", "3", "--resume-session", "s1", "--", "--dangerously-skip-permissions", "--add-dir", "/x"}},
		// Through a launcher the bypass comes only from an earlier launch.
		{"launcher", []string{"--launcher", "box", "s1"}, []string{"run", "claude", "--long", "--fallback", "claude,codex", "--launcher", "box",
			"--model-tier", "1", "--resume-session", "s1", "--"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := a.longAutoResume(tt.args); err != nil {
				t.Fatal(err)
			}
			dir, got := captured()
			if want := tmux.Quote(append([]string{self}, tt.want...)); got != want || dir != work {
				t.Fatalf("in %s: %s\nwant in %s: %s", dir, got, work, want)
			}
			f := parseRunFlags(tt.want[2:])
			if !f.long || f.resumeSession != "s1" || f.modelTier == nil {
				t.Fatalf("run flags: %+v", f)
			}
		})
	}
	for _, bad := range [][]string{{"--print", "s1"}, {"--model-tier", "9", "s1"}, {"nope"}, {"--bare", "s1", "--", "--model", "x"}} {
		if err := a.longAutoResume(bad); err == nil {
			t.Errorf("long auto resume accepted %q", bad)
		}
	}
}
