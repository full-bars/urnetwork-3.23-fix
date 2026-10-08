package main

import "errors"

// errNoHostMemPressure is returned where the OS exposes no memory pressure
// signal beyond what /proc/pressure/memory already provides on Linux.
var errNoHostMemPressure = errors.New("host memory pressure signal not available on this platform")

// Memory pressure off Linux arrives as a coarse level rather than the PSI
// "some avg60" percentage, so each platform's level is translated onto the
// same percent scale the psi_mem component ramps over (psiRampLo..psiRampHi).
// That keeps one ramp, one set of thresholds and one log vocabulary for the
// component on every OS.

// macOS kern.memorystatus_vm_pressure_level values.
const (
	macPressureNormal   = 1
	macPressureWarn     = 2
	macPressureCritical = 4
)

// memPressurePercentFromMacLevel translates the macOS memory-pressure level
// into a PSI-style percent for the psi_mem component. psi_mem scores 0 at
// psiRampLo (10) and 1 at psiRampHi (60), so warn lands mid-ramp (gentle
// shedding; macOS sits in warn for long stretches on busy machines) and
// critical pins the component at 1. Unknown levels fail open, like a missing
// reading.
func memPressurePercentFromMacLevel(level uint32) float64 {
	switch level {
	case macPressureWarn:
		return 30
	case macPressureCritical:
		return psiRampHi
	default: // macPressureNormal and anything the kernel adds later
		return 0
	}
}

// memPressurePercentFromSwapFrac translates FreeBSD swap utilization into a
// PSI-style percent for the psi_mem component.
//
// Swap is a lagging indicator: a host only faults pages out once it is already
// under real pressure, so the curve is mapped so that a swap fraction that
// still looks modest (25%) already scores like the middle of the psi_mem ramp.
// A host that has swapped at all is a host that was short of memory, so the
// ramp saturates at swapFracRampFull rather than at a completely full device:
// past that point more swap use adds no information.
//
// A zero or negative fraction is not a reading and scores 0: a provider that
// has not faulted anything must not be told it is under pressure.
func memPressurePercentFromSwapFrac(frac float64) float64 {
	if frac <= 0 {
		return 0
	}
	// Saturate at the ramp point, not at a full device: the ramp already pins
	// psi_mem at swapFracRampFull, so extrapolating past it produced readings
	// far above psiRampHi (about 238 at 99% swap) and then dropped back to
	// psiRampHi once the device filled, which made the signal fall as the host
	// got worse.
	if frac >= swapFracRampFull {
		return psiRampHi
	}
	// 0 -> 0 and 25% -> psiRampHi, linear between. psiRampLo is unused here
	// because swap cannot report the gentle-contention band that PSI can.
	return frac / swapFracRampFull * psiRampHi
}

// swapFracRampFull is the fraction of configured swap whose use already scores
// as the top of the psi_mem ramp.
const swapFracRampFull = 0.25

// memPressurePercentWindowsLow is the percent reported while Windows signals
// LowMemoryResourceNotification. That is a binary "the system is short of
// physical memory" flag Windows raises only near real exhaustion, so it maps
// to the top of the ramp.
func memPressurePercentWindowsLow(low bool) float64 {
	if low {
		return psiRampHi
	}
	return 0
}
