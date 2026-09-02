package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestEarnTracker_SnapshotKeyFormatMatchesProduction pins the review
// CRITICAL: connect.ProxyHealthSnapshot keys its bandwidth map with the
// FORMATTED "proxy[N] (addr)" key (formatProxyEntry), not the raw
// address. The earn tracker must normalize that key back to the raw
// address, or EarnedSince(rawAddr) never matches and the paid grader's
// earn-skip silently never fires — the whole v28.1 paid-savings feature
// was dead in production for this reason. This test drives the tracker
// through the REAL snapshot API, not a hand-built map.
func TestEarnTracker_SnapshotKeyFormatMatchesProduction(t *testing.T) {
	const idx = 9001
	const addr = "198.51.100.7:443"
	defer connect.UnregisterProxy(idx)

	bw := connect.RegisterProxyBandwidth(idx)
	connect.RegisterProxy(idx, addr)

	// First snapshot establishes the per-address baseline (no delta).
	_, _, _, snap1, _ := connect.ProxyHealthSnapshot()
	globalPerProxyEarnTracker.Update(snap1)
	if globalPerProxyEarnTracker.EarnedSince(addr, time.Minute) {
		t.Fatal("baseline tick must not mark earned (first sight has no prior counter)")
	}

	// Advance the counter and snapshot again: the positive delta must be
	// recorded under the RAW address key.
	bw.BillableRx.Store(1 << 20)
	_, _, _, snap2, _ := connect.ProxyHealthSnapshot()
	globalPerProxyEarnTracker.Update(snap2)

	if !globalPerProxyEarnTracker.EarnedSince(addr, time.Minute) {
		t.Fatal("earn tracker must recognize earning under the raw address — snapshot keys are formatted (proxy[N] (addr)) and must be normalized")
	}
	if last, ok := globalPerProxyEarnTracker.LastEarned(addr); !ok || time.Since(last) > time.Minute {
		t.Fatalf("LastEarned(%q) = %v, %v; want recent timestamp, true", addr, last, ok)
	}
}

// TestEarnTracker_RawKeysPassThrough pins that the tracker still accepts
// raw-address keys (the grader and the tests feed raw addresses; a raw
// key contains no "proxy[N] (" prefix and must be used unchanged).
func TestEarnTracker_RawKeysPassThrough(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const addr = "203.0.113.9:443"
	bw := &connect.ProxyBandwidth{}
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	bw.BillableRx.Store(4096)
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	if !tr.EarnedSince(addr, time.Minute) {
		t.Fatal("raw-address keys must be tracked as-is")
	}
}

// TestEarnTracker_PrunesChurnedAddresses pins the unbounded-map-growth
// finding: the lastEarned/prevCum maps
// must be pruned to the live snapshot set on every Update, or they grow
// forever as proxies churn across the box's lifetime.
func TestEarnTracker_PrunesChurnedAddresses(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const a1 = "203.0.113.1:443"
	const a2 = "203.0.113.2:443"
	bw1 := &connect.ProxyBandwidth{}
	bw2 := &connect.ProxyBandwidth{}

	// a1 earns.
	tr.Update(map[string]*connect.ProxyBandwidth{a1: bw1})
	bw1.BillableRx.Store(100)
	tr.Update(map[string]*connect.ProxyBandwidth{a1: bw1})
	if !tr.EarnedSince(a1, time.Minute) {
		t.Fatal("a1 must be marked earned before the churn")
	}

	// a2 only now: a1 left the live set and must be pruned.
	tr.Update(map[string]*connect.ProxyBandwidth{a2: bw2})
	if _, ok := tr.LastEarned(a1); ok {
		t.Fatal("churned address must be pruned from lastEarned")
	}
	if _, ok := tr.prevCum[a1]; ok {
		t.Fatal("churned address must be pruned from prevCum")
	}

	// a2's first sight is a baseline, not earned.
	if tr.EarnedSince(a2, time.Minute) {
		t.Fatal("first sight of a2 must be a baseline, not earned")
	}

	// a2 advances -> earned; a re-added a1 starts fresh (baseline again).
	bw2.BillableRx.Store(50)
	tr.Update(map[string]*connect.ProxyBandwidth{a2: bw2})
	if !tr.EarnedSince(a2, time.Minute) {
		t.Fatal("a2 must be marked earned after its counter advances")
	}
	bw1.BillableRx.Store(200)
	tr.Update(map[string]*connect.ProxyBandwidth{a1: bw1})
	if tr.EarnedSince(a1, time.Minute) {
		t.Fatal("re-added address must re-establish its baseline before earning")
	}
}

// TestProxyKeyAddress_NormalizesFormats pins proxyKeyAddress's contract
// directly: formatted "proxy[N] (addr)" keys (the shape
// connect.ProxyHealthSnapshot actually produces) normalize to the raw
// address, while a raw address (no " (" separator) passes through
// unchanged.
func TestProxyKeyAddress_NormalizesFormats(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"formatted key", "proxy[3] (198.51.100.1:443)", "198.51.100.1:443"},
		{"formatted key, index 0", "proxy[0] (10.0.0.1:1080)", "10.0.0.1:1080"},
		{"raw address", "198.51.100.1:443", "198.51.100.1:443"},
		{"raw address with no port", "198.51.100.1", "198.51.100.1"},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		if got := proxyKeyAddress(c.key); got != c.want {
			t.Errorf("%s: proxyKeyAddress(%q) = %q, want %q", c.name, c.key, got, c.want)
		}
	}
}

