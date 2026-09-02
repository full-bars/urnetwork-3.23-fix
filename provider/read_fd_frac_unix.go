//go:build !windows

package main

import (
	"os"
	"syscall"
)

// readFDFrac returns the fraction of the FD budget the process is using
// (open FDs / RLIMIT_NOFILE), or -1 if it cannot be determined. It counts
// the entries in /proc/self/fd (symlinks to open descriptors) and divides by
// the soft RLIMIT_NOFILE. This is a proxy's most load-bearing resource —
// open sockets — and had no pressure signal (finding #5).
//
// Unix-only: /proc and syscall.Rlimit are not available on Windows; the
// windows build uses a stub that reports unavailable (-1) so the pressure
// sensor degrades gracefully there instead of failing to compile.
func readFDFrac() float64 {
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err != nil {
		return -1
	}
	limit := rLimit.Cur
	if limit == 0 {
		return -1
	}
	// Count open FDs as entries in /proc/self/fd.
	fdDir, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	open := len(fdDir)
	frac := float64(open) / float64(limit)
	if frac > 1 {
		frac = 1
	}
	return frac
}
