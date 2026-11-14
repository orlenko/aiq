package claude

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/paths"
)

func TestProjectSlugMatchesClaudeCode(t *testing.T) {
	long := "/" + strings.Repeat("a", maxSlug-1)
	cases := map[string]string{
		"/home/a.b/src/proj": "-home-a-b-src-proj",
		`C:\src\proj`:        "C--src-proj",
		long:                 "-" + strings.Repeat("a", maxSlug-1),
		long + "a":           "", // past maxSlug Claude Code adds a hash
		"/tmp/café":          "", // Claude Code normalizes non-ASCII first
		"/x/\U0001F600":      "",
		"":                   "",
	}
	for in, want := range cases {
		if got := projectSlug(in); got != want {
			t.Errorf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func needGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

func tempDir(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRootUsesMainCheckoutForWorktrees(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	main := filepath.Join(base, "main")
	os.MkdirAll(filepath.Join(main, "sub"), 0o755)
	git(t, main, "init", "-q")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "x")
	git(t, main, "worktree", "add", "-q", filepath.Join(base, "wt"))
	os.MkdirAll(filepath.Join(base, "wt", "deep"), 0o755)
	for _, dir := range []string{main, filepath.Join(main, "sub"), filepath.Join(base, "wt"), filepath.Join(base, "wt", "deep")} {
		if got := projectRoot(dir); got != main {
			t.Errorf("projectRoot(%s) = %s, want %s", dir, got, main)
		}
	}
	// git's environment does not steer it: Claude Code reads the files.
	t.Setenv("GIT_DIR", filepath.Join(main, ".git"))
	plain := filepath.Join(base, "plain")
	os.MkdirAll(filepath.Join(plain, "sub"), 0o755)
	if got := projectRoot(filepath.Join(plain, "sub")); got != filepath.Join(plain, "sub") {
		t.Errorf("projectRoot(non-git) = %s, want the dir itself", got)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(main, "sub"), link); err != nil {
		t.Fatal(err)
	}
	if got := projectRoot(link); got != main {
		t.Errorf("projectRoot(symlink into repo) = %s, want %s", got, main)
	}
}

func TestProjectRootOfABareRepositorysWorktreeIsTheRepository(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	src := filepath.Join(base, "src")
	os.Mkdir(src, 0o755)
	git(t, src, "init", "-q")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "x")
	git(t, base, "clone", "-q", "--bare", src, "proj.git")
	git(t, filepath.Join(base, "proj.git"), "worktree", "add", "-q", filepath.Join(base, "wt"))
	if got, want := projectRoot(filepath.Join(base, "wt")), filepath.Join(base, "proj.git"); got != want {
		t.Errorf("projectRoot(bare worktree) = %s, want %s", got, want)
	}
}

// Content that did not come from git (an archive, a package) can carry a
// .git that points elsewhere. None of these may send a session's memory to
// another project's folder: each keys on the directory itself, as Claude
// Code does.
func TestProjectRootIgnoresPlantedGitPointers(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	victim := filepath.Join(base, "victim")
	os.Mkdir(victim, 0o755)
	git(t, victim, "init", "-q")
	git(t, victim, "commit", "-q", "--allow-empty", "-m", "x")
	git(t, victim, "worktree", "add", "-q", filepath.Join(base, "realwt"))

	gitFile := filepath.Join(base, "a")
	write(t, filepath.Join(gitFile, ".git"), "gitdir: ../victim/.git\n")

	commonDir := filepath.Join(base, "b")
	os.MkdirAll(filepath.Join(commonDir, ".git"), 0o755)
	write(t, filepath.Join(commonDir, ".git", "commondir"), "../../victim/.git\n")

	worktree := filepath.Join(base, "c")
	write(t, filepath.Join(worktree, ".git"), "gitdir: meta\n")
	write(t, filepath.Join(worktree, "meta", "config"), "[core]\n\tbare = false\n\tworktree = ../../victim\n")
	write(t, filepath.Join(worktree, "meta", "HEAD"), "ref: refs/heads/main\n")

	// A gitdir inside the victim's worktrees whose back-link names another
	// checkout: the planted .git borrows a real worktree entry.
	borrow := filepath.Join(base, "d")
	entries, _ := os.ReadDir(filepath.Join(victim, ".git", "worktrees"))
	if len(entries) != 1 {
		t.Fatalf("want one worktree entry, got %d", len(entries))
	}
	write(t, filepath.Join(borrow, ".git"), "gitdir: "+filepath.Join(victim, ".git", "worktrees", entries[0].Name())+"\n")

	// A gitdir shipped with the content, back-link and all, whose commondir
	// names the victim: it is not inside the victim's worktrees.
	shipped := filepath.Join(base, "f")
	write(t, filepath.Join(shipped, ".git"), "gitdir: meta/wt\n")
	write(t, filepath.Join(shipped, "meta", "wt", "commondir"), "../../../victim/.git\n")
	write(t, filepath.Join(shipped, "meta", "wt", "gitdir"), filepath.Join(shipped, ".git")+"\n")

	for _, dir := range []string{gitFile, commonDir, worktree, borrow, shipped} {
		if got := projectRoot(dir); got != dir {
			t.Errorf("projectRoot(%s) = %s, want the dir itself", dir, got)
		}
		if got := OverlayMemoryDir(dir); strings.Contains(got, projectSlug(victim)+string(filepath.Separator)) {
			t.Errorf("OverlayMemoryDir(%s) = %s, the victim's folder", dir, got)
		}
	}

	linked := filepath.Join(base, "e")
	os.Mkdir(linked, 0o755)
	if err := os.Symlink(filepath.Join(victim, ".git"), filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := projectRoot(linked); got != "" {
		t.Errorf("projectRoot(symlinked .git) = %q, want empty", got)
	}
}

func TestProjectRootRefusesARelativeDir(t *testing.T) {
	for _, dir := range []string{"", ".", "rel/dir"} {
		if got := projectRoot(dir); got != "" {
			t.Errorf("projectRoot(%q) = %q, want empty", dir, got)
		}
		if got := OverlayMemoryDir(dir); got != "" {
			t.Errorf("OverlayMemoryDir(%q) = %q, want empty", dir, got)
		}
	}
}

func TestOverlayMemoryDirIsTheProjectFolderUnderTheRealHome(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	os.Mkdir(dir, 0o755)
	want := filepath.Join(paths.RealClaudeHome(), "projects", projectSlug(dir), "memory")
	if got := OverlayMemoryDir(dir); got != want {
		t.Fatalf("OverlayMemoryDir = %q, want %q", got, want)
	}
}

func TestOverlayMemoryDirLeavesAChosenDirectoryAlone(t *testing.T) {
	needGit(t)
	root := filepath.Join(tempDir(t), "proj")
	sub := filepath.Join(root, "sub")
	os.MkdirAll(sub, 0o755)
	git(t, root, "init", "-q")
	if OverlayMemoryDir(sub) == "" {
		t.Fatal("no memory dir with nothing chosen")
	}
	user := filepath.Join(paths.RealClaudeHome(), "settings.json")
	cases := map[string]string{
		user: `{"autoMemoryDirectory":"~/m"}`,
		filepath.Join(root, ".claude", "settings.json"):       `{"autoMemoryDirectory":"~/m"}`,
		filepath.Join(root, ".claude", "settings.local.json"): `{"autoMemoryDirectory":"~/m"}`,
		filepath.Join(sub, ".claude", "settings.local.json"):  `{"autoMemoryDirectory":"~/m"}`,
		filepath.Join(sub, ".claude", "settings.json"):        `{not json`,
	}
	for file, data := range cases {
		write(t, file, data)
		if got := OverlayMemoryDir(sub); got != "" {
			t.Errorf("%s = %s: got %q, want empty", file, data, got)
		}
		os.Remove(file)
	}
	write(t, user, `{"model":"x"}`)
	if OverlayMemoryDir(sub) == "" {
		t.Error("settings without the key should not stop it")
	}
	for _, env := range []string{"CLAUDE_CODE_REMOTE_MEMORY_DIR", "CLAUDE_COWORK_MEMORY_PATH_OVERRIDE"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "/elsewhere")
			if got := OverlayMemoryDir(sub); got != "" {
				t.Errorf("got %q, want empty", got)
			}
		})
	}
}

