package main

import (
	"sync"
	"testing"
)

// TestFlushRetentionEventsSafeAgainstConcurrentAppend guards against a
// send-on-closed-channel panic: appendRetentionEvent can race
// flushRetentionEvents during provider shutdown — the SendSequence teardown
// drain (transfer.go Run() exit path) fires RetentionEventCallback for
// every retained item, which is exactly the same window main's deferred
// flushRetentionEvents runs in. Both must be safe to call concurrently, and
// flush must be idempotent (a second call must not double-close the
// channel).
func TestFlushRetentionEventsSafeAgainstConcurrentAppend(t *testing.T) {
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", t.TempDir())

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				appendRetentionEvent("test_event")
			}
		}
	}()

	// Races the appender goroutine above against the close in flush; a
	// send-on-closed-channel panic here would fail the test (and, under
	// -race, a data race on retentionEventClosed would also be reported).
	flushRetentionEvents()
	close(stop)
	wg.Wait()

	// Idempotent: a second flush must not double-close the channel.
	flushRetentionEvents()

	// Post-flush appends must be a safe no-op, not a panic.
	appendRetentionEvent("post_flush_event")
}
