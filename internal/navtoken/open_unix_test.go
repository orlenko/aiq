//go:build unix

package navtoken

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUnixPrivateRegularAndUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invented-state")
	if os.WriteFile(path, []byte(literalOn), 0600) != nil {
		t.Fatal("fixture write failed")
	}
	r, e := Read(path)
	if e != nil || r.Status() != On {
		t.Fatal("private regular control failed")
	}
	if e = os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = Read(path); e != ErrUnsafe {
		t.Error("wide mode accepted")
	}
	if e = os.Chmod(path, 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "invented-link")
	if e = os.Symlink(path, link); e != nil {
		t.Fatal(e)
	}
	if _, e = Read(link); e != ErrUnsafe {
		t.Error("symlink followed")
	}
	if _, e = Read(dir); e != ErrUnsafe {
		t.Error("directory admitted")
	}
	if _, e = Read(filepath.Join(dir, "absent")); e != ErrUnavailable {
		t.Error("missing path not fixed unavailable")
	}
	if e = os.WriteFile(path, []byte(strings.Repeat("x", 129)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Read(path); e != ErrInvalid {
		t.Error("oversize file admitted")
	}
}

func TestUnixOpenedSnapshotDoesNotReopenPath(t *testing.T) {
	dir := t.TempDir()
	path, next := filepath.Join(dir, "state"), filepath.Join(dir, "next")
	if os.WriteFile(path, []byte(literalOn), 0600) != nil || os.WriteFile(next, []byte("off 999 0\n"), 0600) != nil {
		t.Fatal("fixture write failed")
	}
	f, e := openValidated(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(next, path); e != nil {
		f.Close()
		t.Fatal(e)
	}
	r, e := readFile(f)
	if e != nil || r.Status() != On || r.PID() != 123 {
		t.Error("opened descriptor was replaced by path lookup")
	}
	fresh, e := Read(path)
	if e != nil || fresh.Status() != Off || fresh.PID() != 999 {
		t.Error("path replacement fixture failed")
	}
}

func TestUnixForeignOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("foreign-UID fixture requires chown privilege")
	}
	path := filepath.Join(t.TempDir(), "foreign-state")
	if e := os.WriteFile(path, []byte(literalOn), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Chown(path, 65534, -1); e != nil {
		t.Skip("foreign-UID fixture unavailable")
	}
	if _, e := Read(path); e != ErrUnsafe {
		t.Error("foreign owner admitted")
	}
}

func TestUnixFIFOIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if e := syscall.Mkfifo(path, 0600); e != nil {
		t.Fatal(e)
	}
	self, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, self, "-test.run=^TestUnixFIFOChild$")
	command.Env = append(os.Environ(), "AIQ_NAVTOKEN_FIFO_FIXTURE="+path)
	out, e := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal("FIFO child did not finish within bound")
	}
	if e != nil {
		t.Fatalf("FIFO child failed: %s", out)
	}
}
func TestUnixFIFOChild(t *testing.T) {
	path := os.Getenv("AIQ_NAVTOKEN_FIFO_FIXTURE")
	if path == "" {
		t.Skip("subprocess fixture only")
	}
	if _, e := Read(path); e != ErrUnsafe {
		t.Error("FIFO was not refused")
	}
}
