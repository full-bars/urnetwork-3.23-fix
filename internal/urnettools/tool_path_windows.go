//go:build windows

package urnettools

// ensureToolOnPath is a no-op on Windows: the installer manages PATH there.
func ensureToolOnPath(_ bool) {}
