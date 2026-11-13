package transcript

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A changed input is an ordinary, uncached parse failure, not a parser panic
// or a successful parse that found no session. Keep its text content-free.
var errBoundedInput = errors.New("transcript: input changed during bounded parse")

// boundedDescriptor is deliberately narrower than *os.File. Providers receive
// only a SectionReader; invented fault tests can control descriptor observations.
type boundedDescriptor interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Close() error
}

// boundedParse reads a live prefix, not immutable historical bytes. Its caller
// owns admission and panic recovery; Close unwinds before recovery releases a slot.
func boundedParse(snap Snapshot, ceiling int64) (*Session, error) {
	return boundedParseWithOpen(snap, ceiling, func(path string) (boundedDescriptor, error) {
		return OpenRegular(path)
	})
}

func boundedParseWithOpen(snap Snapshot, ceiling int64, open func(string) (boundedDescriptor, error)) (*Session, error) {
	if snap.Size < 0 || ceiling < 0 {
		return nil, errBoundedInput
	}
	if snap.Size > ceiling {
		return nil, ErrTooLarge
	}
	f, err := open(snap.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close() // Preserve the public parsers' Close-error handling.
	info, err := f.Stat()
	if err != nil {
		return nil, errBoundedInput
	}
	if info.Size() > ceiling {
		return nil, ErrTooLarge
	}
	if !boundedDescriptorMatches(info, snap) {
		return nil, errBoundedInput
	}

	observer := &shortReadObserver{r: f}
	r := io.NewSectionReader(observer, 0, snap.Size)
	fallback := func() time.Time { return time.Unix(0, snap.Mtime) }
	var s *Session
	// Keep the path-shape dispatch in agreement with parseByPath. Both Codex
	// passes share this one section and its sticky physical-short-read state.
	switch {
	case strings.HasSuffix(filepath.ToSlash(snap.Path), "/.system_generated/logs/transcript.jsonl"):
		s, err = parseAgyReader(snap.Path, r, fallback)
	case strings.HasPrefix(filepath.Base(snap.Path), "rollout-") && strings.HasSuffix(snap.Path, ".jsonl"):
		s, err = parseCodexReader(snap.Path, r)
	default:
		s, err = parseClaudeReader(snap.Path, r, fallback)
	}
	if err != nil {
		return nil, err // Provider errors precede all final validation failures.
	}

	// Validate positive and nil-session results alike. Do not defer this: a
	// real panic must propagate unchanged after Close. For validation failures,
	// short input precedes Stat failure, then extent/identity mismatch; all use
	// the same private error. Stat still observes the final descriptor after a
	// short read, even if it has regrown. No pathname reopen or mtime relabeling.
	info, err = f.Stat()
	if observer.short || err != nil {
		return nil, errBoundedInput
	}
	if info.Size() > ceiling || !boundedDescriptorMatches(info, snap) {
		return nil, errBoundedInput
	}
	return s, nil
}

func boundedDescriptorMatches(info os.FileInfo, snap Snapshot) bool {
	dev, ino := fileID(info) // Unsupported platforms return zero, as does Stat.
	return info.Mode().IsRegular() && info.Size() >= snap.Size && dev == snap.Dev && ino == snap.Ino
}

// SectionReader clamps every request before it reaches this observer. Compare
// against that in-range request, not the provider's possibly larger buffer.
// Logical EOF at the section endpoint never reaches the underlying ReaderAt.
type shortReadObserver struct {
	r     io.ReaderAt
	short bool
}

func (r *shortReadObserver) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.r.ReadAt(p, off)
	if n < len(p) {
		r.short = true
	}
	return n, err
}

// Public Claude/AGY wrappers call this only at their original fallback point,
// after reading, using their opened descriptor. A failed Stat leaves Updated zero.
func descriptorMtime(stat func() (os.FileInfo, error)) time.Time {
	if info, err := stat(); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
