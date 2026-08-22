package connect

import (
	"sync"
	"testing"
	"time"
)

func TestShouldLogSelectErr_RateLimit(t *testing.T) {
	// Reset state
	selectErrThrottle.lastNanos.Store(0)
	selectErrThrottle.suppressed.Store(0)

	// First call should succeed and log
	ok, suppressed := shouldLogSelectErr()
	if !ok {
		t.Errorf("First call expected to log, but was rate-limited")
	}
	if suppressed != 0 {
		t.Errorf("First call expected 0 suppressed, got %d", suppressed)
	}

	// Immediate second call should be rate-limited
	ok, _ = shouldLogSelectErr()
	if ok {
		t.Errorf("Immediate second call expected to be rate-limited, but allowed")
	}

	// Verify suppression count incremented
	if count := selectErrThrottle.suppressed.Load(); count != 1 {
		t.Errorf("Expected suppressed count 1, got %d", count)
	}

	// Simulate 61 seconds passing
	past := time.Now().Add(-61 * time.Second).UnixNano()
	selectErrThrottle.lastNanos.Store(past)

	// Call after 1 min should succeed and return suppressed count
	ok, suppressed = shouldLogSelectErr()
	if !ok {
		t.Errorf("Call after 1 min expected to log, but was rate-limited")
	}
	if suppressed != 1 {
		t.Errorf("Expected 1 suppressed count returned, got %d", suppressed)
	}

	// Suppressed count should be reset
	if count := selectErrThrottle.suppressed.Load(); count != 0 {
		t.Errorf("Expected suppressed count to reset to 0, got %d", count)
	}
}

func TestShouldLogSelectErr_Concurrency(t *testing.T) {
	// Reset state
	selectErrThrottle.lastNanos.Store(0)
	selectErrThrottle.suppressed.Store(0)

	// First call allows one through
	shouldLogSelectErr()

	// Simulate many concurrent failures
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := shouldLogSelectErr()
			if ok {
				t.Errorf("Concurrent calls within 1m should all be rate-limited")
			}
		}()
	}
	wg.Wait()

	if count := selectErrThrottle.suppressed.Load(); count != 100 {
		t.Errorf("Expected exactly 100 suppressed calls, got %d", count)
	}
}
