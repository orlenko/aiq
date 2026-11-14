package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/orlenko/aiq/internal/state"
)

func TestNavNoteMetadataCountsNamesWithoutOpeningContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "0123456789abcdef.md")
	for _, name := range []string{"0123456789abcdef.md", "0123456789abcdef.20300101-010203.md", "0123456789abcdef.20300102-010203.md", "0123456789abcdef.invalid.md", "fedcba9876543210.20300101-010203.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("INVENTED-NOTE-CONTENT-NEVER-READ"), 0000); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "0123456789abcdef.20300103-010203.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(path, filepath.Join(dir, "0123456789abcdef.20300104-010203.md")); err != nil {
			t.Fatal(err)
		}
	}
	limits := state.NavigatorSnapshotLimits{Rows: 100, TextBytes: 16384}
	body, err := navNoteMetadataAt(context.Background(), path, limits)
	if err != nil || !body.Available || !body.Exists || !body.Updated.Known || body.Archived != 2 {
		t.Fatal("incorrect note metadata", body, err)
	}
	for _, limit := range []state.NavigatorSnapshotLimits{{Rows: 1, TextBytes: 16384}, {Rows: 100, TextBytes: 1}} {
		if _, err := navNoteMetadataAt(context.Background(), path, limit); err != state.ErrNavigatorSnapshotTooLarge {
			t.Fatal("directory admission bypassed", err)
		}
	}
	missing, err := navNoteMetadataAt(context.Background(), filepath.Join(dir, "missing", "0123456789abcdef.md"), limits)
	if err != nil || !missing.Available || missing.Exists || missing.Archived != 0 {
		t.Fatal("missing metadata not known absent", missing, err)
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link.md")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if v, err := navNoteMetadataAt(context.Background(), link, limits); err != nil || v.Available {
			t.Fatal("followed current-note symlink", v, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := navNoteMetadataAt(ctx, path, limits); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestNavWorkspaceProbeUsesOnlyFixedReadOnlyCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owned POSIX command fixture")
	}
	dir := t.TempDir()
	git := filepath.Join(dir, "git")
	// The only executed program is this invented fixture, never the user's git.
	script := "#!/bin/sh\n[ \"$#\" = 7 ] || exit 2\n[ \"$1\" = --no-optional-locks ] || exit 2\n[ \"$2\" = -c ] && [ \"$3\" = core.fsmonitor=false ] || exit 2\n[ \"$4\" = -c ] && [ \"$5\" = core.hooksPath=/dev/null ] || exit 2\n[ \"$6\" = rev-parse ] && [ \"$7\" = --show-toplevel ] || exit 2\nprintf '%s\\n' \"$PWD\"\n"
	if err := os.WriteFile(git, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	root, ok := navWorkspaceAt(context.Background(), dir)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || root != canonical {
		t.Fatal("fixed command not used", root, ok)
	}
	if err := os.WriteFile(git, []byte("#!/bin/sh\nexit 128\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if root, ok := navWorkspaceAt(context.Background(), dir); !ok || root != filepath.Clean(dir) {
		t.Fatal("non-repository fallback changed", root, ok)
	}
	for _, output := range []string{"relative", strings.Repeat("x", 8193)} {
		if err := os.WriteFile(git, []byte("#!/bin/sh\nprintf '%s' '"+output+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, ok := navWorkspaceAt(context.Background(), dir); ok {
			t.Fatal("invalid command output became workspace")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, ok := navWorkspaceAt(ctx, dir); ok || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("cancelled probe accepted")
	}
}