func TestWithMemoryDir(t *testing.T) {
	const mem = `/r/projects/p/memory`
	own := LaunchSettings("", mem)
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"none", []string{"-p", "hi"}, []string{"--settings", own, "-p", "hi"}},
		{"after --", []string{"-p", "--", "--settings", "f.json"}, []string{"--settings", own, "-p", "--", "--settings", "f.json"}},
		{"file", []string{"--settings", "f.json", "-p"}, []string{"--settings", "f.json", "-p"}},
		{"file=", []string{"--settings=f.json"}, []string{"--settings=f.json"}},
		{"has key", []string{"--settings", `{"autoMemoryDirectory":"~/x"}`}, []string{"--settings", `{"autoMemoryDirectory":"~/x"}`}},
		{"inline", []string{"--settings", `{"hooks":{}}`, "--session-id", "s"}, []string{"--settings", `{"autoMemoryDirectory":"` + mem + `","hooks":{}}`, "--session-id", "s"}},
		{"inline=", []string{`--settings={"a":1}`}, []string{`--settings={"a":1,"autoMemoryDirectory":"` + mem + `"}`}},
		{"last wins", []string{"--settings", `{"a":1}`, "--settings", "f.json"}, []string{"--settings", `{"a":1}`, "--settings", "f.json"}},
		{"no value", []string{"--settings"}, []string{"--settings"}},
	}
	for _, c := range cases {
		in := append([]string{}, c.in...)
		got, file := WithMemoryDir(in, mem)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if wantFile := strings.HasPrefix(c.name, "file") || c.name == "last wins"; file != wantFile {
			t.Errorf("%s: file = %v, want %v", c.name, file, wantFile)
		}
		if !reflect.DeepEqual(in, c.in) {
			t.Errorf("%s: input changed to %q", c.name, in)
		}
	}
	if got, _ := WithMemoryDir([]string{"x"}, ""); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("empty: %q", got)
	}
}

