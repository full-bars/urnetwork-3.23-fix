package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// proxySlowRetryState tracks operator-curated proxies that have entered
// the slow-retry phase (post-maxAuthFailures). Persists to disk so
// reboots don't reset the 14-day drop clock — a proxy that was failing
// before a restart continues its countdown rather than getting a fresh
// 14-day window.
type proxySlowRetryState struct {
	mu      sync.Mutex
	Proxies map[string]*proxySlowRetryEntry `json:"proxies"`
}

type proxySlowRetryEntry struct {
	// StartedAt records when this proxy first entered slow retry.
	// Persists across reboots so the 14-day drop clock is continuous.
	StartedAt time.Time `json:"started_at"`
	// LastAttemptAt records the last time we tried this proxy during
	// slow retry. Used to enforce the 24h daily interval across
	// reboots — a restart won't immediately re-hit a proxy that
	// was already attempted recently.
	LastAttemptAt time.Time `json:"last_attempt_at"`
	// DroppedAt is set when the proxy exceeds slowRetryMaxDuration.
	// Non-nil means the proxy has been removed from the active pool.
	DroppedAt *time.Time `json:"dropped_at,omitempty"`
}

const (
	// slowRetryDailyInterval is the pause between retry attempts once
	// a proxy enters slow retry. 24h: one attempt per day, down from
	// the previous 15min (96x reduction in retry volume).
	slowRetryDailyInterval = 24 * time.Hour

	// slowRetryMaxDuration is how long a proxy may continuously fail
	// in slow retry before being dropped from the active pool. 14 days:
	// long enough for temporary outages (maintenance, billing, vacation)
	// to recover, short enough to stop wasting resources on truly dead
	// proxies. The proxy remains in the operator's config file and will
	// be retried fresh on the next provider restart.
	slowRetryMaxDuration = 14 * 24 * time.Hour

	// proxySlowRetryCleanupAge is how long dropped entries remain in
	// the state file before being pruned on load. 30 days: long enough
	// to see recovery/continued-failure logs on restart, short enough
	// to keep the file small.
	proxySlowRetryCleanupAge = 30 * 24 * time.Hour

	// slowRetryMaxConcurrent is the maximum number of slow-retry auth
	// attempts allowed to run concurrently. Without this, a box with
	// hundreds of dead proxies would thundering-herd the auth API on
	// each daily cycle, potentially overwhelming the rate limiter and
	// causing genuine proxies to fail auth too. The semaphore is
	// acquired before the auth attempt and released after.
	slowRetryMaxConcurrent = 3
)

var (
	globalProxySlowRetryState = newProxySlowRetryState()
	// slowRetrySemaphore limits concurrent slow-retry auth attempts.
	slowRetrySemaphore = make(chan struct{}, slowRetryMaxConcurrent)
)

func newProxySlowRetryState() *proxySlowRetryState {
	return &proxySlowRetryState{Proxies: make(map[string]*proxySlowRetryEntry)}
}

// RecordSlowRetryStart records when this proxy first entered slow retry.
// Subsequent calls for the same address are no-ops (preserves the original
// start time). Returns the entry's StartedAt for use in expiry checks.
func (s *proxySlowRetryState) RecordSlowRetryStart(address string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.Proxies[address]; ok && entry.DroppedAt == nil {
		return entry.StartedAt
	}
	now := time.Now()
	s.Proxies[address] = &proxySlowRetryEntry{StartedAt: now}
	persistProxySlowRetryState(s)
	return now
}

// RecordSlowRetryAttempt records a retry attempt. Returns true if the proxy
// should be attempted now (enforces the 24h daily interval).
func (s *proxySlowRetryState) RecordSlowRetryAttempt(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	if !ok || entry.DroppedAt != nil {
		return false
	}
	now := time.Now()
	if entry.LastAttemptAt.IsZero() || now.Sub(entry.LastAttemptAt) >= slowRetryDailyInterval {
		entry.LastAttemptAt = now
		persistProxySlowRetryState(s)
		return true
	}
	return false
}

// MarkDropped marks a proxy as dropped from the active pool. Returns the
// drop timestamp for use in log messages.
func (s *proxySlowRetryState) MarkDropped(address string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if entry, ok := s.Proxies[address]; ok {
		entry.DroppedAt = &now
	} else {
		s.Proxies[address] = &proxySlowRetryEntry{DroppedAt: &now}
	}
	persistProxySlowRetryState(s)
	return now
}

// ShouldDrop checks if a proxy has been in slow retry longer than
// slowRetryMaxDuration. Uses the persisted StartedAt timestamp so
// reboots don't reset the clock.
func (s *proxySlowRetryState) ShouldDrop(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	if !ok || entry.DroppedAt != nil {
		return false
	}
	return time.Since(entry.StartedAt) >= slowRetryMaxDuration
}

// WasDropped returns true if this address was previously dropped.
// Used at startup to log continued failure or recovery.
func (s *proxySlowRetryState) WasDropped(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	return ok && entry.DroppedAt != nil
}

// DropTime returns when a previously-dropped proxy was dropped, or zero time.
func (s *proxySlowRetryState) DropTime(address string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.Proxies[address]; ok && entry.DroppedAt != nil {
		return *entry.DroppedAt
	}
	return time.Time{}
}

// ClearDropped removes a drop record for an address. Called when a
// previously-dropped proxy successfully authenticates on restart,
// or when the operator refreshes the proxy list.
func (s *proxySlowRetryState) ClearDropped(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Proxies, address)
	persistProxySlowRetryState(s)
}

// LoadProxySlowRetryState loads persisted slow-retry state from disk,
// prunes stale entries (>30d), and returns the live state. If the file
// is missing or corrupt, returns a fresh empty state — this is purely
// observability data and must never block provider startup.
func LoadProxySlowRetryState() *proxySlowRetryState {
	s := newProxySlowRetryState()
	path, err := providerStatePath("proxy-slow-retry.json")
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		tlog("[proxy][slow-retry] warning: corrupt state file, starting fresh: %v\n", err)
		return s
	}
	if s.Proxies == nil {
		s.Proxies = make(map[string]*proxySlowRetryEntry)
		return s
	}
	// Prune stale dropped entries (>30 days old).
	staleThreshold := time.Now().Add(-proxySlowRetryCleanupAge)
	for addr, entry := range s.Proxies {
		if entry.DroppedAt != nil && entry.DroppedAt.Before(staleThreshold) {
			delete(s.Proxies, addr)
		}
	}
	persistProxySlowRetryState(s)
	return s
}

// persistProxySlowRetryState writes the current state to disk using
// atomic write (temp + rename) so a crash mid-write never corrupts the file.
func persistProxySlowRetryState(s *proxySlowRetryState) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	path, err := providerStatePath("proxy-slow-retry.json")
	if err != nil {
		return
	}
	if err := atomicWriteFile(path, data, 0600); err != nil {
		tlog("[proxy][slow-retry] warning: could not persist state: %v\n", err)
	}
}
