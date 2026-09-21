//go:build linux

package main

import (
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readProcResources fills RSS, open descriptors and the descriptor limit from
// /proc and getrlimit. Anything that cannot be read is left zero.
func readProcResources(r *SnapshotResources) {
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		// statm: size resident shared text lib data dt, all in pages.
		if f := strings.Fields(string(data)); len(f) >= 2 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				r.RSSBytes = pages * uint64(os.Getpagesize())
			}
		}
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil && len(entries) > 0 {
		// ReadDir holds one descriptor of its own open while it lists.
		r.OpenFDs = uint64(len(entries) - 1)
	}
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur != 0 && uint64(rl.Cur) != math.MaxUint64 {
		r.FDLimit = uint64(rl.Cur)
	}
}
