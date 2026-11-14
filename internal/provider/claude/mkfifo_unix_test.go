//go:build unix

package claude

import "syscall"

func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }
