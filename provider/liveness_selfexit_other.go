//go:build !linux

package main

import "context"

// The self-exit half of the stall defense covers OpenRC's supervise-daemon and
// containers, and both ride the Linux-only thrash watchdog (its exit-75 escape,
// cap ring and state directory). On every other platform only the systemd
// watchdog path exists.

func livenessUnderSelfExitSupervisor() bool { return false }

func runSelfExitLivenessWatchdog(ctx context.Context, gate func(episodeStart bool) (bool, string)) {
}
