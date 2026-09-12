package connect

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestHandshakeReasonClass(t *testing.T) {
	tests := []struct {
		err      error
		expected string
	}{
		{fmt.Errorf("tls handshake timeout after 30s"), "tls_handshake_timeout"},
		{fmt.Errorf("net/http: TLS handshake timeout"), "tls_handshake_timeout"},
		{fmt.Errorf("tls handshake timeout after 1m2s"), "tls_handshake_timeout"},
		{fmt.Errorf("context canceled"), "context_canceled"},
		{fmt.Errorf("context deadline exceeded"), "context_deadline"},
		{fmt.Errorf("connection reset by peer"), "other"},
		{fmt.Errorf("tls: bad certificate"), "certificate_error"},
		{fmt.Errorf("certificate signed by unknown authority"), "certificate_error"},
		{nil, "other"},
		{fmt.Errorf(""), "other"},
	}
	for _, tt := range tests {
		got := handshakeReasonClass(tt.err)
		if got != tt.expected {
			t.Errorf("handshakeReasonClass(%v) = %q, want %q", tt.err, got, tt.expected)
		}
	}
}

// resetHandshakeThrottles clears all entries from the sync.Map so tests
// do not leak state into each other.
func resetHandshakeThrottles() {
	handshakeErrThrottle.Range(func(key, _ any) bool {
		handshakeErrThrottle.Delete(key)
		return true
	})
}

func TestHandshakeThrottle_FirstCallAllowed(t *testing.T) {
	resetHandshakeThrottles()

	ok, suppressed := shouldLogHandshakeErr("tls_handshake_timeout")
	if !ok {
		t.Fatal("first call should be allowed")
	}
	if suppressed != 0 {
		t.Fatalf("first call should report 0 suppressed, got %d", suppressed)
	}
}

func TestHandshakeThrottle_SuppressesWithinWindow(t *testing.T) {
	resetHandshakeThrottles()

	base := time.Now()

	ok, suppressed := shouldLogHandshakeErrAt("tls_handshake_timeout", base)
	if !ok {
		t.Fatal("first call should be allowed")
	}
	if suppressed != 0 {
		t.Fatalf("first call should report 0 suppressed, got %d", suppressed)
	}

	// Simulate rapid calls within the 1-minute window using explicit timestamps.
	for i := 1; i <= 10; i++ {
		ok, _ := shouldLogHandshakeErrAt("tls_handshake_timeout", base.Add(time.Duration(i)*time.Second))
		if ok {
			t.Fatalf("call %d within window should be suppressed", i)
		}
	}

	// After the window, the next call should emit with the suppressed count.
	ok, suppressed = shouldLogHandshakeErrAt("tls_handshake_timeout", base.Add(2*time.Minute))
	if !ok {
		t.Fatal("call after window should be allowed")
	}
	if suppressed != 10 {
		t.Fatalf("expected 10 suppressed, got %d", suppressed)
	}
}

func TestHandshakeThrottle_CountResetsAfterEmit(t *testing.T) {
	resetHandshakeThrottles()

	base := time.Now()

	// Allow the first call.
	ok, suppressed := shouldLogHandshakeErrAt("tls_handshake_timeout", base)
	if !ok {
		t.Fatal("first call should be allowed")
	}
	if suppressed != 0 {
		t.Fatalf("first call should report 0 suppressed, got %d", suppressed)
	}

	// Suppress 3 calls within the window.
	for i := 1; i <= 3; i++ {
		ok, _ := shouldLogHandshakeErrAt("tls_handshake_timeout", base.Add(time.Duration(i)*time.Second))
		if ok {
			t.Fatalf("call %d within window should be suppressed", i)
		}
	}

	// After the window, the next call should emit with suppressed=3.
	ok, suppressed = shouldLogHandshakeErrAt("tls_handshake_timeout", base.Add(2*time.Minute))
	if !ok {
		t.Fatal("should be allowed after window")
	}
	if suppressed != 3 {
		t.Fatalf("expected 3 suppressed, got %d", suppressed)
	}
}

func TestHandshakeThrottle_DifferentClassesThrottleIndependently(t *testing.T) {
	resetHandshakeThrottles()

	// First call for each class — both should be allowed.
	ok1, _ := shouldLogHandshakeErr("tls_handshake_timeout")
	ok2, _ := shouldLogHandshakeErr("context_canceled")
	if !ok1 || !ok2 {
		t.Fatal("first call for each class should be allowed")
	}

	// Second call for each class — both should be suppressed.
	ok1, _ = shouldLogHandshakeErr("tls_handshake_timeout")
	ok2, _ = shouldLogHandshakeErr("context_canceled")
	if ok1 || ok2 {
		t.Fatal("second call within window should be suppressed for both classes")
	}
}

func TestHandshakeThrottle_ConcurrentAccess(t *testing.T) {
	resetHandshakeThrottles()

	const goroutines = 50
	const callsPerGoroutine = 100

	var wg sync.WaitGroup
	allowed := make([]int64, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < callsPerGoroutine; i++ {
				if ok, _ := shouldLogHandshakeErr("tls_handshake_timeout"); ok {
					allowed[id]++
				}
			}
		}(g)
	}
	wg.Wait()

	// Exactly one goroutine should win each interval's CAS.
	// With 50*100=5000 calls and a 1-minute interval, only 1-2 should
	// be allowed (the very first one, plus possibly one more if the test
	// spans the boundary). All goroutines should survive without data
	// races (run with -race).
	totalAllowed := int64(0)
	for _, a := range allowed {
		totalAllowed += a
	}
	if totalAllowed == 0 {
		t.Fatal("at least one call should be allowed")
	}
	// With 5000 calls in a 1-minute window, we expect far fewer than 10
	// allowed calls.
	if totalAllowed > 10 {
		t.Fatalf("expected ≤10 allowed calls under contention, got %d", totalAllowed)
	}
}
