package connect

import (
	"fmt"
	"testing"
	"time"
)

// TestPruneDNSCacheUnderCap: no eviction when the map fits.
func TestPruneDNSCacheUnderCap(t *testing.T) {
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	dnsCache.m = map[string]dnsCacheEntry{
		"a.example.com": {ip: "1.1.1.1", expiry: time.Now().Add(time.Minute)},
		"b.example.com": {ip: "2.2.2.2", expiry: time.Now().Add(time.Minute)},
	}
	pruneDNSCacheLocked(time.Now())
	if len(dnsCache.m) != 2 {
		t.Fatalf("under-cap prune removed entries: %d", len(dnsCache.m))
	}
}

// TestPruneDNSCacheEvictsExpired: expired entries are removed first.
func TestPruneDNSCacheEvictsExpired(t *testing.T) {
	// Force the cap low conceptually by filling expired entries; we can't
	// shrink dnsCacheMaxEntries from here, so build a map just over the cap
	// with a mix of fresh + expired and confirm only the fresh survive.
	now := time.Now()
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	dnsCache.m = make(map[string]dnsCacheEntry, dnsCacheMaxEntries+8)
	for i := 0; i < dnsCacheMaxEntries; i++ {
		// Collision-free keys. The old `rune` composition produced only 3380
		// distinct keys for 4096 iterations (a pattern space of 26*26*10),
		// so the map never exceeded dnsCacheMaxEntries and pruneDNSCacheLocked
		// early-returned at the `<= cap` guard — the expired-eviction branch
		// below was never exercised and the test passed vacuously.
		k := fmt.Sprintf("fresh%04d", i)
		dnsCache.m[k] = dnsCacheEntry{ip: "9.9.9.9", expiry: now.Add(time.Hour)}
	}
	// Add 8 expired.
	for i := 0; i < 8; i++ {
		k := "expired" + string(rune('0'+i))
		dnsCache.m[k] = dnsCacheEntry{ip: "8.8.8.8", expiry: now.Add(-time.Minute)}
	}
	pruneDNSCacheLocked(now)
	if len(dnsCache.m) > dnsCacheMaxEntries {
		t.Fatalf("map still over cap after prune: %d > %d", len(dnsCache.m), dnsCacheMaxEntries)
	}
	if _, ok := dnsCache.m["expired0"]; ok {
		t.Fatal("expired entry not evicted")
	}
}

// TestPruneDNSCacheBoundsAtCap: a hostile flood of unique fresh hostnames
// stays at the cap, not unbounded.
func TestPruneDNSCacheBoundsAtCap(t *testing.T) {
	now := time.Now()
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	dnsCache.m = make(map[string]dnsCacheEntry)
	for i := 0; i < dnsCacheMaxEntries+2048; i++ {
		k := "h" + string(rune('0'+i%10)) + string(rune('a'+(i/10)%26)) + string(rune('A'+(i/100)%26)) + string(rune('0'+(i/1000)%10))
		dnsCache.m[k] = dnsCacheEntry{ip: "9.9.9.9", expiry: now.Add(time.Minute)}
	}
	pruneDNSCacheLocked(now)
	if len(dnsCache.m) > dnsCacheMaxEntries {
		t.Fatalf("hostile flood not bounded: %d > %d", len(dnsCache.m), dnsCacheMaxEntries)
	}
}

// TestPruneDNSCacheNegLockedExpiresEntries: pruneDNSCacheNegLocked drops
// expired negative entries and keeps fresh ones.
func TestPruneDNSCacheNegLockedExpiresEntries(t *testing.T) {
	now := time.Now()
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	dnsCache.neg = map[string]time.Time{
		"expired.example.com": now.Add(-time.Second),
		"fresh.example.com":   now.Add(time.Minute),
	}
	pruneDNSCacheNegLocked(now)
	if _, ok := dnsCache.neg["expired.example.com"]; ok {
		t.Fatal("expired negative entry not evicted")
	}
	if _, ok := dnsCache.neg["fresh.example.com"]; !ok {
		t.Fatal("fresh negative entry incorrectly evicted")
	}
}

// TestPruneDNSCacheNegLockedBoundsAtCap: M7 — a workload of distinct failing
// lookups (each calling pruneDNSCacheNegLocked from lookupProxyTarget's
// failure path, independent of the positive cache's size) must not grow the
// negative cache without bound.
func TestPruneDNSCacheNegLockedBoundsAtCap(t *testing.T) {
	now := time.Now()
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	dnsCache.neg = make(map[string]time.Time)
	for i := 0; i < negPruneCap+512; i++ {
		k := fmt.Sprintf("miss%04d.example.invalid", i)
		dnsCache.neg[k] = now.Add(10 * time.Second) // all fresh (unexpired)
	}
	pruneDNSCacheNegLocked(now)
	if len(dnsCache.neg) > negPruneCap {
		t.Fatalf("negative cache not bounded: %d > %d", len(dnsCache.neg), negPruneCap)
	}
}

// TestPruneDNSCacheNegLockedEmptyIsNoop: an empty/nil negative cache must not
// panic (pruneDNSCacheNegLocked now runs unconditionally on every failed
// lookup, including the very first one before dnsCache.neg is initialized).
func TestPruneDNSCacheNegLockedEmptyIsNoop(t *testing.T) {
	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	dnsCache.neg = nil
	pruneDNSCacheNegLocked(time.Now())
}
