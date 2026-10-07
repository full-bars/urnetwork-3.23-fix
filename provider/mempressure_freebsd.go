//go:build freebsd

package main

import (
	"fmt"
	"os/exec"
)

// readHostMemPressure reports memory pressure as a PSI-style percent on
// FreeBSD.
//
// FreeBSD has no /proc/pressure/memory, so this is the fallback the pressure
// collector reaches for. Swap utilization is the signal: a kernel under memory
// pressure starts faulting pages out, and the fraction of configured swap
// already in use rises long before the host is in trouble. It is a lagging
// signal compared with PSI, so it is deliberately mapped conservatively (see
// memPressurePercentFromSwapFrac).
//
// The reading comes from swapinfo(8), NOT from a sysctl. An earlier version
// read "vm.swap_used", which does not exist: FreeBSD exposes vm.swap_total but
// no matching "used" counter, so the read failed and the pressure sensor was
// silently inert. swapinfo -k is the supported interface for swap usage and is
// present in the base system.
//
// Without configured swap (total == 0) there is nothing to read and the
// MemAvailable fraction in readMemAvailFrac remains the memory signal; this
// reports no pressure rather than inventing one.
func readHostMemPressure() (float64, error) {
	out, err := exec.Command("swapinfo", "-k").Output()
	if err != nil {
		return 0, fmt.Errorf("swapinfo: %w", err)
	}
	total, used, ok := parseSwapinfo(out)
	if !ok || total == 0 {
		return 0, errNoHostMemPressure
	}
	if used > total {
		// Should not happen; a used count above the total means the reading is
		// not comparable, and treating it as 100% would pin the governor on a
		// bogus reading.
		return 0, errNoHostMemPressure
	}
	return memPressurePercentFromSwapFrac(float64(used) / float64(total)), nil
}
