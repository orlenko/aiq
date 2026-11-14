package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/orlenko/aiq/internal/longrun"
	"github.com/orlenko/aiq/internal/state"
)

type navLongNote struct {
	Available bool        `json:"available"`
	Exists    bool        `json:"exists"`
	Updated   navWireTime `json:"updated"`
	Archived  int         `json:"archived"`
}

type navProbeOutput struct {
	body     []byte
	overflow bool
}

func (b *navProbeOutput) Write(p []byte) (int, error) {
	if len(p) > 8192-len(b.body) {
		b.overflow = true
		return 0, errors.New("workspace output too large")
	}
	b.body = append(b.body, p...)
	return len(p), nil
}

// This is the existing workspaceID rule with a bounded, fixed read-only git
// probe. No shell, hook, fsmonitor, provider command or workspace change runs.
func navWorkspaceAt(parent context.Context, dir string) (string, bool) {
	if parent == nil || parent.Err() != nil || !navCatalogPath(dir) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(parent, 250*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	cmd.WaitDelay = 250 * time.Millisecond
	output := &navProbeOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if ctx.Err() != nil || output.overflow {
		return "", false
	}
	if err == nil {
		root := strings.TrimSpace(string(output.body))
		if navCatalogPath(root) {
			return root, true
		}
		return "", false
	}
	var exit *exec.ExitError
	if !(errors.Is(err, exec.ErrNotFound) || errors.As(err, &exit) && exit.ExitCode() == 128) {
		return "", false
	}
	if info, e := os.Stat(dir); e == nil && info.IsDir() {
		return filepath.Clean(dir), true
	}
	return "", false
}

// Only the fixed current-note name is lstat'ed; archive names are counted.
// Note contents, symlink targets, redaction and archive mutations stay out.
func navProductionNoteMetadata(ctx context.Context, workspace string, limits state.NavigatorSnapshotLimits) (navLongNote, error) {
	return navNoteMetadataAt(ctx, longrun.HandoffPath(workspace), limits)
}
func navNoteMetadataAt(ctx context.Context, path string, limits state.NavigatorSnapshotLimits) (navLongNote, error) {
	var out navLongNote
	if ctx == nil || ctx.Err() != nil || limits.Rows <= 0 || limits.TextBytes <= 0 {
		return out, state.ErrNavigatorSnapshotUnavailable
	}
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return out, nil
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return out, nil
		}
		out.Exists = true
		if value, e := navConversationTime(info.ModTime()); e == nil {
			out.Updated = value
		} else {
			return navLongNote{}, nil
		}
	}
	if ctx.Err() != nil {
		return navLongNote{}, ctx.Err()
	}
	dir, err := os.Open(filepath.Dir(path))
	if os.IsNotExist(err) {
		out.Available = true
		return out, nil
	}
	if err != nil {
		return navLongNote{}, nil
	}
	defer dir.Close()
	prefix := strings.TrimSuffix(filepath.Base(path), ".md") + "."
	for {
		if ctx.Err() != nil {
			return navLongNote{}, ctx.Err()
		}
		entries, e := dir.ReadDir(64)
		for _, entry := range entries {
			cost := int64(64 + len(entry.Name()))
			if limits.Rows == 0 || cost > limits.TextBytes {
				return navLongNote{}, state.ErrNavigatorSnapshotTooLarge
			}
			limits.Rows--
			limits.TextBytes -= cost
			name := entry.Name()
			if !entry.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".md") {
				continue
			}
			stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".md")
			if len(stamp) == 15 {
				if _, err := time.Parse("20060102-150405", stamp); err == nil {
					out.Archived++
				}
			}
		}
		if e == io.EOF {
			out.Available = true
			return out, nil
		}
		if e != nil {
			return navLongNote{}, nil
		}
	}
}
