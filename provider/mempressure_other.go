//go:build !darwin && !windows

package main

// readHostMemPressure has no source on Linux (readPSI covers it) or on other
// platforms.
func readHostMemPressure() (float64, error) { return 0, errNoHostMemPressure }
