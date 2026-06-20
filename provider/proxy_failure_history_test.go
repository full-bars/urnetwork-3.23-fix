package main

import "testing"

func TestProxyFailureHistory_TracksPerAddress(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	if got := h.FailureCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected unmarked address to have 0 failures, got %d", got)
	}

	h.RecordFailure("1.2.3.4:1080")
	h.RecordFailure("1.2.3.4:1080")

	if got := h.FailureCount("1.2.3.4:1080"); got != 2 {
		t.Fatalf("expected 2 recorded failures, got %d", got)
	}
	if got := h.FailureCount("5.6.7.8:1080"); got != 0 {
		t.Fatalf("expected a different address to be unaffected, got %d", got)
	}
}

func TestProxyFailureHistory_SurvivesAcrossLaunches(t *testing.T) {
	// Simulates the 15-minute URL-source requeue: a fresh launch resets a
	// local attempt counter to zero, but the lifetime history must not
	// forget how many times this address has already failed.
	h := &proxyFailureHistory{failures: map[string]int{}}

	for i := 0; i < 3; i++ {
		h.RecordFailure("9.9.9.9:1080")
	}
	localAttemptCounter := 0 // reset on relaunch

	if got := h.FailureCount("9.9.9.9:1080"); got != 3 {
		t.Fatalf("expected lifetime history to retain 3 failures across a simulated relaunch, got %d", got)
	}
	if localAttemptCounter != 0 {
		t.Fatalf("sanity check failed: local counter should be reset")
	}
}
