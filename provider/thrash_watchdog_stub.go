//go:build !linux

package main

import (
	"context"
	"sync/atomic"
	"time"
)

// Stub for non-Linux platforms: there is no /proc, no cgroup and no PSI to
// read, so the thrash watchdog has nothing to sense and nothing to do. The
// thrash cap file plumbing (thrash_cap.go) stays portable because
// effectiveTrimCapSource consults it on every platform.
func runThrashWatchdog(_ context.Context, _ bool) {}

// nowFn is the shared supervisor clock seam on non-Linux builds: the Linux
// watchdog (thrash_watchdog.go) declares the same var next to thrashNowFn,
// which only exists there. The portable pressure monitor's gcNow delegates
// to nowFn on every platform, so non-Linux builds need it declared too.
var nowFn = time.Now

// thrashFreeze is referenced by provide/main supervision wiring on every
// platform; off Linux it never becomes true (the watchdog is a no-op) and the
// pool controller that reads it is a no-op stub too.
var thrashFreeze atomic.Bool

// cgroupV2SelfDir is a no-op off Linux: there is no cgroup to resolve, so
// the callers' fallbacks (unreadable -> unquota'd -> NumCPU) apply.
func cgroupV2SelfDir() string { return "" }
