//go:build !linux && !darwin

package longrun

import "errors"

func renameNoteExclusive(from, to string) error {
	return errors.New("atomic handoff-note retirement is unavailable on this platform")
}
