package main

import "sync"

// proxyFailureHistory tracks, for the lifetime of this process, how many
// auth attempts have failed for a given proxy address — across every
// launch, including any requeue after maxAuthFailures gives up and the
// URL-source fetcher brings the address back in. Without this, a
// chronically dead proxy would keep coming back into the admission lottery
// at full "never tried" priority every requeue, since the per-launch
// attempt counter resets to zero each time a proxy is relaunched.
type proxyFailureHistory struct {
	mu       sync.Mutex
	failures map[string]int
}

var globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}

// RecordFailure records another failed auth attempt for address and returns
// the new lifetime total.
func (h *proxyFailureHistory) RecordFailure(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures[address]++
	return h.failures[address]
}

// FailureCount reports address's lifetime failure count.
func (h *proxyFailureHistory) FailureCount(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failures[address]
}
