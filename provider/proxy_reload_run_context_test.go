package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A reload queued behind a held slot aborts immediately when stop is signalled,
// instead of blocking for reloadSlotTimeout.
func TestReloadWaitingForSlotHonorsStop(t *testing.T) {
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: &sync.Mutex{},
	}
	reloader.initStop()

	// Hold r.mu so acquireReloadSlot must wait
	reloader.mu.Lock()
	defer reloader.mu.Unlock()

	finished := make(chan bool, 1)
	go func() {
		finished <- reloader.acquireReloadSlot()
	}()

	select {
	case <-finished:
		t.Fatal("acquireReloadSlot returned while mu is held before stop")
	case <-time.After(100 * time.Millisecond):
	}

	reloader.stop()

	select {
	case acquired := <-finished:
		if acquired {
			t.Fatal("acquired reload slot after stop")
		}
	case <-time.After(time.Second):
		t.Fatal("acquireReloadSlot did not exit promptly after stop")
	}
}

// quiesceForRun stops the reloader when the run's context is ended so waiting
// reloads let go before quiescing.
func TestRunQuiesceStopsTheReloaderWhenTheRunIsEnding(t *testing.T) {
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: &sync.Mutex{},
	}
	endedCtx, cancel := context.WithCancel(context.Background())
	cancel()

	quiesceForRun(endedCtx, reloader)
	if !reloader.isStopped() {
		t.Fatal("reloader was not stopped by quiesceForRun on ended context")
	}
}

// While the run is live quiesceForRun does not stop the reloader.
func TestRunQuiesceLeavesTheReloaderRunningWhileTheRunIsLive(t *testing.T) {
	reloader := &ProxyReloader{
		cancelMap:   map[string]context.CancelFunc{},
		cancelMapMu: &sync.Mutex{},
	}
	quiesceForRun(context.Background(), reloader)
	if reloader.isStopped() {
		t.Fatal("reloader was stopped by quiesceForRun while run is live")
	}
}
