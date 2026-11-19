//go:build !linux

package nicstat

import "runtime"

// Read reports that this platform's implementation is not yet available.
func Read() ([]Counters, error) { return nil, &UnsupportedError{OS: runtime.GOOS} }

// DefaultRoute reports that this platform's implementation is not yet available.
func DefaultRoute() (string, error) { return "", &UnsupportedError{OS: runtime.GOOS} }
