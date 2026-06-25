package main

import (
	"sync"
	"time"
)

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
	giveUps  map[string]int

	// backoffUntil records, per URL-sourced address, the earliest time it may
	// be relaunched. It is set on each give-up to now+proxyURLGiveUpRetryDelay.
	// The reload path consults Eligible() and skips re-adding an address whose
	// window has not elapsed, so the escalating backoff is actually enforced:
	// without this, the give-up only scheduled a one-shot time.AfterFunc reload
	// nudge, and any *other* reload (another proxy's give-up, a URL refresh)
	// relaunched the dead proxy immediately, defeating the backoff entirely.
	backoffUntil map[string]time.Time
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

// RecordGiveUp records another give-up cycle for address: the proxy
// exhausted its per-launch auth attempts and is backing off before its next
// requeue. Tracked separately from RecordFailure, which counts individual
// auth attempts within a cycle — RecordGiveUp counts only the cycles
// themselves, which is what drives the escalating requeue delay
// (proxyURLGiveUpRetryDelay) and the eventual eviction threshold
// (proxyURLGiveUpEvictAfterCycles). Returns the new lifetime total.
func (h *proxyFailureHistory) RecordGiveUp(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.giveUps == nil {
		h.giveUps = map[string]int{}
	}
	h.giveUps[address]++
	return h.giveUps[address]
}

// GiveUpCount reports address's lifetime give-up count.
func (h *proxyFailureHistory) GiveUpCount(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.giveUps[address]
}

// SetBackoffUntil records the earliest time address may be relaunched. Called
// at each give-up with now+proxyURLGiveUpRetryDelay(giveUpCount) so the reload
// path can enforce the escalating backoff instead of merely scheduling it.
func (h *proxyFailureHistory) SetBackoffUntil(address string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.backoffUntil == nil {
		h.backoffUntil = map[string]time.Time{}
	}
	h.backoffUntil[address] = until
}

// Eligible reports whether address may be (re)launched as of now: true if it
// has no recorded backoff window, or that window has elapsed. An address with
// no entry (never gave up, or freshly reset/pruned) is always eligible.
func (h *proxyFailureHistory) Eligible(address string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.backoffUntil[address]
	if !ok {
		return true
	}
	return !now.Before(until)
}

// Reset clears address's failure and give-up counts, called when it
// successfully authenticates. Without this, a proxy that failed several
// times before finally succeeding (or one that succeeds, drops, and
// reconnects later) keeps the admission gate's weighted lottery permanently
// biased against it based on failures that are no longer representative —
// a proven-working proxy should compete on equal footing with an untried
// one, not carry a scar from before it proved itself.
func (h *proxyFailureHistory) Reset(address string) {
	h.mu.Lock()
	delete(h.failures, address)
	delete(h.giveUps, address)
	delete(h.backoffUntil, address)
	h.mu.Unlock()
}

// Prune removes entries for addresses not in keepAddrs, called periodically
// to prevent unbounded growth from proxies that cycled out of the fleet.
func (h *proxyFailureHistory) Prune(keepAddrs map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for addr := range h.failures {
		if !keepAddrs[addr] {
			delete(h.failures, addr)
		}
	}
	for addr := range h.giveUps {
		if !keepAddrs[addr] {
			delete(h.giveUps, addr)
		}
	}
	for addr := range h.backoffUntil {
		if !keepAddrs[addr] {
			delete(h.backoffUntil, addr)
		}
	}
}
