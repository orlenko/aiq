//go:build unix

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// openOwnerOnly opens path for reading only if it is a regular file, not a link, owned by
// this user and closed to everyone else. A missing file comes back as fs.ErrNotExist.
func openOwnerOnly(path string) (*os.File, error) {
	// O_NONBLOCK so a FIFO put there cannot hold the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case !info.Mode().IsRegular():
		err = fmt.Errorf("%s is not a regular file", path)
	case !ok || int(st.Uid) != os.Getuid():
		err = fmt.Errorf("%s is not owned by this user", path)
	case info.Mode().Perm()&0o077 != 0:
		err = fmt.Errorf("%s is open to other users (mode %v)", path, info.Mode().Perm())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
