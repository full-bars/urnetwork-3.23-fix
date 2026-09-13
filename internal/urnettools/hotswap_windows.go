//go:build windows

package urnettools

import (
	"syscall"
	"unsafe"
)

// triggerHotSwap sends {cmd: "hotswap"} over the provider's control socket
// to initiate the in-process handoff. Windows has no SIGUSR2, so the
// control socket is the only trigger path.
func triggerHotSwap(p Provider) error {
	if err := hotSwapPreflight(p); err != nil {
		return err
	}
	return triggerHotSwapViaSocket(p)
}

// pidIsAlive reports whether pid still refers to a running process.
// Uses OpenProcess + GetExitCodeProcess: a process whose exit code is
// STILL_ACTIVE (259) is considered alive. Every other code means it has
// exited.
func pidIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const (
		processQueryLimitedInformation = 0x1000
		stillActive                    = 259 // 0x103
	)

	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	procOpen := kernel32.NewProc("OpenProcess")
	procExitCode := kernel32.NewProc("GetExitCodeProcess")

	handle, _, _ := procOpen.Call(
		uintptr(processQueryLimitedInformation),
		0, // bInheritHandle = FALSE
		uintptr(pid),
	)
	if handle == 0 {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	var exitCode uint32
	ret, _, _ := procExitCode.Call(
		handle,
		uintptr(unsafe.Pointer(&exitCode)),
	)
	if ret == 0 {
		// GetExitCodeProcess failed — treat as alive to avoid false rollback.
		return true
	}
	return exitCode == stillActive
}