func TestLaunchSettingsCombinesHooksAndMemory(t *testing.T) {
	if LaunchSettings("", "") != "" {
		t.Fatal("empty settings should be empty")
	}
	var doc struct {
		AutoMemoryDirectory string `json:"autoMemoryDirectory"`
		Hooks               map[string][]struct {
			Hooks []struct{ Command string }
		} `json:"hooks"`
	}
	bin := "/bin/a\x01q é"
	if err := json.Unmarshal([]byte(LaunchSettings(bin, `/m "q"/memory`)), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.AutoMemoryDirectory != `/m "q"/memory` || len(doc.Hooks) != 3 || doc.Hooks["Stop"][0].Hooks[0].Command != bin+" claude-hook stop" {
		t.Fatalf("got %+v", doc)
	}
	if HookSettings("/bin/aiq") != LaunchSettings("/bin/aiq", "") {
		t.Fatal("HookSettings changed")
	}
}

// A settings file in an untrusted project that is a FIFO (or a device, or a
// directory) must not hang the launch: it counts as choosing.
func TestOverlayMemoryDirDoesNotReadSpecialSettingsFiles(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	if err := syscallMkfifo(filepath.Join(dir, ".claude", "settings.json")); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	done := make(chan string, 1)
	go func() { done <- OverlayMemoryDir(dir) }()
	select {
	case got := <-done:
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OverlayMemoryDir blocked on a FIFO")
	}
	os.Remove(filepath.Join(dir, ".claude", "settings.json"))
	os.Mkdir(filepath.Join(dir, ".claude", "settings.json"), 0o755)
	if got := OverlayMemoryDir(dir); got != "" {
		t.Errorf("settings.json as a directory: got %q, want empty", got)
	}
}

func TestProjectRootIsNeverTheFileSystemRoot(t *testing.T) {
	root := string(filepath.Separator)
	if v := filepath.VolumeName(os.TempDir()); v != "" {
		root = v + root
	}
	if got := projectRoot(root); got != "" {
		t.Errorf("projectRoot(%q) = %q, want empty", root, got)
	}
}

func TestOverlayMemoryDirLeavesAHostsProjectNameAlone(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	os.Mkdir(dir, 0o755)
	for _, env := range []string{"CLAUDE_CODE_PROJECT_DIR_NAME", "CLAUDE_CODE_USE_COWORK_PLUGINS"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "x")
			if got := OverlayMemoryDir(dir); got != "" {
				t.Errorf("got %q, want empty", got)
			}
		})
	}
}

