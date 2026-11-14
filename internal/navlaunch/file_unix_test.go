//go:build linux || darwin

package navlaunch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func launchData(t *testing.T) string {
	t.Helper()
	// macOS temporary parents may have a symlink spelling. Resolve only this
	// invented fixture; the production reader never resolves symlinks.
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func launchBody(t *testing.T) []byte {
	t.Helper()
	b, e := Document(12345, "/invented", strings.Repeat("A", 43))
	if e != nil {
		t.Fatal(e)
	}
	return b
}

func TestPrivateFilePublicationAndRetention(t *testing.T) {
	data := launchData(t)
	ctx := context.Background()
	f, e := Prepare(ctx, data)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Finish(false)
	info, e := f.file.Stat()
	if e != nil || info.Size() != 0 || info.Mode().Perm() != 0600 {
		t.Fatal("not exclusively precreated private empty file")
	}
	name, e := f.Publish(ctx, launchBody(t))
	if e != nil {
		t.Fatal(e)
	}
	if f.file != nil || filepath.Dir(name) != filepath.Join(data, "ui-launch") || !canonicalName(filepath.Base(name)) {
		t.Fatal("publication")
	}
	b, e := os.ReadFile(name)
	if e != nil || string(b) != string(launchBody(t)) {
		t.Fatal("not complete before handoff")
	}
	f.Finish(true)
	f.Finish(false)
	if _, e = os.Stat(name); e != nil {
		t.Fatal("asynchronous handoff removed")
	}
	if _, e = os.Stat(filepath.Join(data, "ui-launch", ".lock")); e != nil {
		t.Fatal("stable lock removed")
	}
}

func TestPrivateFileCleanupLeavesReplacement(t *testing.T) {
	for _, kind := range []string{"owned", "replacement", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			data := launchData(t)
			f, e := Prepare(context.Background(), data)
			if e != nil {
				t.Fatal(e)
			}
			name := filepath.Join(data, "ui-launch", f.name)
			if kind != "owned" {
				if e = os.Rename(name, name+".old"); e != nil {
					t.Fatal(e)
				}
				if kind == "replacement" {
					e = os.WriteFile(name, []byte("invented sentinel"), 0600)
				} else {
					e = os.Symlink(name+".old", name)
				}
				if e != nil {
					t.Fatal(e)
				}
			}
			f.Finish(false)
			_, e = os.Lstat(name)
			if kind == "owned" && !os.IsNotExist(e) {
				t.Fatal("owned failure residue")
			}
			if kind != "owned" && e != nil {
				t.Fatal("replacement deleted")
			}
		})
	}
}

func TestPrivateNamespaceRefusals(t *testing.T) {
	for _, kind := range []string{"data-public", "data-symlink", "parent-writable", "launch-public", "launch-symlink", "lock-symlink", "lock-fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := launchData(t)
			data := filepath.Join(root, "data")
			if e := os.Mkdir(data, 0700); e != nil {
				t.Fatal(e)
			}
			dir := filepath.Join(data, "ui-launch")
			switch kind {
			case "data-public":
				os.Chmod(data, 0755)
			case "data-symlink":
				os.Symlink(data, filepath.Join(root, "alias"))
				data = filepath.Join(root, "alias")
			case "parent-writable":
				os.Chmod(root, 0777)
			default:
				os.Mkdir(dir, 0700)
				switch kind {
				case "launch-public":
					os.Chmod(dir, 0755)
				case "launch-symlink":
					os.Rename(dir, dir+"-target")
					os.Symlink(dir+"-target", dir)
				case "lock-symlink":
					os.Symlink(filepath.Join(root, "sentinel"), filepath.Join(dir, ".lock"))
				case "lock-fifo":
					unix.Mkfifo(filepath.Join(dir, ".lock"), 0600)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if f, e := Prepare(ctx, data); e == nil {
				f.Finish(false)
				t.Fatal("unsafe namespace admitted")
			}
		})
	}
}

func TestPrivateFileLockAndBoundedPruning(t *testing.T) {
	ctx := context.Background()
	data := launchData(t)
	f, e := Prepare(ctx, data)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Finish(false)
	wait, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	if other, e := Prepare(wait, data); e == nil {
		other.Finish(false)
		t.Fatal("concurrent creator bypassed held lock")
	}
	dir := filepath.Join(data, "ui-launch")
	for i, kind := range []string{"old", "fresh", "unknown", "public", "symlink", "fifo"} {
		name := filepath.Join(dir, strings.Repeat(string(rune('a'+i)), 64)+".html")
		body := launchBody(t)
		if kind == "unknown" {
			body = []byte("invented unknown")
		}
		switch kind {
		case "symlink":
			e = os.Symlink(filepath.Join(dir, f.name), name)
		case "fifo":
			e = unix.Mkfifo(name, 0600)
		default:
			e = os.WriteFile(name, body, 0600)
		}
		if e != nil {
			t.Fatal(e)
		}
		if kind == "public" {
			os.Chmod(name, 0644)
		}
		if kind != "fresh" && kind != "symlink" && kind != "fifo" {
			old := time.Now().Add(-11 * time.Minute)
			os.Chtimes(name, old, old)
		}
	}
	if _, e = f.dir.Seek(0, 0); e != nil {
		t.Fatal(e)
	}
	if e = f.prune(ctx, time.Now()); e != nil {
		t.Fatal(e)
	}
	for i, kind := range []string{"old", "fresh", "unknown", "public", "symlink", "fifo"} {
		_, e := os.Lstat(filepath.Join(dir, strings.Repeat(string(rune('a'+i)), 64)+".html"))
		if kind == "old" && !os.IsNotExist(e) {
			t.Fatal("known old not pruned")
		}
		if kind != "old" && e != nil {
			t.Fatal("unmanaged/fresh artifact removed")
		}
	}
	f.Finish(false)
	other, e := Prepare(ctx, data)
	if e != nil {
		t.Fatal("lock not released")
	}
	other.Finish(false)
}

func TestPublishRefusesSubstitutedNamespaceAndCancellation(t *testing.T) {
	for _, kind := range []string{"file", "directory", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			data := launchData(t)
			f, e := Prepare(context.Background(), data)
			if e != nil {
				t.Fatal(e)
			}
			defer f.Finish(false)
			dir := filepath.Join(data, "ui-launch")
			switch kind {
			case "file":
				os.Rename(filepath.Join(dir, f.name), filepath.Join(dir, f.name+".old"))
				os.WriteFile(filepath.Join(dir, f.name), []byte("sentinel"), 0600)
			case "directory":
				os.Rename(dir, dir+"-old")
				os.Mkdir(dir, 0700)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			if name, e := f.Publish(ctx, launchBody(t)); e == nil || name != "" {
				t.Fatal("stale/canceled publication")
			}
		})
	}
}
