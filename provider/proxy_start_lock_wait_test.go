package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A lock holder that outlives the old five-attempt, 100 ms retry window is a
// slow but live process, not a reason to abort provider startup. Startup must
// still be waiting after that window and complete once the holder lets go.
func TestProxyStartSurvivesLockHeldPastOldRetryWindow(t *testing.T) {
	withTempHome(t)
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	finished := make(chan error, 1)
	go func() {
		rel, err := acquireProxyLockWait(context.Background(), proxyStartLockWait)
		if err == nil && rel != nil {
			rel()
		}
		finished <- err
	}()

	select {
	case err := <-finished:
		t.Fatalf("startup ended while the lock was held past the old retry window: %v", err)
	case <-time.After(900 * time.Millisecond):
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("startup failed after lock release: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup lock wait did not finish after release")
	}
}

// The wait is bounded by maxWait and context cancellation.
func TestProxyStartLockWaitIsBoundedAndCancellable(t *testing.T) {
	withTempHome(t)
	release, err := acquireProxyLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	// 1. Timeout bound
	start := time.Now()
	_, err = acquireProxyLockWait(context.Background(), 250*time.Millisecond)
	if err == nil {
		t.Fatal("expected error on timeout, got nil")
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("returned too early: %v", elapsed)
	}

	// 2. Context cancellation
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = acquireProxyLockWait(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// acquireProxyLockWait waits only while the lock is held by another process;
// an unrecoverable failure (e.g. read-only or invalid directory) returns immediately.
func TestProxyLockWaitOnlyWaitsWhileHeld(t *testing.T) {
	withTempHome(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(home, ".urnetwork")
	// Create a regular file in place of the directory so MkdirAll fails
	if err := os.RemoveAll(lockDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockDir, []byte("blocker"), 0600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = acquireProxyLockWait(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("expected error when lock directory is uncreatable, got nil")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("unheld error took too long to return: %v", time.Since(start))
	}
}
