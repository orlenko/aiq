package longrun

import "golang.org/x/sys/unix"

func renameNoteExclusive(from, to string) error { return unix.RenamexNp(from, to, unix.RENAME_EXCL) }
