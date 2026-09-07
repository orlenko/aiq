// Package binpath finds the real provider CLI on PATH, skipping aiq's own
// shims, aiq itself, and any candidate the caller has already tried.
//
// Wrappers that re-exec the bare command name (an IDE's injected shim, a
// user's own script) are handled by the exec chain in cmd/aiq: each hop
// records the path it exec'd, and a re-entry into `aiq run` from the same
// pid resolves the next candidate that is not on that list.
package binpath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns the preferred executable for name. configured (from the
// config file) is used unless it is excluded; otherwise PATH is walked,
// skipping skipDir, aiq itself, aiq's shim scripts and every path in exclude.
func Resolve(name, configured, skipDir string, exclude []string) (string, error) {
	excluded := map[string]bool{}
	for _, e := range exclude {
		excluded[canon(e)] = true
	}
	if configured != "" && !excluded[canon(configured)] {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("configured %s binary %s: %w", name, configured, err)
		}
		return configured, nil
	}
	self, _ := os.Executable()
	selfReal := canon(self)
	skipReal := canon(skipDir)
	seenDir := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		dirReal := canon(dir)
		if seenDir[dirReal] {
			continue
		}
		seenDir[dirReal] = true
		if skipReal != "" && dirReal == skipReal {
			continue
		}
		cand := filepath.Join(dir, name)
		info, err := os.Stat(cand)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		candReal := canon(cand)
		if selfReal != "" && candReal == selfReal {
			continue
		}
		if excluded[candReal] || excluded[canon(cand)] {
			continue
		}
		if isShim(cand) {
			continue
		}
		return cand, nil
	}
	return "", fmt.Errorf("%s not found on PATH (outside aiq's shims and %d wrapper(s) already tried); set providers.%s.binary in the config", name, len(exclude), name)
}

func canon(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// isShim detects a script that execs aiq (our own shim, or a copy of it).
func isShim(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	head := string(buf[:n])
	if !strings.HasPrefix(head, "#!") {
		return false
	}
	return strings.Contains(head, "aiq run ") || strings.Contains(head, "AIQ_SHIM")
}
