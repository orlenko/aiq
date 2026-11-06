package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// flushInput is tcflush(fd, TCIFLUSH).
func flushInput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }

// flushOutput is tcflush(fd, TCOFLUSH).
func flushOutput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCOFLUSH) }

// toNull points fd at null.
func toNull(null, fd int) { unix.Dup3(null, fd, 0) }
