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

// TestProxyFailureHistory_ResetClearsCount is a regression test for a bug
// found during fleet log analysis: a proxy that failed several times before
// finally succeeding (or one that drops and reconnects later) kept its
// lifetime failure count forever, permanently biasing the admission gate's
// weighted lottery against it even after it proved itself. Reset must wipe
// the count so a proven proxy competes on equal footing with an untried one.
func TestProxyFailureHistory_RecordGiveUp_TracksPerAddress(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	if got := h.GiveUpCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected unmarked address to have 0 give-ups, got %d", got)
	}

	h.RecordGiveUp("1.2.3.4:1080")
	h.RecordGiveUp("1.2.3.4:1080")

	if got := h.GiveUpCount("1.2.3.4:1080"); got != 2 {
		t.Fatalf("expected 2 recorded give-ups, got %d", got)
	}
	if got := h.GiveUpCount("5.6.7.8:1080"); got != 0 {
		t.Fatalf("expected a different address to be unaffected, got %d", got)
	}
}

func TestProxyFailureHistory_Reset_ClearsGiveUpsToo(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	h.RecordFailure("1.2.3.4:1080")
	h.RecordGiveUp("1.2.3.4:1080")
	h.RecordGiveUp("1.2.3.4:1080")

	h.Reset("1.2.3.4:1080")

	if got := h.FailureCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected failure count cleared after Reset, got %d", got)
	}
	if got := h.GiveUpCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected give-up count cleared after Reset, got %d", got)
	}
}

func TestProxyFailureHistory_Prune_RemovesGiveUpsForDroppedAddresses(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	h.RecordGiveUp("1.2.3.4:1080")
	h.RecordGiveUp("5.6.7.8:1080")

	h.Prune(map[string]bool{"1.2.3.4:1080": true})

	if got := h.GiveUpCount("1.2.3.4:1080"); got != 1 {
		t.Fatalf("expected kept address to retain its give-up count, got %d", got)
	}
	if got := h.GiveUpCount("5.6.7.8:1080"); got != 0 {
		t.Fatalf("expected dropped address's give-up count to be pruned, got %d", got)
	}
}

func TestProxyFailureHistory_ResetClearsCount(t *testing.T) {
	h := &proxyFailureHistory{failures: map[string]int{}}

	for i := 0; i < 5; i++ {
		h.RecordFailure("1.2.3.4:1080")
	}
	if got := h.FailureCount("1.2.3.4:1080"); got != 5 {
		t.Fatalf("expected 5 recorded failures before reset, got %d", got)
	}

	h.Reset("1.2.3.4:1080")

	if got := h.FailureCount("1.2.3.4:1080"); got != 0 {
		t.Fatalf("expected failure count to be cleared after Reset, got %d", got)
	}
}
