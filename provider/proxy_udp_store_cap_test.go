package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestUDPStoreSweepCapsCompletedEntries(t *testing.T) {
	now := time.Now()
	store := newProxyUDPCheckStore(nil)
	for i := 0; i < 4*udpStateMaxEntries+2; i++ {
		store.entries[fmt.Sprintf("%05d", i)] = &proxyUDPCheckEntry{at: now.Add(-time.Duration(4*udpStateMaxEntries+2-i) * time.Second), result: proxyUDPResult{Status: proxyUDPNoRelay}}
	}
	store.entries["running"] = &proxyUDPCheckEntry{done: make(chan struct{})}
	runUDPProbeTick(context.Background(), store, nil, now, time.Hour, time.Minute, 0, true)
	if got := len(store.entries); got != 4*udpStateMaxEntries {
		t.Fatalf("got %d entries", got)
	}
	if store.entries["running"] == nil {
		t.Fatal("in-flight entry removed")
	}
	for _, key := range []string{"00000", "00001", "00002"} {
		if store.entries[key] != nil {
			t.Fatalf("old entry retained: %s", key)
		}
	}
	if store.entries["00003"] == nil {
		t.Fatal("newer entry removed")
	}
}
