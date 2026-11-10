package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The runner may set none of what Main clears, so plant some for it to clear.
func TestMain(m *testing.M) {
	if !inherited(os.Getenv(marker)) { // whenever Main will set up
		for _, k := range []string{"TMUX", "TMUX_PANE", "DBUS_SESSION_BUS_ADDRESS", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN",
			"OPENAI_API_KEY", "GH_TOKEN", "COPILOT_GITHUB_TOKEN", "GIT_DIR", "GIT_INDEX_FILE", "AIQ_ACCOUNT",
			"CODEX_HOME", "CLAUDE_CONFIG_DIR", "COPILOT_HOME", "QUOTA_HOME"} {
			os.Setenv(k, "planted")
		}
	}
	Main(m)
}

func under(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func TestTheEnvironmentIsTemporary(t *testing.T) {
	root := os.Getenv(marker)
	if !inherited(root) {
		t.Fatalf("%s=%q", marker, root)
	}
	data := os.Getenv("AIQ_DATA_DIR")
	for _, k := range []string{"HOME", "AIQ_DATA_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "COPILOT_HOME", "QUOTA_HOME",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR", "TMUX_TMPDIR"} {
		v := os.Getenv(k)
		if k != "TMUX_TMPDIR" && !under(v, root) {
			t.Errorf("%s=%q is not under the root", k, v)
		}
		if k != "AIQ_DATA_DIR" && under(v, data) {
			t.Errorf("%s=%q is under AIQ_DATA_DIR", k, v)
		}
	}
	if tmux := os.Getenv("TMUX_TMPDIR"); !strings.HasPrefix(filepath.Base(tmux), "aiqt") {
		t.Errorf("TMUX_TMPDIR=%q is not the guard's", tmux)
	} else if fi, err := os.Stat(tmux); err != nil || !fi.IsDir() {
		t.Errorf("TMUX_TMPDIR: %v", err)
	}
	for _, k := range []string{"TMUX", "TMUX_PANE", "DBUS_SESSION_BUS_ADDRESS", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY", "GH_TOKEN", "COPILOT_GITHUB_TOKEN"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s is set", k)
		}
	}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "AIQ_") && k != "AIQ_DATA_DIR" && !testSwitch(k) || strings.HasPrefix(k, "GIT_") {
			t.Errorf("%s is set", k)
		}
	}
}

// A test binary run again by a test keeps the environment the test gave it.
func TestARerunKeepsItsParentsEnvironment(t *testing.T) {
	if os.Getenv("TESTENV_CHILD_HELPER") == "1" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, tmp := t.TempDir(), t.TempDir() // the child's own TMPDIR, which no other package's run shares
	cmd := exec.Command(exe, "-test.run=^TestChildReports$", "-test.v")
	cmd.Env = append(os.Environ(), "TESTENV_CHILD_HELPER=1", "HOME="+home, "AIQ_FROM_PARENT=yes", "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, want := range []string{"root=" + os.Getenv(marker) + "\n", "home=" + home + "\n", "parent=yes\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the child said %q, without %q", out, want)
		}
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the child left %d entries in its temporary directory", len(left))
	}
}

func TestChildReports(t *testing.T) {
	if os.Getenv("TESTENV_CHILD_HELPER") != "1" {
		t.Skip("run by TestARerunKeepsItsParentsEnvironment")
	}
	os.Stdout.WriteString("root=" + os.Getenv(marker) + "\nhome=" + os.Getenv("HOME") + "\nparent=" + os.Getenv("AIQ_FROM_PARENT") + "\n")
}

func TestGoCommandHasTheRunnersGoSettings(t *testing.T) {
	cmd := GoCommand("env", "GOMODCACHE")
	home := ""
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME":
			home = v
		case strings.HasPrefix(k, "AIQ_") || strings.HasPrefix(k, "TMUX") || k == marker || k == "CODEX_HOME" || k == "CLAUDE_CONFIG_DIR" ||
			k == "COPILOT_HOME" || k == "QUOTA_HOME" || k == "DBUS_SESSION_BUS_ADDRESS" || k == "ANTHROPIC_API_KEY" || k == "GH_TOKEN":
			t.Errorf("the go command gets %s", k)
		}
	}
	if under(home, os.Getenv(marker)) {
		t.Errorf("the go command's HOME is %q", home)
	}
	if out, err := cmd.Output(); err != nil || under(strings.TrimSpace(string(out)), os.Getenv(marker)) {
		t.Errorf("go env GOMODCACHE: %q, %v", out, err)
	}
}

// tmux finds no server: the runner's default one is out of reach.
func TestTmuxReachesNoServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	if os.Getenv("TMUX") != "" || !inherited(os.Getenv(marker)) || !strings.HasPrefix(filepath.Base(os.Getenv("TMUX_TMPDIR")), "aiqt") {
		t.Fatal("the environment is not set up")
	}
	dir := os.Getenv("TMUX_TMPDIR")
	real, _ := filepath.EvalSymlinks(dir) // tmux names the resolved path
	out, err := exec.Command("tmux", "list-sessions").CombinedOutput()
	if err == nil || !strings.Contains(string(out), dir) && (real == "" || !strings.Contains(string(out), real)) {
		t.Errorf("tmux list-sessions did not fail on the temporary socket (%d bytes of output, %v)", len(out), err)
	}
}

func TestTheKeychainIsStubbedOnMacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	for _, name := range []string{"security", "launchctl"} {
		if p, err := exec.LookPath(name); err != nil || !under(p, os.Getenv(marker)) {
			t.Errorf("%s resolves to %q, %v", name, p, err)
		}
	}
}
