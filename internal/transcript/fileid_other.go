//go:build !unix

package transcript

import "os"

// fileID is zero where a stat gives no inode: such a file is known by its
// path alone.
func fileID(fi os.FileInfo) (dev, ino uint64) { return 0, 0 }
