package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMergePATHKeepsOrderAndAddsLoginEntries(t *testing.T) {
	got := mergePATH("/usr/bin:/home/u/.local/bin", "/home/u/.local/share/aiq/shims:/home/u/.nvm/bin:/usr/bin")
	want := "/usr/bin:/home/u/.local/bin:/home/u/.local/share/aiq/shims:/home/u/.nvm/bin"
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if mergePATH("/a", "") != "/a" {
		t.Fatal("an empty login PATH must leave the current one")
	}
}

func TestLoginShellPATHIgnoresBackgroundChildHoldingStdout(t *testing.T) {
	// A shell whose rc files leave a background process on stdout, as an
	// agent started from .zshrc does.
	sh := filepath.Join(t.TempDir(), "sh")
	script := "#!/bin/sh\nsleep 60 &\nprintf '\\n__AIQ_PATH__/opt/x/bin:/usr/bin__AIQ_PATH__\\n'\n"
	if err := os.WriteFile(sh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", sh)
	start := time.Now()
	got := loginShellPATH()
	if got != "/opt/x/bin:/usr/bin" {
		t.Errorf("PATH = %q", got)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %s: waited on the background child's pipe", d)
	}
}
