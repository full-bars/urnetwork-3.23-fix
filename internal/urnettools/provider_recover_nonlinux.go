//go:build !linux

package urnettools

import "fmt"

// lookupUserIDs resolves the numeric uid and gid for a username. On non-Linux
// platforms this is a stub — the tool doesn't chown under sudo on non-Linux.
func lookupUserIDs(user string) (int, int, error) {
	return 0, 0, fmt.Errorf("lookupUserIDs not implemented on this platform")
}