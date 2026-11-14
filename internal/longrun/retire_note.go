package longrun

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NoteRetirement prepares the existing redaction outside the lease writer lock.
// The current note is unchanged until Retire rechecks its captured version.
// Its scratch file uses the existing .redact temporary-file format and lifetime.
type NoteRetirement struct {
	path, temp string
	snapshot   os.FileInfo
}

func PrepareNoteRetirement(workspace string) (plan *NoteRetirement, err error) {
	plan = &NoteRetirement{path: HandoffPath(workspace)}
	_, text, info, err := scanNote(plan.path)
	if os.IsNotExist(err) {
		return plan, nil
	}
	if err != nil {
		return nil, err
	}
	plan.snapshot = info
	file, err := os.CreateTemp(filepath.Dir(plan.path), fmt.Sprintf(".redact-%d-*", time.Now().Unix()))
	if err != nil {
		return nil, err
	}
	plan.temp = file.Name()
	prepared := plan
	defer func() {
		if err != nil {
			prepared.Close()
		}
	}()
	_, err = file.WriteString(text)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chtimes(plan.temp, info.ModTime(), info.ModTime())
	}
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *NoteRetirement) Close() {
	if p != nil && p.temp != "" {
		os.Remove(p.temp)
		p.temp = ""
	}
}

// Retire performs metadata operations only, after the caller's exact lease
// UPDATE has matched. Existing archive names are never overwritten. A failed
// database commit can leave a safely archived note, as with ordinary drain.
func (p *NoteRetirement) Retire(now time.Time) (string, error) {
	if p == nil {
		return "", errors.New("note retirement unavailable")
	}
	current, err := os.Lstat(p.path)
	if p.snapshot == nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", errors.New("handoff note changed; retry")
	}
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, p.snapshot) || current.Size() != p.snapshot.Size() || !current.ModTime().Equal(p.snapshot.ModTime()) {
		return "", errors.New("handoff note changed; retry")
	}
	if p.temp == "" {
		return "", errors.New("note retirement unavailable")
	}
	if err := os.Chmod(filepath.Dir(p.path), 0700); err != nil {
		return "", err
	}
	if err := os.Rename(p.temp, p.path); err != nil {
		return "", err
	}
	p.temp = ""
	for i := 0; i < 60; i++ {
		dst := strings.TrimSuffix(p.path, ".md") + "." + now.Format("20060102-150405") + ".md"
		err := renameNoteExclusive(p.path, dst)
		if err == nil {
			return dst, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		now = now.Add(time.Second)
	}
	return "", errors.New("handoff archive names are busy; retry later")
}
