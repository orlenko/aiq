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
		flags   []string
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
		{name: "tier and effort", args: []string{"--model-tier", "0", "--account=x", "--effort=3", "--", "--yolo"}, account: "x", flags: []string{"--model-tier", "0", "--effort", "3"}, rest: []string{"--yolo"}},
		{name: "tier out of range", args: []string{"--model-tier", "7"}, wantErr: true},
		{name: "effort without value", args: []string{"--effort"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account, flags, rest, err := parseLongArgs(tt.args)
			if (err != nil) != tt.wantErr || account != tt.account || !reflect.DeepEqual(flags, tt.flags) || !reflect.DeepEqual(rest, tt.rest) {
				t.Fatalf("got (%q, %v, %v, %v), want (%q, %v, %v, error=%v)", account, flags, rest, err, tt.account, tt.flags, tt.rest, tt.wantErr)
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
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, acc := range []state.Account{
		{ID: "codex/bjola", Provider: "codex", Name: "bjola", Enabled: true, Identity: "vlad@bjola.ca"},
		{ID: "codex/off", Provider: "codex", Name: "off"},
	} {
		if err := st.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct{ account, want string }{
		{"claude4", "codex accounts: bjola (vlad@bjola.ca), off"},
		{"off", "account codex/off is disabled"},
	} {
		cfg := config.Default()
		a := &app{cfg: cfg, st: st}
		err := a.longStart("codex", "", []string{"--account", tt.account, "--", "--yolo"})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--account %s: err = %v; want it to mention %q", tt.account, err, tt.want)
		}
		if _, statErr := os.Stat(capture); statErr == nil {
			t.Fatalf("--account %s reached tmux", tt.account)
		}
	}
	for _, launcher := range []string{"", "test-launcher"} {
		t.Run("launcher="+launcher, func(t *testing.T) {
			cfg := config.Default()
			cfg.Long.Fallback = nil
			cfg.Launchers = map[string]config.Launcher{"test-launcher": {Provider: "codex"}}
			a := &app{cfg: cfg, st: st}
			if err := a.longStart("codex", launcher, []string{"--account", "bjola", "--model-tier", "1", "--", "--yolo"}); err != nil {
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
			want = append(want, "--account", "bjola", "--model-tier", "1", "--", "--yolo")
			if string(got) != tmux.Quote(want) {
				t.Fatalf("tmux command = %s; want %s", got, tmux.Quote(want))
			}
			f := parseRunFlags(want[3:])
			if f.account != "bjola" || !f.long || f.launcher != launcher || f.modelTier == nil || *f.modelTier != 1 || strings.Join(f.rest, " ") != "--yolo" {
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
	for _, acc := range []state.Account{
		{ID: "claude/pinned", Provider: "claude", Name: "pinned", Enabled: true, Identity: "pinned@example.com"},
		{ID: "codex/other", Provider: "codex", Name: "other", Enabled: true},
	} {
		if err := st.AddAccount(acc); err != nil {
			t.Fatal(err)
		}
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
		{"bare", []string{"--bare", "s1"}, []string{"run", "claude", "--long", "--fallback", "claude,claude,codex,agy", "--model-tier", "1",
			"--resume-session", "s1", "--", "--dangerously-skip-permissions"}},
		{"tier and extra", []string{"--effort=2", "--bare", "--model-tier", "3", "s1", "--", "--add-dir", "/x"}, []string{"run", "claude", "--long", "--fallback", "claude,claude,codex,agy",
			"--effort=2", "--model-tier", "3", "--resume-session", "s1", "--", "--dangerously-skip-permissions", "--add-dir", "/x"}},
		// Through a launcher the bypass comes only from an earlier launch.
		{"launcher", []string{"--launcher", "box", "s1"}, []string{"run", "claude", "--long", "--fallback", "claude,codex", "--launcher", "box",
			"--model-tier", "1", "--resume-session", "s1", "--"}},
		// --account forces where the resumed session starts, by name or id.
		{"account", []string{"--account", "pinned", "--bare", "s1"}, []string{"run", "claude", "--long", "--fallback", "claude,claude,codex,agy",
			"--account", "pinned", "--model-tier", "1", "--resume-session", "s1", "--", "--dangerously-skip-permissions"}},
		{"account id", []string{"--account=claude/pinned", "--bare", "s1"}, []string{"run", "claude", "--long", "--fallback", "claude,claude,codex,agy",
			"--account", "pinned", "--model-tier", "1", "--resume-session", "s1", "--", "--dangerously-skip-permissions"}},
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
	for _, bad := range [][]string{{"--print", "s1"}, {"--model-tier", "9", "s1"}, {"nope"}, {"--bare", "s1", "--", "--model", "x"},
		{"--account", "s1"}, {"--account=", "s1"}, {"--account", "gone", "s1"}, {"--account", "codex/other", "s1"}} {
		if err := a.longAutoResume(bad); err == nil {
			t.Errorf("long auto resume accepted %q", bad)
		}
		if _, statErr := os.Stat(filepath.Join(bin, "command")); statErr == nil {
			t.Fatalf("long auto resume %q reached tmux", bad)
		}
	}
	// The session decides the provider, so a mismatch is caught on the pick.
	if err := a.longAutoResume([]string{"--account", "codex/other", "s1"}); err == nil || !strings.Contains(err.Error(), "runs on claude") {
		t.Errorf("--account codex/other: err = %v", err)
	}
	// An id names its own provider, so a typo fails before the browser opens.
	if err := a.longAutoResume([]string{"--account", "claude/gone"}); err == nil || !strings.Contains(err.Error(), "claude accounts: pinned") {
		t.Errorf("--account claude/gone: err = %v", err)
	}
}
