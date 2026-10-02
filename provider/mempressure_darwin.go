//go:build darwin

package main

import "golang.org/x/sys/unix"

// readHostMemPressure reports the kernel's memory-pressure level as a PSI-style
// percent. kern.memorystatus_vm_pressure_level is what the OS itself uses to
// ask apps to release memory, which makes it the closest analogue to PSI some.
func readHostMemPressure() (float64, error) {
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return 0, err
	}
	return memPressurePercentFromMacLevel(level), nil
}
