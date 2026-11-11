package tmux

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestQuote(t *testing.T) {
	cases := map[string][]string{
		`aiq run claude --long -- -p`:                                         {"aiq", "run", "claude", "--long", "--", "-p"},
		`'a b' 'it'\''s' ''`:                                                  {"a b", "it's", ""},
		`'{"hooks":{"Stop":[]}}' '$HOME' 'x;y' '*.go'`:                        {`{"hooks":{"Stop":[]}}`, "$HOME", "x;y", "*.go"},
		`'=ls' 'a=b' '--effort=2' --fallback claude,codex '^x' '~' 'a#b' '雪'`: {"=ls", "a=b", "--effort=2", "--fallback", "claude,codex", "^x", "~", "a#b", "雪"},
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
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	if _, err := NewSession("aiq-test", t.TempDir(), "sleep 30"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() })
	exists, running, err := PaneState("%999999")
	if err != nil {
		t.Skipf("tmux reported the missing pane as an error: %v", err)
	}
	if exists || running {
		t.Fatalf("PaneState(missing) = exists %v, running %v; want false, false", exists, running)
	}
}

// An empty pane id is refused before tmux runs: tmux reads it as the current
// pane, which from outside tmux belongs to the newest session.
func TestEmptyPaneIsRefused(t *testing.T) {
	bin := t.TempDir()
	ran := filepath.Join(bin, "ran")
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := Respawn("", "/", "true"); !errors.Is(err, errNoPane) {
		t.Errorf("Respawn: %v", err)
	}
	if err := RespawnLong("", "/", "true"); !errors.Is(err, errNoPane) {
		t.Errorf("RespawnLong: %v", err)
	}
	if exists, _, err := PaneState(""); exists || !errors.Is(err, errNoPane) {
		t.Errorf("PaneState: %v %v", exists, err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("tmux ran")
	}
}

// tmux runs the line in its default shell. Each word comes back as it went
// in under sh, bash and zsh, zsh with extended globbing included.
func TestQuoteSurvivesTheShell(t *testing.T) {
	args := []string{
		"=ls", "=nosuchcommandxyz", "a=b", "x=~", "x==ls", "x==nosuchcommandxyz", "x=a:=nosuchcommandxyz", "~", "~root", "^x", "a^b", "#x", "a#b", "x~y", "!x", "$HOME", "`id`",
		"*", "?", "[a]", "{a,b}", "a,b", "%1", "@x", "+x", ":x", "-", "--yolo", "a b", "it's", "", "雪", "a\nb", "a\\b", "\"",
	}
	shells := map[string][]string{
		"sh": {"-c"}, "bash": {"-c"}, "zsh": {"-f", "-c"}, "zsh extendedglob": {"-f", "-o", "extendedglob", "-c"},
		"zsh magicequalsubst": {"-f", "-o", "magicequalsubst", "-c"},
	}
	ran := 0
	for name, flags := range shells {
		bin, err := exec.LookPath(strings.Fields(name)[0])
		if err != nil {
			continue
		}
		ran++
		line := "printf '%s\\0' " + Quote(args)
		out, err := exec.Command(bin, append(flags, line)...).Output()
		if err != nil {
			t.Errorf("%s: %v running %s", name, err, line)
			continue
		}
		if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !reflect.DeepEqual(got, args) {
			t.Errorf("%s: got %q\nwant %q", name, got, args)
		}
	}
	if ran == 0 {
		t.Skip("no shell found")
	}
}

// tmux runs a single command string with its default shell, which is the
// user's login shell. aiq's sessions run under /bin/sh instead: on a private
// server whose default shell is a stand-in that records being run, a title
// with quotes and backslashes comes back as it went in through each way aiq
// starts a pane, and the stand-in never runs.
func TestPanesRunUnderShNotTheDefaultShell(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	// A short socket path: a unix socket's path has a small length limit.
	sock, err := os.MkdirTemp("/tmp", "aiqtmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sock) })
	home := t.TempDir()
	t.Setenv("TMUX_TMPDIR", sock)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	t.Setenv("HOME", home) // no ~/.tmux.conf
	t.Setenv("XDG_CONFIG_HOME", home)
	marker := filepath.Join(home, "standin-ran")
	standin := filepath.Join(home, "standin")
	if err := os.WriteFile(standin, []byte("#!/bin/sh\necho \"$@\" >> '"+marker+"'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", standin)
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() })

	title := `it\'s a \\ title 'q' "d" $HOME ; =ls`
	wrote := func(out string) string {
		t.Helper()
		for i := 0; i < 100; i++ {
			if b, err := os.ReadFile(out); err == nil && len(b) > 0 {
				return string(b)
			}
			exec.Command("sleep", "0.05").Run()
		}
		m, _ := os.ReadFile(marker)
		t.Fatalf("%s was not written; stand-in ran with: %q", out, m)
		return ""
	}
	command := func(out string) string {
		return Quote([]string{"/bin/sh", "-c", `printf '%s' "$1" > "$2"; exec sleep 30`, "sh", title, out})
	}
	dir := t.TempDir()
	out := filepath.Join(home, "new-session")
	pane, err := NewSession("aiq-test", dir, command(out))
	if err != nil {
		t.Fatal(err)
	}
	if got := wrote(out); got != title {
		t.Errorf("new-session: got %q, want %q", got, title)
	}
	// The pane's own process is the command, not a shell waiting for it:
	// adoption reads the pane pid's environment.
	pid, err := run("display-message", "-p", "-t", pane, "#{pane_pid}")
	if err != nil {
		t.Fatal(err)
	}
	if comm, _ := exec.Command("ps", "-o", "comm=", "-p", pid).Output(); filepath.Base(strings.TrimSpace(string(comm))) != "sleep" {
		t.Errorf("the pane runs %q, not the command", strings.TrimSpace(string(comm)))
	}
	if got := shellArgs("cmd"); !reflect.DeepEqual(got, []string{"/bin/sh", "-c", "exec cmd"}) {
		t.Errorf("shellArgs = %q", got)
	}
	out = filepath.Join(home, "respawn")
	if err := Respawn(pane, dir, command(out)); err != nil {
		t.Fatal(err)
	}
	if got := wrote(out); got != title {
		t.Errorf("respawn-pane: got %q, want %q", got, title)
	}
	out = filepath.Join(home, "respawn-long")
	if err := RespawnLong(pane, dir, command(out)); err != nil {
		t.Fatal(err)
	}
	if got := wrote(out); got != title {
		t.Errorf("respawn long: got %q, want %q", got, title)
	}
	if m, err := os.ReadFile(marker); err == nil {
		t.Fatalf("tmux's default shell ran: %q", m)
	}
}

// The daemon may run without a UTF-8 locale; a window name still reads back
// as it was set.
func TestWindowNameWithoutALocale(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	pane, err := NewSession("aiq-test", t.TempDir(), "sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("tmux", "kill-server").Run() })
	for _, k := range []string{"LANG", "LC_ALL", "LC_CTYPE"} {
		t.Setenv(k, "C")
	}
	name := "codex/acct-a · moving"
	if err := NameWindow(pane, name); err != nil {
		t.Fatal(err)
	}
	if got, err := WindowName(pane); err != nil || got != name {
		t.Fatalf("WindowName = %q, %v; want %q", got, err, name)
	}
}

func TestSafeName(t *testing.T) {
	if got := SafeName("codex/a#{pane_id}#[fg=red];\x1b"); got != "codex/a_{pane_id}_[fg=red]__" {
		t.Fatalf("SafeName = %q", got)
	}
	if err := NameWindow("", "x"); err == nil {
		t.Fatal("an empty pane was renamed")
	}
}
