package herdr

import "golang.org/x/sys/unix"

func fixtureMode() (*unix.Termios, error) { return unix.IoctlGetTermios(0, unix.TIOCGETA) }
