package connect

import "golang.org/x/sys/unix"

// OpenBSD exposes physical memory as a 64-bit sysctl. Its VM counters differ
// from FreeBSD's; do not report a guessed available-memory value.
func HostMemoryTotalBytes() (int64, bool) {
	v, err := unix.SysctlUint64("hw.physmem")
	if err != nil || v == 0 || v > uint64(1<<63-1) {
		return 0, false
	}
	return int64(v), true
}

func HostMemoryAvailableBytes() (int64, bool) { return 0, false }
