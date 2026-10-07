//go:build freebsd

package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

// readHostMemPressure reports memory pressure as a PSI-style percent on
// FreeBSD.
//
// FreeBSD has no /proc/pressure/memory, so this is the fallback the pressure
// collector reaches for. Swap utilization is the signal: a kernel under
// memory pressure starts faulting pages out, and the fraction of configured
// swap already in use rises long before the host is in trouble. It is a
// lagging signal compared with PSI, so it is deliberately mapped conservatively
// (see memPressurePercentFromSwapFrac).
//
// Without configured swap (vm.swap_total == 0) there is nothing to read and
// the MemAvailable fraction in readMemAvailFrac remains the memory signal; this
// reports no pressure rather than inventing one.
func readHostMemPressure() (float64, error) {
	total, err := swapBlocks()
	if err != nil || total == 0 {
		return 0, errNoHostMemPressure
	}
	used, err := unix.SysctlUint32("vm.swap_used")
	if err != nil {
		return 0, errors.New("vm.swap_used: " + err.Error())
	}
	if uint64(used) > total {
		// Should not happen; a used count above the total means the reading
		// is not comparable, and treating it as 100% would pin the governor
		// on a bogus reading.
		return 0, errNoHostMemPressure
	}
	return memPressurePercentFromSwapFrac(float64(used) / float64(total)), nil
}

// swapBlocks is the total configured swap in FreeBSD's native block units.
// Both swap sysctls count these, so the ratio below needs no unit conversion.
func swapBlocks() (uint64, error) {
	v, err := unix.SysctlUint64("vm.swap_total")
	if err != nil {
		return 0, err
	}
	return v, nil
}