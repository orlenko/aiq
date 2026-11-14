package daemon

import (
	"fmt"
	"os"
)

// openOwnerOnly opens path for reading only if it is a regular file and not a link.
// Windows has no mode bits to check; the data directory's access list protects it.
func openOwnerOnly(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.Open(path)
}
