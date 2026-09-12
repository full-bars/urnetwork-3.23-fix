package connect

import (
	"sync"
	"testing"
	"time"
)

func TestAuditSendThrottle_FirstCallAllowed(t *testing.T) {
	// Reset the throttle by replacing it.
	auditSendErrThrottle = newLogThrottle(time.Minute)

	ok, suppressed := auditSendErrThrottle.Allow(time.Now())
	if !ok {
		t.Fatal("first call should be allowed")
	}
	if suppressed != 0 {
		t.Fatalf("first call should report 0 suppressed, got %d", suppressed)
	}
}

func TestAuditSendThrottle_SuppressesWithinWindow(t *testing.T) {
	auditSendErrThrottle = newLogThrottle(time.Minute)

	auditSendErrThrottle.Allow(time.Now()) // first allowed

	// Next 5 calls within the window should be suppressed.
	for i := 0; i < 5; i++ {
		ok, _ := auditSendErrThrottle.Allow(time.Now())
		if ok {
			t.Fatalf("call %d within window should be suppressed", i+1)
		}
	}
}

func TestAuditSendThrottle_CountResetsAfterEmit(t *testing.T) {
	auditSendErrThrottle = newLogThrottle(time.Minute)

	auditSendErrThrottle.Allow(time.Now()) // allowed
	auditSendErrThrottle.Allow(time.Now()) // suppressed -> count 1
	auditSendErrThrottle.Allow(time.Now()) // suppressed -> count 2

	// Create a new throttle to simulate time passing.
	auditSendErrThrottle = newLogThrottle(time.Minute)
	ok, suppressed := auditSendErrThrottle.Allow(time.Now())
	if !ok {
		t.Fatal("fresh throttle should allow first call")
	}
	if suppressed != 0 {
		t.Fatalf("fresh throttle should report 0 suppressed, got %d", suppressed)
	}
}

func TestAuditSendThrottle_ConcurrentAccess(t *testing.T) {
	auditSendErrThrottle = newLogThrottle(time.Minute)

	const goroutines = 50
	const callsPerGoroutine = 100

	var wg sync.WaitGroup
	allowed := make([]int64, goroutines)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < callsPerGoroutine; i++ {
				if ok, _ := auditSendErrThrottle.Allow(time.Now()); ok {
					allowed[id]++
				}
			}
		}(g)
	}
	wg.Wait()

	totalAllowed := int64(0)
	for _, a := range allowed {
		totalAllowed += a
	}
	if totalAllowed == 0 {
		t.Fatal("at least one call should be allowed")
	}
	if totalAllowed > 10 {
		t.Fatalf("expected ≤10 allowed calls under contention, got %d", totalAllowed)
	}
}
