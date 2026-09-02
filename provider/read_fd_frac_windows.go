//go:build windows

package main

// readFDFrac reports FD-budget usage. On Windows there is no /proc/self/fd
// count and no syscall.Rlimit/RLIMIT_NOFILE in Go's syscall package, so the
// FD pressure sensor is unavailable there — report -1 (unknown), which the
// pressure system treats as "no signal" and fails open. See the Unix
// implementation for the real sensor.
func readFDFrac() float64 {
	return -1
}
