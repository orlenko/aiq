//go:build unix

package navtoken

import (
	"io"
	"os"
	"syscall"
)

func openValidated(path string) (io.ReadCloser, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if p, ok := err.(*os.PathError); ok {
			if cause, ok := p.Err.(syscall.Errno); ok && cause == syscall.ELOOP {
				return nil, ErrUnsafe
			}
		}
		return nil, ErrUnavailable
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ErrUnavailable
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || int(owner.Uid) != os.Getuid() || info.Mode().Perm()&0o077 != 0 {
		f.Close()
		return nil, ErrUnsafe
	}
	return f, nil
}
