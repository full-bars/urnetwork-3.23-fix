//go:build linux

package main

import (
	"net"
	"os"
	"testing"
	"time"
)

func unsetMetricsEnv(t *testing.T) {
	t.Helper()
	orig, had := os.LookupEnv("URNETWORK_METRICS")
	os.Unsetenv("URNETWORK_METRICS")
	t.Cleanup(func() {
		if had {
			os.Setenv("URNETWORK_METRICS", orig)
		} else {
			os.Unsetenv("URNETWORK_METRICS")
		}
	})
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestMetricsListenerReleasedOnYield: a HotSwap parent must release its
// metrics port when it yields, or the promoted candidate cannot bind it.
func TestMetricsListenerReleasedOnYield(t *testing.T) {
	addr := freeLoopbackAddr(t)
	t.Setenv("URNETWORK_METRICS", addr)
	ClearCoordinatorClosers()
	t.Cleanup(func() { _ = stopMetrics(); ClearCoordinatorClosers() })

	if err := applyMetricsLive("on"); err != nil {
		t.Fatalf("metrics on: %v", err)
	}
	if metricsServer == nil {
		t.Fatal("metrics listener not running")
	}

	yieldCoordinatorSession()

	if metricsServer != nil {
		t.Fatal("yield left the metrics listener running")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("metrics address still held after yield: %v", err)
	}
	ln.Close()
}

// TestCandidateDefersPersistedMetricsUntilTakeover: before takeover the
// parent owns the port, so a persisted metrics=on must not start (it used to
// hop to the next port); after takeover it starts.
func TestCandidateDefersPersistedMetricsUntilTakeover(t *testing.T) {
	unsetMetricsEnv(t)
	if metricsServer != nil {
		t.Fatal("metrics listener already running before the test")
	}
	setPersistedControlValue(t, "metrics", "on")
	metricsHandoffPending.Store(true)
	t.Cleanup(func() { metricsHandoffPending.Store(false); _ = stopMetrics(); ClearCoordinatorClosers() })

	applyPersistedRuntimeTuning(globalControlState)
	if metricsServer != nil {
		t.Fatal("candidate started metrics before takeover")
	}

	startMetricsAfterTakeover(globalControlState)
	if metricsServer == nil {
		t.Fatal("metrics did not start after takeover")
	}
	if metricsHandoffPending.Load() {
		t.Fatal("handoff flag still set after takeover")
	}
}

// TestListenMetricsWaitsForReleasedAddress: after takeover the candidate
// waits for the parent's address instead of failing or moving port.
func TestListenMetricsWaitsForReleasedAddress(t *testing.T) {
	addr := freeLoopbackAddr(t)
	t.Setenv("URNETWORK_METRICS", addr)
	holder, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		holder.Close()
	}()

	ln, err := listenMetrics(3 * time.Second)
	if err != nil {
		t.Fatalf("listenMetrics did not wait for the address: %v", err)
	}
	defer ln.Close()
	if ln.Addr().String() != addr {
		t.Fatalf("bound %s, want %s", ln.Addr(), addr)
	}
}
