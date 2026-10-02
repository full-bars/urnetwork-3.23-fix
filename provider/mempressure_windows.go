//go:build windows

package main

import (
	"sync"
	"syscall"
	"unsafe"
)

const lowMemoryResourceNotification = 0

var (
	memPressureKernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMemResourceNotif = memPressureKernel32.NewProc("CreateMemoryResourceNotification")
	procQueryMemResourceNotif  = memPressureKernel32.NewProc("QueryMemoryResourceNotification")

	memPressureOnce   sync.Once
	memPressureHandle uintptr
	memPressureErr    error
)

// readHostMemPressure reports whether Windows currently signals low physical
// memory, as a PSI-style percent. The notification handle is created once and
// reused for the life of the process.
func readHostMemPressure() (float64, error) {
	memPressureOnce.Do(func() {
		h, _, err := procCreateMemResourceNotif.Call(lowMemoryResourceNotification)
		if h == 0 {
			memPressureErr = err
			return
		}
		memPressureHandle = h
	})
	if memPressureErr != nil {
		return 0, memPressureErr
	}
	var low int32
	r, _, err := procQueryMemResourceNotif.Call(memPressureHandle, uintptr(unsafe.Pointer(&low)))
	if r == 0 {
		return 0, err
	}
	return memPressurePercentWindowsLow(low != 0), nil
}
