package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

// TestH1ConnectionProcessStatsIsAProcessCollector checks that the provider
// gauge starts as an empty collector before any H1 connection has registered.
func TestH1ConnectionProcessStatsIsAProcessCollector(t *testing.T) {
	var c connect.H1ConnectionStats
	if c.Snapshot().WebSocketConnectionCount != 0 || c.Snapshot().H1PlusConnectionCount != 0 {
		t.Fatalf("new collector should be zero: %+v", c.Snapshot())
	}
}
