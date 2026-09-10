package binpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A PATH with a wrapper ahead of the shims, the shims, then the real CLI:
// the shape an IDE terminal produces when it prepends its own shim dir.
func TestResolveStartsAfterShimDir(t *testing.T) {
	root := t.TempDir()
	wrapperDir := filepath.Join(root, "ide")
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "bin")
	writeExec(t, wrapperDir, "codex", "#!/bin/sh\nexec codex \"$@\"\n")
	writeExec(t, shimDir, "codex", "#!/bin/sh\n# AIQ_SHIM\nexec aiq run codex -- \"$@\"\n")
	real := writeExec(t, realDir, "codex", "#!/bin/sh\necho real\n")

	t.Setenv("PATH", strings.Join([]string{wrapperDir, shimDir, realDir}, string(os.PathListSeparator)))
	got, err := Resolve("codex", "", shimDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("wrapper ahead of the shims must not be a candidate: got %s, want %s", got, real)
	}
}

// A wrapper after the shims is the next hop; once excluded by the chain the
// real CLI follows.
func TestResolveWrapperAfterShimDir(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	wrapperDir := filepath.Join(root, "ide")
	realDir := filepath.Join(root, "bin")
	writeExec(t, shimDir, "codex", "#!/bin/sh\n# AIQ_SHIM\nexec aiq run codex -- \"$@\"\n")
	wrapper := writeExec(t, wrapperDir, "codex", "#!/bin/sh\nexec codex \"$@\"\n")
	real := writeExec(t, realDir, "codex", "#!/bin/sh\necho real\n")

	t.Setenv("PATH", strings.Join([]string{shimDir, wrapperDir, realDir}, string(os.PathListSeparator)))
	got, err := Resolve("codex", "", shimDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != wrapper {
		t.Fatalf("first hop after the shims: got %s, want %s", got, wrapper)
	}
	got, err = Resolve("codex", "", shimDir, []string{wrapper})
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("after excluding the wrapper: got %s, want %s", got, real)
	}
}

// Without the shim dir on PATH (aiq invoked directly) the whole PATH is
// walked; a copy of the shim script elsewhere is still skipped.
func TestResolveWithoutShimDirOnPath(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	copyDir := filepath.Join(root, "copies")
	realDir := filepath.Join(root, "bin")
	writeExec(t, copyDir, "codex", "#!/bin/sh\n# AIQ_SHIM copy\nexec aiq run codex -- \"$@\"\n")
	real := writeExec(t, realDir, "codex", "#!/bin/sh\necho real\n")

	t.Setenv("PATH", strings.Join([]string{copyDir, realDir}, string(os.PathListSeparator)))
	got, err := Resolve("codex", "", shimDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("got %s, want %s", got, real)
	}
}

// The shim dir listed twice: the walk starts after the first occurrence.
func TestResolveShimDirRepeated(t *testing.T) {
	root := t.TempDir()
	wrapperDir := filepath.Join(root, "ide")
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "bin")
	writeExec(t, wrapperDir, "codex", "#!/bin/sh\nexec codex \"$@\"\n")
	writeExec(t, shimDir, "codex", "#!/bin/sh\n# AIQ_SHIM\nexec aiq run codex -- \"$@\"\n")
	real := writeExec(t, realDir, "codex", "#!/bin/sh\necho real\n")

	t.Setenv("PATH", strings.Join([]string{wrapperDir, shimDir, realDir, shimDir}, string(os.PathListSeparator)))
	got, err := Resolve("codex", "", shimDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Fatalf("got %s, want %s", got, real)
	}
}

func TestWithoutDir(t *testing.T) {
	sep := string(os.PathListSeparator)
	root := t.TempDir()
	shims := filepath.Join(root, "shims")
	other := filepath.Join(root, "bin")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	// Every occurrence goes, including a trailing-slash spelling of it.
	got := WithoutDir(strings.Join([]string{shims, other, shims + "/"}, sep), shims)
	if got != other {
		t.Fatalf("got %q, want %q", got, other)
	}
	// A PATH without the directory is returned intact.
	if got := WithoutDir(other, shims); got != other {
		t.Fatalf("got %q, want %q", got, other)
	}
	// An empty directory is a no-op, and empty entries are dropped.
	if got := WithoutDir(other+sep, ""); got != other+sep {
		t.Fatalf("empty dir must not rewrite: %q", got)
	}
	// Removing the only entry leaves an empty PATH rather than a stray sep.
	if got := WithoutDir(shims, shims); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
