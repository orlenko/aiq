package navtoken

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestNativeWindowsPrivateRegularAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invented-state")
	if err := os.WriteFile(path, []byte(literalOn), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := openWindows(path, nil)
	if err != nil {
		t.Fatal("native unchanged-file open refused")
	}
	owned := f.(*windowsReader)
	metadata, data := owned.metadata, syscall.Handle(owned.File.Fd())
	r, err := readFile(f)
	if err != nil || r.Status() != On || r.PID() != 123 {
		t.Error("native positive control failed")
	}
	for _, handle := range []syscall.Handle{metadata, data} {
		var facts syscall.ByHandleFileInformation
		if syscall.GetFileInformationByHandle(handle, &facts) == nil {
			t.Error("acquired handle remains usable after completed read")
		}
	}
}

func TestNativeWindowsReplacementMismatchBeforeRead(t *testing.T) {
	dir := t.TempDir()
	path, next := filepath.Join(dir, "state"), filepath.Join(dir, "next")
	if os.WriteFile(path, []byte(literalOn), 0600) != nil || os.WriteFile(next, []byte("off 999 0\n"), 0600) != nil {
		t.Fatal("fixture write failed")
	}
	replaced := false
	f, err := openWindows(path, func() {
		if e := os.Rename(next, path); e != nil {
			t.Fatal("actual share-delete replacement failed")
		}
		replaced = true
	})
	if f != nil {
		f.Close()
		t.Error("different data-handle identity admitted")
	}
	if !replaced || err != ErrUnsafe {
		t.Error("native mismatch was not distinguished from acquisition failure")
	}
	r, err := Read(path)
	if err != nil || r.Status() != Off || r.PID() != 999 {
		t.Error("replacement positive control failed")
	}
}

func TestNativeWindowsRejectsFinalLinkAndDirectory(t *testing.T) {
	dir := t.TempDir()
	path, link := filepath.Join(dir, "state"), filepath.Join(dir, "link")
	if e := os.WriteFile(path, []byte(literalOn), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Read(dir); e != ErrUnsafe {
		t.Error("directory admitted")
	}
	if e := os.Symlink(path, link); e != nil {
		t.Skip("native symlink fixture privilege unavailable")
	}
	if _, e := Read(link); e != ErrUnsafe {
		t.Error("final reparse point followed")
	}
}
