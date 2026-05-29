package main

import (
	"context"
	"testing"
)

// The eco memory monitor is a single global watcher, but it was started from
// inside the per-proxy provide loop, so a large proxy list spawned one monitor
// per proxy. Each copy logs the same "[eco]" line and calls runtime.GC() on a
// pressure transition. startEcoMonitorOnce must start exactly one regardless of
// how many times (and from how many call sites) it is invoked.
func TestEcoMonitorStartsOnce(t *testing.T) {
	ecoMonitorStarted.Store(false)

	starts := 0
	orig := startEcoMonitor
	startEcoMonitor = func(ctx context.Context) { starts++ }
	defer func() { startEcoMonitor = orig }()

	ctx := context.Background()
	const proxies = 5
	for i := 0; i < proxies; i++ {
		startEcoMonitorOnce(ctx)
	}

	if starts != 1 {
		t.Fatalf("expected eco monitor to start exactly once across %d calls, got %d", proxies, starts)
	}
}
