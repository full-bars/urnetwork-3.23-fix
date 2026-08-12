package main

import (
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// perProxyEarnTracker tracks, per proxy address, the last time the proxy
// produced a positive billable delta. It exists because earn-skip must key
// off a RECENT liveness signal, never the raw cumulative BillableRx/Tx
// counters — a cumulative counter only resets on process restart, so a
// proxy that earned once early then died would otherwise look "earning"
// forever and never be re-probed (Sonnet design review finding 2c).
type perProxyEarnTracker struct {
	mu sync.Mutex
	// lastEarned maps proxy address -> time of the last positive billable
	// delta observed by the snapshot loop.
	lastEarned map[string]time.Time
	// prevCum holds the previous cumulative (rx+tx) per address for the
	// delta computation.
	prevCum map[string]uint64
}

func newPerProxyEarnTracker() *perProxyEarnTracker {
	return &perProxyEarnTracker{
		lastEarned: map[string]time.Time{},
		prevCum:    map[string]uint64{},
	}
}

// proxyKeyAddress normalizes a proxy-health key to the raw proxy address.
// connect.ProxyHealthSnapshot keys its bandwidth map with the FORMATTED
// key "proxy[N] (addr)" (formatProxyEntry), while the paid grader and the
// tracker's callers key by the raw address "addr". The tracker must
// normalize on ingest or EarnedSince(rawAddr) never matches and earn-skip
// silently never fires — the paid-savings feature would be dead in
// production (review CRITICAL). Raw-address keys pass through unchanged
// (a raw address contains no " (" separator, so parseProxyString returns
// no address half and the key is used as-is).
func proxyKeyAddress(key string) string {
	// parseProxyString splits "proxy[N] (addr)" on " (" and returns the
	// second half as the address. A raw address has no such split, so it
	// returns itself with an empty address half.
	_, addr := parseProxyString(key)
	if addr != "" {
		return addr
	}
	return key
}

// Update snapshots the current per-address billable counters and records
// "earned now" for any address whose counter advanced since the previous
// call. Mirrors runEarningWindows's diff logic but keyed by address.
//
// The snapshot keys may be in the formatted "proxy[N] (addr)" shape
// (ProxyHealthSnapshot) or raw; both are normalized to the raw address.
// Addresses absent from the snapshot are pruned from both maps, so memory
// stays bounded by the live proxy set as proxies churn across the box's
// lifetime (independent review finding).
func (t *perProxyEarnTracker) Update(snapshot map[string]*connect.ProxyBandwidth) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	live := make(map[string]struct{}, len(snapshot))
	for key, bw := range snapshot {
		addr := proxyKeyAddress(key)
		live[addr] = struct{}{}
		cum := bw.BillableRx.Load() + bw.BillableTx.Load()
		prev, seen := t.prevCum[addr]
		if seen {
			// A backwards counter means the proxy restarted and reset its
			// counters; treat as a zero-delta tick (same rule as
			// runEarningWindows). We do NOT mark it as earned — a reset
			// alone is not traffic.
			if cum > prev {
				t.lastEarned[addr] = now
			}
		}
		t.prevCum[addr] = cum
	}
	// Prune addresses that left the live snapshot. Without this the maps
	// grow without bound as proxies are added and removed; a pruned
	// address that returns re-establishes its baseline on first sight
	// (same semantics as a brand-new address).
	for addr := range t.lastEarned {
		if _, ok := live[addr]; !ok {
			delete(t.lastEarned, addr)
		}
	}
	for addr := range t.prevCum {
		if _, ok := live[addr]; !ok {
			delete(t.prevCum, addr)
		}
	}
}

// EarnedSince reports whether the proxy at addr produced a positive
// billable delta within the given window. Unknown addresses report false
// (never seen earning -> not protected by earn-skip).
func (t *perProxyEarnTracker) EarnedSince(addr string, window time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.lastEarned[proxyKeyAddress(addr)]
	if !ok {
		return false
	}
	return time.Since(last) <= window
}

// LastEarned returns the last-earned timestamp for addr, and whether the
// address has ever been observed earning in this process.
func (t *perProxyEarnTracker) LastEarned(addr string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.lastEarned[proxyKeyAddress(addr)]
	return last, ok
}

// globalPerProxyEarnTracker is fed by the same snapshot loop that feeds
// runEarningWindows, so the earn-skip sees the same per-address liveness
// data the [earn] log line reports in aggregate.
var globalPerProxyEarnTracker = newPerProxyEarnTracker()

// earnCheckInterval is how often the per-address earn tracker is updated
// from the proxy health snapshot. Matches the [earn] 1-minute cadence so
// the earn-skip and the [earn] log agree on "actively earning".
const earnCheckInterval = 1 * time.Minute

// paidEarnWindow is how recent a positive billable delta must be for a paid
// proxy to be considered "actively earning" and skipped by the grader.
// Distinct from the 1/5/15/60m [earn] log windows: this is a liveness
// decision, not a report. A proxy that earned within this window is
// demonstrably alive; one that has been quiet for longer gets probed.
const paidEarnWindow = 15 * time.Minute

// paidForceProbeCeiling is the hard upper bound on how long a paid proxy
// can go without a real stage-1 probe, regardless of earn state. Earn-skip
// suppresses probes for actively-earning proxies, but a proxy that has been
// "earning" (or just not quiet long enough) must still be force-probed at
// least this often so the fail-fast path can never be starved indefinitely
// (Sonnet design review findings 2c/4b — the multiplicative hazard with the
// persisted-grade cache).
const paidForceProbeCeiling = 24 * time.Hour
