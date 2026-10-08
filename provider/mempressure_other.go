//go:build !darwin && !windows && !freebsd

package main

// readHostMemPressure has no source on Linux (readPSI covers it) or on other
// platforms.
func readHostMemPressure() (float64, error) { return 0, errNoHostMemPressure }
