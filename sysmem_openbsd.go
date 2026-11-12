package connect

import "golang.org/x/sys/unix"

// The width of hw.physmem on OpenBSD is unverified. If it is 32-bit,
// SysctlUint64 fails and total memory remains unknown. Available memory is
// also unknown because OpenBSD's VM counters differ from FreeBSD's.
func HostMemoryTotalBytes() (int64, bool) {
	v, err := unix.SysctlUint64("hw.physmem")
	if err != nil || v == 0 || v > uint64(1<<63-1) {
		return 0, false
	}
	return int64(v), true
}

func HostMemoryAvailableBytes() (int64, bool) { return 0, false }
