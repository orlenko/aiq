package overlay

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var old = time.Now().Add(-time.Minute)

func write(t *testing.T, path, data string, mtime time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, mtime, mtime)
}

func setup(t *testing.T) (real, over string) {
	t.Helper()
	root := t.TempDir()
	real = filepath.Join(root, "real")
	over = filepath.Join(root, "over")
	os.MkdirAll(filepath.Join(real, "projects"), 0o700)
	write(t, filepath.Join(real, "settings.json"), `{"a":1}`, old)
	write(t, filepath.Join(real, "auth.json"), `real-secret`, old)
	write(t, filepath.Join(real, ".DS_Store"), `junk`, old)
	return real, over
}

func TestSyncLinksEverythingButExcluded(t *testing.T) {
	real, over := setup(t)
	spec := Spec{Real: real, Overlay: over, Exclude: []string{"auth.json"}, SkipPrefixes: []string{".DS_Store", ".aiq-"}}
	rep, err := Sync(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Linked) != 2 {
		t.Fatalf("linked %v", rep.Linked)
	}
	for _, name := range []string{"settings.json", "projects"} {
		target, err := os.Readlink(filepath.Join(over, name))
		if err != nil || target != filepath.Join(real, name) {
			t.Fatalf("%s: %v %s", name, err, target)
		}
	}
	if _, err := os.Lstat(filepath.Join(over, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("excluded auth.json must not be linked")
	}
	if _, err := os.Lstat(filepath.Join(over, ".DS_Store")); !os.IsNotExist(err) {
		t.Fatal("skipped entry must not be linked")
	}
	rep, _ = Sync(spec)
	if rep.Changed() {
		t.Fatalf("second sync changed something: %+v", rep)
	}
}

func TestSyncAdoptsNewFileAndKeepsBackups(t *testing.T) {
	real, over := setup(t)
	spec := Spec{Real: real, Overlay: over, Exclude: []string{"auth.json"}, SkipPrefixes: []string{".aiq-"}}
	Sync(spec)
	// The CLI wrote a brand-new file into the overlay.
	write(t, filepath.Join(over, "history.jsonl"), "h", old)
	// The CLI replaced a symlink with a newer real file (rename-over write).
	os.Remove(filepath.Join(over, "settings.json"))
	write(t, filepath.Join(over, "settings.json"), `{"a":2}`, old.Add(30*time.Second))

	rep, err := Sync(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Adopted) != 2 {
		t.Fatalf("adopted %v (warnings %v)", rep.Adopted, rep.Warnings)
	}
	if data, _ := os.ReadFile(filepath.Join(real, "history.jsonl")); string(data) != "h" {
		t.Fatal("new file was not moved into the real home")
	}
	if data, _ := os.ReadFile(filepath.Join(real, "settings.json")); string(data) != `{"a":2}` {
		t.Fatalf("newer overlay copy should replace real: %s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(real, "settings.json.aiq-bak")); string(data) != `{"a":1}` {
		t.Fatalf("displaced real copy must be kept: %q", data)
	}
	for _, name := range []string{"history.jsonl", "settings.json"} {
		if info, err := os.Lstat(filepath.Join(over, name)); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s should be a symlink again", name)
		}
	}

	// An older overlay copy is kept as a backup, and the previous backup rotates.
	os.Remove(filepath.Join(over, "settings.json"))
	write(t, filepath.Join(over, "settings.json"), `old`, old.Add(-time.Hour))
	rep, _ = Sync(spec)
	if len(rep.Replaced) != 1 {
		t.Fatalf("replaced %v", rep.Replaced)
	}
	if data, _ := os.ReadFile(filepath.Join(real, "settings.json")); string(data) != `{"a":2}` {
		t.Fatalf("real copy must survive: %s", data)
	}
	if data, _ := os.ReadFile(filepath.Join(real, "settings.json.aiq-bak")); string(data) != `old` {
		t.Fatalf("older overlay copy must be kept: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(real, "settings.json.aiq-bak.1")); string(data) != `{"a":1}` {
		t.Fatalf("previous backup must rotate: %q", data)
	}
}

// Two overlays each created the same directory before the real home had it.
func TestSyncMergesDirectoriesInsteadOfDeleting(t *testing.T) {
	real, over := setup(t)
	spec := Spec{Real: real, Overlay: over}
	Sync(spec)
	other := filepath.Join(filepath.Dir(real), "over2")
	spec2 := Spec{Real: real, Overlay: other}
	Sync(spec2)

	write(t, filepath.Join(over, "session-env", "a.json"), "A", old)
	write(t, filepath.Join(other, "session-env", "b.json"), "B", old)
	write(t, filepath.Join(other, "session-env", "sub", "c.json"), "C", old)

	if rep, _ := Sync(spec); len(rep.Adopted) != 1 {
		t.Fatalf("first overlay should adopt the dir: %+v", rep)
	}
	rep, _ := Sync(spec2)
	if len(rep.Merged) != 1 || len(rep.Warnings) != 0 {
		t.Fatalf("second overlay should merge: %+v", rep)
	}
	for _, f := range []string{"a.json", "b.json", "sub/c.json"} {
		if _, err := os.Stat(filepath.Join(real, "session-env", f)); err != nil {
			t.Fatalf("%s lost in merge", f)
		}
	}
	if info, err := os.Lstat(filepath.Join(other, "session-env")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("merged dir should be a symlink now")
	}
}

func TestSyncLeavesScratchAndFreshFilesAlone(t *testing.T) {
	real, over := setup(t)
	spec := Spec{Real: real, Overlay: over, Extra: map[string]string{".claude.json": filepath.Join(filepath.Dir(real), ".claude.json")}}
	Sync(spec)
	write(t, filepath.Join(over, ".claude.json.tmp.123.abc"), "x", old)
	write(t, filepath.Join(over, ".claude.json.lock"), "x", old)
	write(t, filepath.Join(over, "fresh.json"), "x", time.Now())
	rep, _ := Sync(spec)
	if rep.Changed() {
		t.Fatalf("scratch/fresh files must not move: %+v", rep)
	}
	for _, name := range []string{".claude.json.tmp.123.abc", ".claude.json.lock", "fresh.json"} {
		if info, err := os.Lstat(filepath.Join(over, name)); err != nil || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s should still be a real file in the overlay", name)
		}
	}
	// An Extra entry created in the overlay is adopted to the Extra target.
	write(t, filepath.Join(over, ".claude.json"), `{}`, old)
	rep, _ = Sync(spec)
	if len(rep.Adopted) != 1 {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(real), ".claude.json")); err != nil {
		t.Fatal("extra should land at its target, not inside the real home")
	}
}