// git keeps a worktree's back-link as it was typed. On a disk that ignores
// case, a back-link in another case still names this worktree.
func TestProjectRootMatchesAMiscasedWorktreeBackLink(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	if _, err := os.Stat(strings.ToUpper(base)); err != nil {
		t.Skip("the disk does not ignore case")
	}
	main := filepath.Join(base, "main")
	os.Mkdir(main, 0o755)
	git(t, main, "init", "-q")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(base, "wtcase")
	git(t, main, "worktree", "add", "-q", wt)
	entries, _ := os.ReadDir(filepath.Join(main, ".git", "worktrees"))
	if len(entries) != 1 {
		t.Fatalf("want one worktree entry, got %d", len(entries))
	}
	write(t, filepath.Join(main, ".git", "worktrees", entries[0].Name(), "gitdir"), filepath.Join(base, "WTCASE", ".git")+"\n")
	if got := projectRoot(wt); got != main {
		t.Errorf("projectRoot(miscased worktree) = %s, want %s", got, main)
	}
}

func TestWithMemoryDirSkipsSubcommandsAndFlagValues(t *testing.T) {
	const mem = `/r/projects/p/memory`
	own := LaunchSettings("", mem)
	for _, in := range [][]string{
		{"agents", "--settings", `{"a":1}`},
		{"agents"},
		{"--model", "opus", "mcp", "list"},
		{"purge", "."},
	} {
		if got, _ := WithMemoryDir(in, mem); !reflect.DeepEqual(got, in) {
			t.Errorf("%q: got %q, want it unchanged", in, got)
		}
	}
	in := []string{"--append-system-prompt", "--settings", `{}`}
	want := append([]string{"--settings", own}, in...)
	if got, _ := WithMemoryDir(in, mem); !reflect.DeepEqual(got, want) {
		t.Errorf("a flag's value read as --settings: got %q", got)
	}
	in = []string{"--settings", `{"autoMemoryDirectory":null}`}
	if got, _ := WithMemoryDir(in, mem); len(got) != 2 || !strings.Contains(got[1], `"autoMemoryDirectory":"`+mem+`"`) {
		t.Errorf("null counts as unset: got %q", got)
	}
}

func TestOverlayMemoryDirReadsEmptyAndNullSettingsAsUnset(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	for _, data := range []string{"", " \n", `{"autoMemoryDirectory":null}`} {
		write(t, filepath.Join(dir, ".claude", "settings.json"), data)
		if OverlayMemoryDir(dir) == "" {
			t.Errorf("settings %q stopped it", data)
		}
	}
}

func TestOverlayMemoryDirTreatsUnreadableAsChosen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	dir := filepath.Join(tempDir(t), "proj")
	f := filepath.Join(dir, ".claude", "settings.json")
	write(t, f, `{}`)
	os.Chmod(f, 0)
	defer os.Chmod(f, 0o644)
	if got := OverlayMemoryDir(dir); got != "" {
		t.Errorf("unreadable settings: got %q, want empty", got)
	}
	locked := filepath.Join(tempDir(t), "locked")
	os.Mkdir(locked, 0o755)
	os.Chmod(locked, 0o600) // no search: its .git cannot be looked up
	defer os.Chmod(locked, 0o755)
	if got := projectRoot(locked); got != "" {
		t.Errorf("unsearchable dir: got %q, want empty", got)
	}
}

func TestWorkingDirIsTheKernelsNotPWD(t *testing.T) {
	real := filepath.Join(tempDir(t), "real")
	os.Mkdir(real, 0o755)
	link := filepath.Join(filepath.Dir(real), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link)
	t.Setenv("PWD", link)
	if got := WorkingDir(); got != real {
		t.Errorf("WorkingDir = %q, want %q", got, real)
	}
}

