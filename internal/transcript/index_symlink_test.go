package transcript

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCodexIndexRefreshesReplacedSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, dir := range []string{a, b} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	a, b = canonical(a), canonical(b)
	// The target is outside the discovery root, so only the link is indexed.
	target := filepath.Join(root, "target")
	discovery := filepath.Join(root, "rollouts")
	if err := os.Mkdir(discovery, 0o700); err != nil {
		t.Fatal(err)
	}
	writeLines(t, target, codexFixture(a, idA)...)
	path := filepath.Join(discovery, "rollout-"+idA+".jsonl")
	if err := os.Symlink(target, path); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	r := Roots{CodexSessions: []string{discovery}}
	x, clk := testIndex(r)
	ctx := context.Background()
	check := func(dir string, candidate, written bool) {
		t.Helper()
		var wantCandidates, wantWritten []string
		if candidate {
			wantCandidates = []string{path}
		}
		if written {
			wantWritten = []string{path}
		}
		for _, src := range []Discovery{x, DirectWalk(r)} {
			candidates, err := src.Candidates(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, c := range candidates {
				paths = append(paths, c.Path)
			}
			if !reflect.DeepEqual(paths, wantCandidates) {
				t.Fatalf("%T Candidates in %q: %v want %v", src, dir, paths, wantCandidates)
			}
			if got := src.Written(ctx, "codex", dir, time.Time{}); !slices.Equal(got, wantWritten) {
				t.Fatalf("%T Written in %q: %v want %v", src, dir, got, wantWritten)
			}
		}
	}
	check(a, true, true)
	check(b, false, false)
	for _, replacement := range []struct{ cwd, source string }{{b, "exec"}, {a, "cli"}} {
		before, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		lines := codexFixture(replacement.cwd, idA)
		lines[0] = strings.ReplaceAll(lines[0], `"source":"cli"`, `"source":"`+replacement.source+`"`)
		fresh := filepath.Join(root, "replacement")
		writeLines(t, fresh, lines...)
		if err := os.Rename(fresh, target); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(target)
		if err != nil || os.SameFile(before, after) {
			t.Fatal("target was not replaced with a different file:", err)
		}
		recoverySameInode(t, linkInfo, path)
		for i := 0; i < 2; i++ {
			recoveryRefresh(t, x, clk, replacement.cwd)
			check(a, replacement.cwd == a, replacement.cwd == a)
			check(b, replacement.cwd == b, false)
		}
	}
	parked := filepath.Join(root, "parked")
	if err := os.Rename(target, parked); err != nil {
		t.Fatal(err)
	}
	recoveryRefresh(t, x, clk, a)
	check(a, false, false)
	check(b, false, false)
	if err := os.Rename(parked, target); err != nil {
		t.Fatal(err)
	}
	recoverySameInode(t, linkInfo, path)
	recoveryRefresh(t, x, clk, a)
	check(a, true, true)
	check(b, false, false)
}
