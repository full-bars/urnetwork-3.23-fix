//go:build !linux

package main

import "context"

// The stall defense's other-than-systemd half is OpenRC only, and OpenRC is
// Linux (the thrash watchdog and its exit-75 escape are Linux-only too), so on
// every other platform only the systemd watchdog path exists.

func livenessUnderOpenRc() bool { return false }

func runOpenRcLivenessWatchdog(ctx context.Context, gate func(episodeStart bool) (bool, string)) {
}
