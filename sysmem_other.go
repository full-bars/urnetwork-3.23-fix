//go:build !linux && !darwin && !windows

package connect

// HostMemoryTotalBytes is not implemented on this platform.
func HostMemoryTotalBytes() (int64, bool) { return 0, false }

// HostMemoryAvailableBytes is not implemented on this platform.
func HostMemoryAvailableBytes() (int64, bool) { return 0, false }
