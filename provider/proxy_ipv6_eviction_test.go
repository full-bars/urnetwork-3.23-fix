package main

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A running extra IPv6 transport rests on the real-check pair (completedAt,
// realStatus) of its identity's entry. That pair lives in memory only, so a
// capacity eviction used to drop it: the entry came back from the saved Unknown
// flow row, the next poll read Ready with a zero CheckedAt, and the transport
// stayed up past the real UDP expiry it should have stopped at. An identity
// that runs no transport must stay evictable, so the cap still holds.
func TestCapacityEvictionKeepsARunningIPv6TransportExpiry(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	checked := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	expires := checked.Add(proxyUDPOKTTL)
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	key := settings.Key()
	idle := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.2:1080"}
	okExit := proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1"}

	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(filepath.Join(t.TempDir(), "udp-state.json"), t0)
	store.mu.Lock()
	store.entries[key] = &proxyUDPCheckEntry{
		at: t0, completedAt: t0, realStatus: proxyUDPOK, result: okExit,
		ipv6: v6Result(proxyIPv6Yes, "yes", t0, 24*time.Hour),
	}
	store.entries[idle.Key()] = &proxyUDPCheckEntry{
		at: t0, completedAt: t0, realStatus: proxyUDPOK, result: okExit,
		ipv6: v6Result(proxyIPv6Yes, "yes", t0, 24*time.Hour),
	}
	store.mu.Unlock()

	transport := newProxiedPlatformTransport(&fakePlatformTransport{}, &connect.ClientAuth{ByJwt: "jwt"})
	t.Cleanup(func() { transport.stopExtra() })
	controller := &proxiedIPv6Controller{
		store:     store,
		key:       key,
		transport: transport,
		build:     func(*connect.ClientAuth) providerPlatformTransport { return &fakePlatformTransport{} },
	}
	// two measurements start the extra IPv6 transport
	controller.step(t0)
	store.mu.Lock()
	store.entries[key] = &proxyUDPCheckEntry{
		at: checked, completedAt: checked, realStatus: proxyUDPOK, result: okExit,
		ipv6: v6Result(proxyIPv6Yes, "yes", checked, 24*time.Hour),
	}
	store.mu.Unlock()
	controller.step(checked)
	if !transport.hasExtra() {
		t.Fatal("the extra IPv6 transport did not start")
	}

	// a relayed flow on each identity leaves fresh Unknown admission evidence
	// beside the real-check pair
	store.noteFlowReply(key, expires)
	store.noteFlowReply(idle.Key(), expires)
	store.flushPersist()
	if _, _, state := store.measurement(key, expires); state != measurementExpired {
		t.Fatalf("the real expiry was not visible before the eviction: %v", state)
	}

	// newer completed entries push the two flow rows past the capacity bound
	store.mu.Lock()
	for i := range 4 * udpStateMaxEntries {
		at := expires.Add(time.Second)
		store.entries[strconv.Itoa(i)] = &proxyUDPCheckEntry{at: at, completedAt: at, realStatus: proxyUDPOK, result: proxyUDPResult{Status: proxyUDPOK}}
	}
	store.prune(expires.Add(time.Second))
	runningKept := store.entries[key] != nil
	idleEvicted := store.entries[idle.Key()] == nil
	store.mu.Unlock()
	if !runningKept {
		t.Fatal("capacity pruning evicted the entry of an identity that runs an extra IPv6 transport")
	}
	if !idleEvicted {
		t.Fatal("capacity pruning kept the entry of an identity without a transport, so the cap no longer holds")
	}

	controller.step(expires)
	if transport.hasExtra() {
		t.Fatal("the extra IPv6 transport stayed up past the real UDP expiry after a capacity eviction")
	}
}
