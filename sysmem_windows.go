//go:build windows

package connect

import (
	"syscall"
	"unsafe"
)

var (
	sysmemKernel32             = syscall.NewLazyDLL("kernel32.dll")
	sysmemGlobalMemoryStatusEx = sysmemKernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors the Win32 MEMORYSTATUSEX structure.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func readMemoryStatusEx() (memoryStatusEx, bool) {
	var s memoryStatusEx
	s.Length = uint32(unsafe.Sizeof(s))
	r, _, _ := sysmemGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s)))
	if r == 0 {
		return memoryStatusEx{}, false
	}
	return s, true
}

// HostMemoryTotalBytes is the host's physical RAM from GlobalMemoryStatusEx.
// ok is false when it cannot be read.
func HostMemoryTotalBytes() (int64, bool) {
	s, ok := readMemoryStatusEx()
	if !ok || s.TotalPhys == 0 {
		return 0, false
	}
	return int64(s.TotalPhys), true
}

// HostMemoryAvailableBytes is the physical memory available to processes
// (free plus standby), from GlobalMemoryStatusEx. ok is false when it cannot
// be read.
func HostMemoryAvailableBytes() (int64, bool) {
	s, ok := readMemoryStatusEx()
	if !ok || s.TotalPhys == 0 {
		return 0, false
	}
	return int64(s.AvailPhys), true
}
