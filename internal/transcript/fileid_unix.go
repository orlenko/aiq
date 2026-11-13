//go:build unix

package transcript

import (
	"os"
	"syscall"
)

// fileID is the device and inode of a file a stat described.
func fileID(fi os.FileInfo) (dev, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}