// TestEarnTracker_EarnedSince_UnknownAddress pins the never-seen default:
// an address the tracker has never observed must report false (not
// earning) rather than panicking or defaulting to true — an unknown
// address must never be treated as protected by earn-skip.
func TestEarnTracker_EarnedSince_UnknownAddress(t *testing.T) {
	tr := newPerProxyEarnTracker()
	if tr.EarnedSince("203.0.113.99:443", time.Hour) {
		t.Fatal("never-seen address must not be reported as earning")
	}
	if _, ok := tr.LastEarned("203.0.113.99:443"); ok {
		t.Fatal("never-seen address must report ok=false from LastEarned")
	}
}

// TestEarnTracker_EarnedSince_WindowBoundary pins the <= boundary of the
// EarnedSince window check: an earn event exactly at (or just inside) the
// window must count, one just past it must not.
func TestEarnTracker_EarnedSince_WindowBoundary(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const addr = "203.0.113.5:443"
	const window = time.Minute

	tr.mu.Lock()
	tr.lastEarned[addr] = time.Now().Add(-window + time.Second)
	tr.mu.Unlock()
	if !tr.EarnedSince(addr, window) {
		t.Fatal("an earn event just inside the window must count")
	}

	tr.mu.Lock()
	tr.lastEarned[addr] = time.Now().Add(-window - time.Second)
	tr.mu.Unlock()
	if tr.EarnedSince(addr, window) {
		t.Fatal("an earn event just outside the window must not count")
	}
}

// TestEarnTracker_MultipleAddressesIndependent pins that Update tracks
// each address's earn state independently within a single snapshot: one
// address earning must not mark an unrelated, non-earning address as
// earning too.
func TestEarnTracker_MultipleAddressesIndependent(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const earning = "203.0.113.10:443"
	const quiet = "203.0.113.11:443"
	bwEarning := &connect.ProxyBandwidth{}
	bwQuiet := &connect.ProxyBandwidth{}

	tr.Update(map[string]*connect.ProxyBandwidth{earning: bwEarning, quiet: bwQuiet})
	bwEarning.BillableTx.Store(2048) // only the "earning" address advances
	tr.Update(map[string]*connect.ProxyBandwidth{earning: bwEarning, quiet: bwQuiet})

	if !tr.EarnedSince(earning, time.Minute) {
		t.Fatal("address with a positive delta must be marked earned")
	}
	if tr.EarnedSince(quiet, time.Minute) {
		t.Fatal("address with no delta must not be marked earned, regardless of a sibling's activity")
	}
}

// TestEarnTracker_ZeroDeltaTickIsNotEarned pins that an unchanged
// cumulative counter (cum == prev, no traffic since the last tick) is
// never treated as earning — only a strictly positive delta counts.
func TestEarnTracker_ZeroDeltaTickIsNotEarned(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const addr = "203.0.113.12:443"
	bw := &connect.ProxyBandwidth{}
	bw.BillableRx.Store(1000)
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw}) // baseline at 1000
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw}) // still 1000: no delta
	if tr.EarnedSince(addr, time.Minute) {
		t.Fatal("an unchanged cumulative counter must not be treated as earning")
	}
}

// TestEarnTracker_EmptySnapshotPrunesAll pins this behavior:
// Update with an empty (or nil) snapshot must prune ALL per-address
// state — this is what the runEarningWindows empty-health-set branch
// calls so proxies that disappear (health count drops to 0) cannot keep
// stale "earning" state that would wrongly suppress a later probe.
func TestEarnTracker_EmptySnapshotPrunesAll(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const addr = "203.0.113.4:443"
	bw := &connect.ProxyBandwidth{}
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	bw.BillableRx.Store(1024)
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	if !tr.EarnedSince(addr, time.Minute) {
		t.Fatal("addr must be earning before the empty snapshot")
	}
	// Empty health set: every address must be pruned.
	tr.Update(nil)
	if tr.EarnedSince(addr, time.Minute) {
		t.Fatal("empty snapshot must prune the earning state — a re-added proxy must not look 'earning' from stale state")
	}
	if _, ok := tr.LastEarned(addr); ok {
		t.Fatal("empty snapshot must prune lastEarned")
	}
	if _, ok := tr.prevCum[addr]; ok {
		t.Fatal("empty snapshot must prune prevCum")
	}
}

// TestEarnTracker_BackwardsCounterIsNotEarned pins the proxy-restart
// rule: a counter that goes backwards (proxy restarted and reset its
// counters) is a zero-delta tick, never an earn event.
func TestEarnTracker_BackwardsCounterIsNotEarned(t *testing.T) {
	tr := newPerProxyEarnTracker()
	const addr = "203.0.113.3:443"
	bw := &connect.ProxyBandwidth{}
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	bw.BillableRx.Store(5000)
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	if !tr.EarnedSince(addr, time.Minute) {
		t.Fatal("positive delta must mark earned")
	}
	// Simulate a restart: counters reset to a lower value.
	bw.BillableRx.Store(0)
	tr.Update(map[string]*connect.ProxyBandwidth{addr: bw})
	// LastEarned must still hold the PREVIOUS earn time — a backwards
	// counter is not an earn event and must not advance the clock to now
	// in a way that could re-trigger anything; it also must not wipe the
	// previous earn record (the proxy was alive then).
	if last, ok := tr.LastEarned(addr); !ok || time.Since(last) > time.Minute {
		t.Fatalf("backwards counter must not erase the prior earn record: %v, %v", last, ok)
	}
}
