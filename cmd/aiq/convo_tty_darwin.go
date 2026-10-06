package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// flushInput is tcflush(fd, TCIFLUSH): TIOCFLUSH with FREAD, which is 1
// like TCIFLUSH.
func flushInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, unix.TCIFLUSH) }
