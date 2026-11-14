package main

import (
	"errors"
	"golang.org/x/sys/unix"
)

func (d *unixResumeDir) accessList(name string) (bool, error) {
	_, err := unix.Fgetxattr(d.fd, name, nil)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		return false, nil
	}
	return false, err
}
