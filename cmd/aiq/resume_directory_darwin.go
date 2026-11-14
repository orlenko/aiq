package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"runtime"
	"unsafe"
)

func (d *unixResumeDir) accessList(name string) (bool, error) {
	if name != "darwin.acl" {
		return false, errors.New("unsupported access-list request")
	}
	var stat unix.Stat_t
	var buffer [resumeDarwinACLBytes]byte
	size := uintptr(len(buffer))
	// This is the descriptor-bound syscall used by libc's fstatx_np.
	// Syscall6, rather than RawSyscall6, permits the scheduler to progress.
	_, _, errno := unix.Syscall6(unix.SYS_FSTAT64_EXTENDED, uintptr(d.fd), uintptr(unsafe.Pointer(&stat)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), 0, 0)
	runtime.KeepAlive(&stat)
	runtime.KeepAlive(&buffer)
	runtime.KeepAlive(&size)
	if errno != 0 {
		return false, errno
	}
	if size > uintptr(len(buffer)) {
		return false, errors.New("macOS ACL exceeds supported kernel format")
	}
	return resumeDarwinACLGrantsWrite(buffer[:int(size)])
}
