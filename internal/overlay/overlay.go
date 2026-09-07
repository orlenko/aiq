// Package overlay maintains a provider home that shares everything with the
// user's real home except a few private files: a directory whose entries are
// symlinks into the real home, plus real files of its own.
//
// Sync is idempotent and runs before every launch, under a per-overlay flock:
//
//   - every entry of the real home gets a symlink of the same name;
//   - an excluded name (the credential, a per-account state file) is never
//     linked and never adopted;
//   - a regular file that appeared only in the overlay (a CLI wrote a new
//     file, or replaced a symlink with a rename-over write) is adopted: moved
//     into the real home when it is the newer copy, then re-linked; the
//     displaced real copy is kept as a rotated backup;
//   - a directory that appeared in both places is merged, never deleted;
//   - scratch files (*.tmp.*, *.lock, *~) and files modified in the last few
//     seconds are left alone, since a CLI may be mid-write;
//   - dangling symlinks are removed.
package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Spec struct {
	Real    string
	Overlay string
	// Exclude names entries that stay private to the overlay.
	Exclude []string
	// Extra maps an overlay entry name to a target outside Real
	// (e.g. ".claude.json" → ~/.claude.json).
	Extra map[string]string
	// SkipPrefixes names entries never linked or adopted (junk like ".DS_Store").
	SkipPrefixes []string
	// LockDir holds the per-overlay lock file ("" = inside the overlay).
	LockDir string
}

type Report struct {
	Linked   []string
	Adopted  []string
	Merged   []string
	Replaced []string // overlay copy was older than the real one and got discarded
	Removed  []string
	Warnings []string
}

func (r Report) Changed() bool {
	return len(r.Linked)+len(r.Adopted)+len(r.Merged)+len(r.Replaced)+len(r.Removed) > 0
}

// settleTime is how old a file must be before Sync will move it: a CLI that
// is between writing a temp file and renaming it must not lose the race.
const settleTime = 5 * time.Second

func (s Spec) excluded(name string) bool {
	for _, e := range s.Exclude {
		if e == name {
			return true
		}
	}
	return false
}

// scratch reports names that are never linked and never adopted.
func (s Spec) scratch(name string) bool {
	for _, p := range s.SkipPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	if strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".lock") || strings.Contains(name, ".tmp.") || strings.HasSuffix(name, ".tmp") {
		return true
	}
	return false
}

// Sync brings the overlay up to date with the real home.
func Sync(s Spec) (Report, error) {
	var rep Report
	if err := os.MkdirAll(s.Overlay, 0o700); err != nil {
		return rep, err
	}
	if _, err := os.Stat(s.Real); err != nil {
		return rep, fmt.Errorf("real home %s: %w", s.Real, err)
	}
	unlock, err := lock(s)
	if err != nil {
		return rep, err
	}
	defer unlock()

	wanted := map[string]string{} // overlay entry name → target
	entries, err := os.ReadDir(s.Real)
	if err != nil {
		return rep, err
	}
	for _, e := range entries {
		name := e.Name()
		if s.excluded(name) || s.scratch(name) {
			continue
		}
		wanted[name] = filepath.Join(s.Real, name)
	}
	for name, target := range s.Extra {
		if s.excluded(name) {
			continue
		}
		if _, err := os.Lstat(target); err == nil {
			wanted[name] = target
		}
	}
	now := time.Now()

	// Pass 1: reconcile what the overlay already has.
	have, err := os.ReadDir(s.Overlay)
	if err != nil {
		return rep, err
	}
	for _, e := range have {
		name := e.Name()
		path := filepath.Join(s.Overlay, name)
		if s.excluded(name) || s.scratch(name) {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		target, want := wanted[name]
		if info.Mode()&os.ModeSymlink != 0 {
			current, _ := os.Readlink(path)
			switch {
			case !want:
				if _, err := os.Stat(path); err != nil {
					os.Remove(path)
					rep.Removed = append(rep.Removed, name)
				}
			case current != target:
				os.Remove(path)
				if err := os.Symlink(target, path); err != nil {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("relink %s: %v", name, err))
				} else {
					rep.Linked = append(rep.Linked, name)
				}
			}
			continue
		}
		// A real file or directory in the overlay.
		if !info.IsDir() && now.Sub(info.ModTime()) < settleTime {
			continue // possibly mid-write; next launch will get it
		}
		if !want {
			// New entry the CLI created here: move it home and link it.
			target = filepath.Join(s.Real, name)
			if extra, ok := s.Extra[name]; ok {
				target = extra
			}
			if _, err := os.Lstat(target); err == nil {
				// Appeared in the real home since we listed it: fall through
				// to the collision logic on the next launch.
				continue
			}
			if err := os.Rename(path, target); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("adopt %s: %v", name, err))
				continue
			}
			if err := os.Symlink(target, path); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("link adopted %s: %v", name, err))
				continue
			}
			rep.Adopted = append(rep.Adopted, name)
			continue
		}
		// Both sides have a real copy.
		realInfo, err := os.Stat(target)
		if err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("stat %s: %v", target, err))
			continue
		}
		if info.IsDir() != realInfo.IsDir() {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: overlay and real home disagree on file vs directory; left alone", name))
			continue
		}
		if info.IsDir() {
			// Two sessions created the same directory independently: merge
			// the overlay's contents into the real one. Nothing is deleted.
			if err := mergeDir(path, target, now); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("merge %s: %v", name, err))
				continue
			}
			if err := os.Remove(path); err != nil {
				// Something is still inside (a file mid-write). Retry next launch.
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("merge %s: directory not empty yet, retrying next launch", name))
				continue
			}
			rep.Merged = append(rep.Merged, name)
		} else if info.ModTime().After(realInfo.ModTime()) {
			if err := adoptNewer(path, target); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("adopt newer %s: %v", name, err))
				continue
			}
			rep.Adopted = append(rep.Adopted, name)
		} else {
			// Older than the real copy: keep it as a backup, not deleted.
			if err := rotateBackup(path, target); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("drop %s: %v", name, err))
				continue
			}
			rep.Replaced = append(rep.Replaced, name)
		}
		if err := os.Symlink(target, path); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("link %s: %v", name, err))
		}
	}

	// Pass 2: link whatever is still missing.
	for name, target := range wanted {
		path := filepath.Join(s.Overlay, name)
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := os.Symlink(target, path); err != nil {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("link %s: %v", name, err))
			continue
		}
		rep.Linked = append(rep.Linked, name)
	}
	return rep, nil
}

// adoptNewer moves the overlay file over the real one, keeping the real one
// as a rotated backup. Each rename re-checks the entry it is about to move,
// so a concurrent Sync that already did the work cannot turn the real file
// into a self-referencing symlink.
func adoptNewer(overlayPath, realPath string) error {
	info, err := os.Lstat(overlayPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
		return fmt.Errorf("overlay entry changed underneath us")
	}
	if err := rotateBackup(realPath, realPath); err != nil {
		return err
	}
	return os.Rename(overlayPath, realPath)
}

// rotateBackup moves src to <real>.aiq-bak, shifting older backups to
// .aiq-bak.1 .. .aiq-bak.3. Nothing is ever unlinked outright.
func rotateBackup(src, realPath string) error {
	base := realPath + ".aiq-bak"
	os.Remove(base + ".3")
	for i := 2; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", base, i), fmt.Sprintf("%s.%d", base, i+1))
	}
	os.Rename(base, base+".1")
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink now; refusing to rotate it", src)
	}
	return os.Rename(src, base)
}

// mergeDir moves everything under src into dst, recursing into directories
// that exist on both sides and keeping the newer copy of a file that exists
// on both (the older one becomes a rotated backup). src is left empty (or
// nearly: files still mid-write stay behind).
func mergeDir(src, dst string, now time.Time) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		from := filepath.Join(src, e.Name())
		to := filepath.Join(dst, e.Name())
		fromInfo, err := os.Lstat(from)
		if err != nil {
			continue
		}
		toInfo, err := os.Lstat(to)
		if err != nil {
			if err := os.Rename(from, to); err != nil {
				return err
			}
			continue
		}
		switch {
		case fromInfo.IsDir() && toInfo.IsDir():
			if err := mergeDir(from, to, now); err != nil {
				return err
			}
			os.Remove(from)
		case fromInfo.IsDir() || toInfo.IsDir():
			return fmt.Errorf("%s: file/directory mismatch", to)
		case fromInfo.Mode()&os.ModeSymlink != 0:
			os.Remove(from)
		case now.Sub(fromInfo.ModTime()) < settleTime:
			// leave it; the directory stays and is retried next launch
		case fromInfo.ModTime().After(toInfo.ModTime()):
			if err := rotateBackup(to, to); err != nil {
				return err
			}
			if err := os.Rename(from, to); err != nil {
				return err
			}
		default:
			if err := rotateBackup(from, to); err != nil {
				return err
			}
		}
	}
	return nil
}

// lock takes an exclusive flock for the overlay so two launches of the same
// account cannot adopt the same file at once.
func lock(s Spec) (func(), error) {
	dir := s.LockDir
	if dir == "" {
		dir = s.Overlay
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := ".aiq-sync.lock"
	if s.LockDir != "" {
		name = strings.ReplaceAll(strings.Trim(s.Overlay, string(filepath.Separator)), string(filepath.Separator), "_") + ".sync.lock"
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Seed copies a file into the overlay once, for entries that are private per
// account but should start from the user's current copy.
func Seed(src, overlayPath string) error {
	if info, err := os.Lstat(overlayPath); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		// A leftover link from an earlier layout: replace it with a copy.
		os.Remove(overlayPath)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(overlayPath, data, 0o600)
}

// Touch writes a marker file recording the last sync time (for doctor).
func Touch(overlayDir string) {
	os.WriteFile(filepath.Join(overlayDir, ".aiq-synced"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}