// A pointer file that is a symlink, or sits under a path with a backslash,
// makes the worktree key on itself, as Claude Code does. One with a line
// break or over 4 KiB, which only aiq refuses, gives no folder.
func TestWorktreeMainRefusesOddPointers(t *testing.T) {
	needGit(t)
	setup := func(t *testing.T, parent string) (main, wt, entry string) {
		main, wt = filepath.Join(parent, "main"), filepath.Join(parent, "wt")
		os.MkdirAll(main, 0o755)
		git(t, main, "init", "-q")
		git(t, main, "commit", "-q", "--allow-empty", "-m", "x")
		git(t, main, "worktree", "add", "-q", wt)
		entries, _ := os.ReadDir(filepath.Join(main, ".git", "worktrees"))
		if len(entries) != 1 {
			t.Fatalf("want one worktree entry, got %d", len(entries))
		}
		entry = filepath.Join(main, ".git", "worktrees", entries[0].Name())
		return
	}
	if main, wt, _ := setup(t, tempDir(t)); projectRoot(wt) != main {
		t.Fatalf("untouched worktree: %s, want %s", projectRoot(wt), main)
	}
	// What Claude Code refuses too keys the worktree on itself; what only aiq
	// refuses gives no folder.
	for _, c := range []struct {
		name, file, text string
		root             bool // true: the worktree itself; false: no folder
	}{
		{"empty .git file", ".git", "", true},
		{"empty commondir", "commondir", "", true},
		{"empty back-link", "gitdir", "", true},
		{"line break in .git", ".git", "gitdir: a\nb\n", false},
		{"line break in back-link", "gitdir", "a\nb\n", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, wt, entry := setup(t, tempDir(t))
			dir := entry
			if c.file == ".git" {
				dir = wt
			}
			write(t, filepath.Join(dir, c.file), c.text)
			want := ""
			if c.root {
				want = wt
			}
			if got := projectRoot(wt); got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
	t.Run("unreadable commondir", func(t *testing.T) {
		_, wt, entry := setup(t, tempDir(t))
		c := filepath.Join(entry, "commondir")
		if err := os.Chmod(c, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(c, 0o644)
		if f, err := os.Open(c); err == nil {
			f.Close()
			t.Skip("the file stays readable here (running as root?)")
		}
		if got := projectRoot(wt); got != wt {
			t.Errorf("got %q, want %s", got, wt)
		}
	})
	t.Run("symlinked commondir", func(t *testing.T) {
		_, wt, entry := setup(t, tempDir(t))
		c := filepath.Join(entry, "commondir")
		data, _ := os.ReadFile(c)
		write(t, c+".real", string(data))
		os.Remove(c)
		os.Symlink(c+".real", c)
		if got := projectRoot(wt); got != wt {
			t.Errorf("got %s, want %s", got, wt)
		}
	})
	t.Run("line break", func(t *testing.T) {
		_, wt, entry := setup(t, tempDir(t))
		// Claude Code might follow it; aiq does not, so it names no folder.
		write(t, filepath.Join(entry, "commondir"), "../..\nextra\n")
		if got := projectRoot(wt); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		_, wt, entry := setup(t, tempDir(t))
		write(t, filepath.Join(entry, "commondir"), "../.."+strings.Repeat(" ", 5000)+"\n")
		if got := projectRoot(wt); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("backslash", func(t *testing.T) {
		if filepath.Separator == '\\' {
			t.Skip("a separator here")
		}
		parent := filepath.Join(tempDir(t), `a\b`)
		_, wt, _ := setup(t, parent)
		if got := projectRoot(wt); got != wt {
			t.Errorf("got %s, want %s", got, wt)
		}
	})
}

func TestBareRepositoryWithAGitEntryKeysOnTheWorktree(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	src := filepath.Join(base, "src")
	os.Mkdir(src, 0o755)
	git(t, src, "init", "-q")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "x")
	git(t, base, "clone", "-q", "--bare", src, "proj.git")
	bare, wt := filepath.Join(base, "proj.git"), filepath.Join(base, "wt")
	git(t, bare, "worktree", "add", "-q", wt)
	os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(bare, ".git"))
	if got := projectRoot(wt); got != bare {
		t.Errorf("dangling .git link in the bare repo: got %s, want %s", got, bare)
	}
	os.Remove(filepath.Join(bare, ".git"))
	os.Mkdir(filepath.Join(bare, ".git"), 0o755)
	if got := projectRoot(wt); got != wt {
		t.Errorf(".git inside the bare repo: got %s, want %s", got, wt)
	}
}

func TestWithMemoryDirReportsOnlyAFileAsAFile(t *testing.T) {
	for value, want := range map[string]bool{"f.json": true, "null": false, "[1]": false, `{"autoMemoryDirectory":"~/x"}`: false} {
		if _, file := WithMemoryDir([]string{"--settings", value}, "/m"); file != want {
			t.Errorf("--settings %s: file = %v, want %v", value, file, want)
		}
	}
}

// A hard link to another worktree's .git file does not make this directory
// that worktree: Claude Code compares paths, and so does aiq.
func TestProjectRootIgnoresAHardLinkedWorktreeGitFile(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	victim := filepath.Join(base, "victim")
	os.Mkdir(victim, 0o755)
	git(t, victim, "init", "-q")
	git(t, victim, "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(base, "wt")
	git(t, victim, "worktree", "add", "-q", wt)
	copy := filepath.Join(base, "copy")
	os.Mkdir(copy, 0o755)
	if err := os.Link(filepath.Join(wt, ".git"), filepath.Join(copy, ".git")); err != nil {
		t.Skip("no hard links here:", err)
	}
	if got := projectRoot(copy); got != copy {
		t.Errorf("projectRoot(hard-linked .git) = %s, want %s", got, copy)
	}
	if got := projectRoot(wt); got != victim {
		t.Errorf("projectRoot(the real worktree) = %s, want %s", got, victim)
	}
}

func TestWithMemoryDirKnowsWhichFlagsTakeAValue(t *testing.T) {
	const mem = "/m"
	own := LaunchSettings("", mem)
	for _, c := range []struct{ in, want []string }{
		// --ide takes none, and -w's name may be left out: the --settings
		// after them is the user's own, and gets the key.
		{[]string{"--ide", "--settings", `{"a":1}`}, []string{"--ide", "--settings", `{"a":1,"autoMemoryDirectory":"/m"}`}},
		{[]string{"-w", "--settings", `{"a":1}`}, []string{"-w", "--settings", `{"a":1,"autoMemoryDirectory":"/m"}`}},
		// -w with a name: the name is its value.
		{[]string{"-w", "tree", "--settings", `{"a":1}`}, []string{"-w", "tree", "--settings", `{"a":1,"autoMemoryDirectory":"/m"}`}},
		// A required value is taken even when it reads like a flag.
		{[]string{"--append-system-prompt", "--settings", `{"a":1}`}, []string{"--settings", own, "--append-system-prompt", "--settings", `{"a":1}`}},
	} {
		if got, _ := WithMemoryDir(c.in, mem); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

// openRegular does not wait for a writer on a FIFO, and refuses it.
func TestOpenRegularRefusesAFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(tempDir(t), "fifo")
	if err := syscallMkfifo(fifo); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	done := make(chan error, 1)
	go func() {
		f, err := openRegular(fifo)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO was opened as a settings file")
		}
	case <-time.After(5 * time.Second):
		os.WriteFile(fifo, nil, 0) // let the blocked open return
		t.Fatal("openRegular blocked on a FIFO")
	}
	reg := filepath.Join(filepath.Dir(fifo), "reg")
	write(t, reg, "{}")
	f, err := openRegular(reg)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// A back-link that reaches root/.git through a symlinked directory names it,
// as Claude Code's realpath does.
func TestProjectRootFollowsABackLinkThroughASymlink(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	main := filepath.Join(base, "main")
	os.Mkdir(main, 0o755)
	git(t, main, "init", "-q")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", wt)
	link := filepath.Join(base, "wtlink")
	if err := os.Symlink(wt, link); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(main, ".git", "worktrees"))
	if len(entries) != 1 {
		t.Fatalf("want one worktree entry, got %d", len(entries))
	}
	write(t, filepath.Join(main, ".git", "worktrees", entries[0].Name(), "gitdir"), filepath.Join(link, ".git")+"\n")
	if got := projectRoot(wt); got != main {
		t.Errorf("projectRoot = %s, want %s", got, main)
	}
}

// -d's filter is its value, so the word after it is no subcommand.
func TestFirstPositionalSkipsAnOptionalValue(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"-d", "mcp"}, ""},
		{[]string{"--debug", "api", "mcp"}, "mcp"},
		{[]string{"-d", "--ide", "mcp"}, "mcp"},
		{[]string{"-w", "tree", "agents"}, "agents"},
	} {
		if got := firstPositional(c.in); got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
	if got, _ := WithMemoryDir([]string{"-d", "mcp"}, "/m"); len(got) != 4 || got[0] != "--settings" {
		t.Errorf("-d mcp got no memory folder: %q", got)
	}
}

// From a folder inside a linked worktree, a memory folder chosen in the
// main checkout's settings counts (Claude Code 2.1.322 reads local settings
// at the canonical root), and so does one in the worktree's own folder: aiq
// checks there too, to be safe, though Claude Code reads it only when it is
// the launch folder.
func TestOverlayMemoryDirReadsALinkedWorktreesSettings(t *testing.T) {
	needGit(t)
	base := tempDir(t)
	main := filepath.Join(base, "main")
	os.Mkdir(main, 0o755)
	git(t, main, "init", "-q")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "x")
	wt := filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", wt)
	sub := filepath.Join(wt, "sub")
	os.Mkdir(sub, 0o755)
	if got := OverlayMemoryDir(sub); got == "" {
		t.Fatal("no memory dir with nothing chosen")
	}
	for _, d := range []string{wt, main} {
		for _, name := range []string{"settings.json", "settings.local.json"} {
			file := filepath.Join(d, ".claude", name)
			write(t, file, `{"autoMemoryDirectory":"~/m"}`)
			if got := OverlayMemoryDir(sub); got != "" {
				t.Errorf("%s: got %q, want empty", file, got)
			}
			os.Remove(file)
		}
	}
}

// The env given, not aiq's own, decides: a launcher may set the override.
func TestOverlayMemoryDirInReadsTheGivenEnv(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	os.Mkdir(dir, 0o755)
	if OverlayMemoryDirIn(dir, nil) == "" {
		t.Fatal("no memory dir with an empty env")
	}
	for _, env := range [][]string{
		{"CLAUDE_CODE_PROJECT_DIR_NAME=box"},
		{"CLAUDE_CODE_REMOTE_MEMORY_DIR=", "CLAUDE_CODE_REMOTE_MEMORY_DIR=/r"},
	} {
		if got := OverlayMemoryDirIn(dir, env); got != "" {
			t.Errorf("%q: got %q, want empty", env, got)
		}
	}
	if OverlayMemoryDirIn(dir, []string{"CLAUDE_CODE_PROJECT_DIR_NAME="}) == "" {
		t.Error("an empty value counted as set")
	}
}

// An overlay keeps a freshly rewritten settings.json as its own file, and
// the CLI reads that one, so a choice made there counts.
func TestOverlayMemoryDirReadsTheConfigHomesSettings(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	os.Mkdir(dir, 0o755)
	home := filepath.Join(tempDir(t), "overlay")
	os.Mkdir(home, 0o755)
	env := []string{"CLAUDE_CONFIG_DIR=" + home}
	if OverlayMemoryDirIn(dir, env) == "" {
		t.Fatal("no memory dir with a config home that has no settings")
	}
	os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"theme":"dark"}`), 0o600)
	if OverlayMemoryDirIn(dir, env) == "" {
		t.Fatal("no memory dir with settings that choose nothing")
	}
	os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"autoMemoryDirectory":"/m"}`), 0o600)
	if got := OverlayMemoryDirIn(dir, env); got != "" {
		t.Errorf("the config home's settings chose a folder, got %q", got)
	}
	if OverlayMemoryDirIn(dir, nil) == "" {
		t.Error("another config home's settings were read")
	}
}