func TestSyncExtraAndDangling(t *testing.T) {
	real, over := setup(t)
	extra := filepath.Join(filepath.Dir(real), ".claude.json")
	write(t, extra, `{}`, old)
	spec := Spec{Real: real, Overlay: over, Extra: map[string]string{".claude.json": extra}}
	Sync(spec)
	if target, _ := os.Readlink(filepath.Join(over, ".claude.json")); target != extra {
		t.Fatalf("extra not linked: %s", target)
	}
	os.Remove(filepath.Join(real, "settings.json"))
	rep, _ := Sync(spec)
	if len(rep.Removed) != 1 || rep.Removed[0] != "settings.json" {
		t.Fatalf("dangling link should be removed: %+v", rep)
	}
}

func TestSeed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	dst := filepath.Join(dir, "dst.json")
	write(t, src, `{"k":1}`, old)
	if err := Seed(src, dst); err != nil {
		t.Fatal(err)
	}
	write(t, src, `{"k":2}`, old)
	Seed(src, dst)
	if data, _ := os.ReadFile(dst); string(data) != `{"k":1}` {
		t.Fatalf("seed must not overwrite: %s", data)
	}
	if err := Seed(filepath.Join(dir, "missing"), filepath.Join(dir, "x")); err != nil {
		t.Fatal("missing source is not an error")
	}
}
