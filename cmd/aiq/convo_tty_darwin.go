package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// flushInput is tcflush(fd, TCIFLUSH): TIOCFLUSH with FREAD, which is 1
// like TCIFLUSH.
func flushInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, unix.TCIFLUSH) }

// flushOutput is tcflush(fd, TCOFLUSH): TIOCFLUSH with FWRITE, which is 2
// like TCOFLUSH.
func flushOutput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, unix.TCOFLUSH) }

// toNull points fd at null.
func toNull(null, fd int) { unix.Dup2(null, fd) }
