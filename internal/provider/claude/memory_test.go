package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSlugMatchesClaudeCode(t *testing.T) {
	cases := map[string]string{
		"/home/a.b/src/proj":               "-home-a-b-src-proj",
		"/tmp/café":                        "-tmp-caf-",
		"/x/\U0001F600":                    "-x---",
		"":                                 "",
		"/" + strings.Repeat("a", maxSlug): "",
	}
	for in, want := range cases {
		if got := projectSlug(in); got != want {
			t.Errorf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectRootUsesMainCheckoutForWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(base, "main")
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(main, "sub"), 0o755)
	run(main, "init", "-q")
	run(main, "commit", "-q", "--allow-empty", "-m", "x")
	run(main, "worktree", "add", "-q", filepath.Join(base, "wt"))
	for _, dir := range []string{main, filepath.Join(main, "sub"), filepath.Join(base, "wt")} {
		if got := projectRoot(dir); got != main {
			t.Errorf("projectRoot(%s) = %s, want %s", dir, got, main)
		}
	}
	plain := filepath.Join(base, "plain")
	os.Mkdir(plain, 0o755)
	if got := projectRoot(plain); got != plain {
		t.Errorf("projectRoot(non-git) = %s, want %s", got, plain)
	}
}

func TestLaunchSettingsCombinesHooksAndMemory(t *testing.T) {
	if LaunchSettings("", "") != "" {
		t.Fatal("empty settings should be empty")
	}
	var doc struct {
		AutoMemoryDirectory string                     `json:"autoMemoryDirectory"`
		Hooks               map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(LaunchSettings("/bin/aiq", `/m "q"/memory`)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.AutoMemoryDirectory != `/m "q"/memory` || len(doc.Hooks) != 3 {
		t.Fatalf("got %+v", doc)
	}
	if HookSettings("/bin/aiq") != LaunchSettings("/bin/aiq", "") {
		t.Fatal("HookSettings changed")
	}
}
