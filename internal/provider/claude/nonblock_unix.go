//go:build unix

package claude

import "syscall"

// oNonblock keeps opening a FIFO from waiting for a writer.
const oNonblock = syscall.O_NONBLOCK
