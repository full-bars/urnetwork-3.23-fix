//go:build windows

package urnettools

// processAliveStrict is unused on Windows (the process-list guard owns that
// path); it fails closed in case it is ever reached.
func processAliveStrict(int) bool { return true }
