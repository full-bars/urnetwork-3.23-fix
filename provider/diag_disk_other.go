//go:build !unix && !windows

package main

import "errors"

// diskFreeBytes has no implementation here, so diag refuses to run rather than
// capture without the free-space check.
func diskFreeBytes(dir string) (uint64, error) {
	return 0, errors.New("free space cannot be measured on this platform")
}
