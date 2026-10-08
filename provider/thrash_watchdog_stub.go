//go:build !linux

package main

import (
	"context"
	"sync/atomic"
)

// Stub for non-Linux platforms: there is no /proc, no cgroup and no PSI to
// read, so the thrash watchdog has nothing to sense and nothing to do. The
// thrash cap file plumbing (thrash_cap.go) stays portable because
// effectiveTrimCapSource consults it on every platform.
func runThrashWatchdog(_ context.Context, _ bool) {}

// thrashFreeze is referenced by provide/main supervision wiring on every
// platform; off Linux it never becomes true (the watchdog is a no-op) and the
// pool controller that reads it is a no-op stub too.
var thrashFreeze atomic.Bool

// cgroupV2SelfDir is a no-op off Linux: there is no cgroup to resolve, so
// the callers' fallbacks (unreadable -> unquota'd -> NumCPU) apply.
func cgroupV2SelfDir() string { return "" }
