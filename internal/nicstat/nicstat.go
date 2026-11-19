// Package nicstat reads cumulative network interface byte counters.
package nicstat

import (
	"errors"
	"fmt"
)

// Counters is a snapshot of an interface's cumulative byte counters.
// Counters may wrap or reset when an interface is recreated.
type Counters struct {
	Name             string
	RxBytes, TxBytes uint64
	Up, Loopback     bool
}

// ErrNoDefaultRoute indicates that no eligible default route was found.
var ErrNoDefaultRoute = errors.New("no default route")

// UnsupportedError indicates that the operating system is not supported.
type UnsupportedError struct{ OS string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("nicstat: unsupported operating system %s", e.OS)
}
