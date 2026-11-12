package herdr

import (
	"runtime"
	"syscall"
	"unsafe"
)

// Darwin's waitid is not exported by x/sys/unix. WNOWAIT observes an exit
// without releasing the PID; Run reaps exactly once after group cleanup.
func waitExited(pid int) error {
	var info [32]uint64 // aligned, larger than Darwin's siginfo_t; no fields read
	for {
		_, _, err := syscall.Syscall6(syscall.SYS_WAITID, 1, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), 0x04|0x20, 0, 0) // P_PID, WEXITED|WNOWAIT
		runtime.KeepAlive(&info)
		if err == syscall.EINTR {
			continue
		}
		if err != 0 {
			return err
		}
		return nil
	}
}
