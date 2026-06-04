package main

import (
	"context"
	"os"
	"sync/atomic"
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

func TestReadSHMLog_NotExist(t *testing.T) {
	out, err := readSHMLog("/tmp/does-not-exist-urnetwork.log", 0)
	if err == nil {
		t.Fatal("expected error for missing log file")
	}
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}
}

func TestReadSHMLog_AllLines(t *testing.T) {
	f, err := os.CreateTemp("", "urnetwork-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("line1\nline2\nline3\n")
	f.Close()

	out, err := readSHMLog(f.Name(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if out != "line1\nline2\nline3\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestNextProxyID_MonotonicallyIncreasing(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 0)

	id0 := nextProxyID()
	id1 := nextProxyID()
	id2 := nextProxyID()

	if id0 != 0 || id1 != 1 || id2 != 2 {
		t.Fatalf("expected 0,1,2 got %d,%d,%d", id0, id1, id2)
	}
}

func TestInitProxyIDCounter_StartsAboveExisting(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 0)
	initProxyIDCounter(10)
	id := nextProxyID()
	if id != 11 {
		t.Fatalf("expected first ID after init to be 11, got %d", id)
	}
}

func TestInitProxyIDCounter_NoopIfAlreadyHigher(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 100)
	initProxyIDCounter(5)
	id := nextProxyID()
	if id != 100 {
		t.Fatalf("expected counter unchanged at 100, got %d", id)
	}
}

func TestReadSHMLog_LastN(t *testing.T) {
	f, err := os.CreateTemp("", "urnetwork-test-*.log")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString("line1\nline2\nline3\nline4\nline5\n")
	f.Close()

	out, err := readSHMLog(f.Name(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if out != "line4\nline5\n" {
		t.Fatalf("unexpected output: %q", out)
	}
}