func TestMovesProject(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"--project-config-root", "/x"}, true},
		{[]string{"--project-config-root=/x"}, true},
		{[]string{"-p", "hi", "--deep-link-cwd-b64", "L3g"}, true},
		{[]string{"--deep-link-cwd-b64=L3g"}, true},
		{[]string{"-p", "--project-config-rooted"}, false},
		{[]string{"--", "--project-config-root"}, false},
		{nil, false},
	} {
		if got := MovesProject(c.args); got != c.want {
			t.Errorf("%q: got %v, want %v", c.args, got, c.want)
		}
	}
}

// A settings file's env block is applied by Claude Code, so an override set
// there leaves the memory folder alone.
func TestOverlayMemoryDirReadsASettingsEnvBlock(t *testing.T) {
	dir := filepath.Join(tempDir(t), "proj")
	os.Mkdir(dir, 0o755)
	file := filepath.Join(dir, ".claude", "settings.json")
	for data, want := range map[string]bool{
		`{"env":{"CLAUDE_CODE_PROJECT_DIR_NAME":"box"}}`: false,
		`{"env":{"CLAUDE_CODE_REMOTE_MEMORY_DIR":"/r"}}`: false,
		`{"env":"odd"}`: false,
		`{"env":{"CLAUDE_CODE_PROJECT_DIR_NAME":""}}`:   true,
		`{"env":{"CLAUDE_CODE_PROJECT_DIR_NAME":null}}`: true,
		`{"env":{"OTHER":"x"}}`:                         true,
		`{"env":null}`:                                  true,
	} {
		write(t, file, data)
		if got := OverlayMemoryDirIn(dir, nil) != ""; got != want {
			t.Errorf("%s: memory folder set = %v, want %v", data, got, want)
		}
	}
}
