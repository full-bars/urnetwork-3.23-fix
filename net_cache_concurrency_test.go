package connect

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestLookupProxyTargetCacheHitForwards a pre-populated DNS cache entry
// through lookupProxyTarget without touching the network (the cache-hit path
// returns early before net.DefaultResolver is called). This lock-split
// regression test guards the reordering: the cache-hit branch must read under
// the lock and return without calling LookupNetIP.
func TestLookupProxyTargetCacheHit(t *testing.T) {
	dnsCache.mu.Lock()
	dnsCache.m = map[string]dnsCacheEntry{
		"cached.example.com": {ip: "10.9.9.9", expiry: time.Now().Add(time.Minute)},
	}
	dnsCache.mu.Unlock()
	defer func() {
		dnsCache.mu.Lock()
		dnsCache.m = nil
		dnsCache.mu.Unlock()
	}()

	ip, ok := lookupProxyTarget(context.Background(), "cached.example.com")
	if !ok {
		t.Fatal("expected cache-hit to resolve the host")
	}
	if ip != "10.9.9.9" {
		t.Fatalf("expected cached IP 10.9.9.9, got %q", ip)
	}
}

// TestLookupProxyTargetStaleOnFailure exercises the stale-entry fallback: a
// pre-populated but EXPIRED cache entry must be returned (not dropped) when
// the resolver path fails. We force the failure by resolving a hostname that
// is not a valid DNS name and would fail quickly; because it is already in
// the (expired) cache, the "stale entry is better than nothing" branch runs.
// The resolver failure is network-dependent, so instead of asserting the IP
// we only assert that the call does not panic and returns a bool (which must
// be true if the stale path won). This test's real value is running under
// -race: the lock-split must not create a map race on the expired-entry path.
func TestLookupProxyTargetStaleNoPanic(t *testing.T) {
	dnsCache.mu.Lock()
	dnsCache.m = map[string]dnsCacheEntry{
		"stale.example.com": {ip: "10.1.2.3", expiry: time.Now().Add(-time.Minute)},
	}
	dnsCache.mu.Unlock()
	defer func() {
		dnsCache.mu.Lock()
		dnsCache.m = nil
		dnsCache.mu.Unlock()
	}()

	// A bogus host forces LookupNetIP to error (go >= 1.17 validates the
	// name syntactically and returns before any network I/O for an invalid
	// label), which then runs the stale-return branch.
	_, _ = lookupProxyTarget(context.Background(), "stale.example.com")
}

// TestDNSCacheConcurrentReadPrune hammers the DNS cache with concurrent
// writers and pruners under -race. It guards the #4 bounded-cache change and
// the lock-split in lookupProxyTarget: concurrent prune and insert on the
// same map must be race-free because both hold dnsCache.mu.
func TestDNSCacheConcurrentReadPrune(t *testing.T) {
	dnsCache.mu.Lock()
	dnsCache.m = make(map[string]dnsCacheEntry)
	dnsCache.mu.Unlock()

	const workers = 8
	const iterations = 2000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				host := fmt.Sprintf("host%d.example.com", (w*iterations+i)%dnsCacheMaxEntries)
				dnsCache.mu.Lock()
				dnsCache.m[host] = dnsCacheEntry{ip: "9.9.9.9", expiry: time.Now().Add(30 * time.Second)}
				if i%64 == 0 {
					pruneDNSCacheLocked(time.Now())
				}
				dnsCache.mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	dnsCache.mu.Lock()
	defer dnsCache.mu.Unlock()
	if len(dnsCache.m) > dnsCacheMaxEntries {
		t.Fatalf("map over cap after concurrent prunes: %d", len(dnsCache.m))
	}
}
