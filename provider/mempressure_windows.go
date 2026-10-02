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

	memPressureMu     sync.Mutex
	memPressureHandle uintptr
)

// memPressureNotificationHandle returns the cached notification handle,
// creating it on first use. Only a successful creation is cached: the API may
// fail transiently, and a failure must not disable the sensor for the life of
// the process, so the next sample tries again.
func memPressureNotificationHandle() (uintptr, error) {
	memPressureMu.Lock()
	defer memPressureMu.Unlock()
	if memPressureHandle != 0 {
		return memPressureHandle, nil
	}
	h, _, err := procCreateMemResourceNotif.Call(lowMemoryResourceNotification)
	if h == 0 {
		return 0, err
	}
	memPressureHandle = h
	return h, nil
}

// readHostMemPressure reports whether Windows currently signals low physical
// memory, as a PSI-style percent. The notification handle is reused for the
// life of the process once it has been created.
func readHostMemPressure() (float64, error) {
	h, err := memPressureNotificationHandle()
	if err != nil {
		return 0, err
	}
	var low int32
	r, _, err := procQueryMemResourceNotif.Call(h, uintptr(unsafe.Pointer(&low)))
	if r == 0 {
		return 0, err
	}
	return memPressurePercentWindowsLow(low != 0), nil
}
