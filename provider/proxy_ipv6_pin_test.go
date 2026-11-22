package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The pin that keeps a running transport's entry has to be in place before the
// builder runs. The builder can take long enough for capacity pruning to evict
// the entry, and the entry holds the real-check pair that stops the transport on
// the UDP expiry. Without the pin the next poll reads Missing and stops a
// transport that has just started.
func TestCapacityEvictionDuringExtraStartKeepsTheEntry(t *testing.T) {
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	key := settings.Key()
	ok := proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1"}
	store := newProxyUDPCheckStore(nil)
	set := func(at time.Time) {
		store.mu.Lock()
		store.entries[key] = &proxyUDPCheckEntry{
			at: at, completedAt: at, realStatus: proxyUDPOK, result: ok,
			ipv6: v6Result(proxyIPv6Yes, "yes", at, 24*time.Hour),
		}
		store.mu.Unlock()
	}
	set(t0)

	transport := newProxiedPlatformTransport(&fakePlatformTransport{}, nil)
	t.Cleanup(func() { transport.stopExtra() })
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	pinnedDuringBuild := false
	controller := &proxiedIPv6Controller{
		store: store, key: key, transport: transport,
		build: func(*connect.ClientAuth) providerPlatformTransport {
			store.mu.Lock()
			pinnedDuringBuild = store.pinnedLocked(key)
			store.mu.Unlock()
			close(entered)
			<-release
			return &fakePlatformTransport{}
		},
	}
	// the start streak is two measurements
	controller.step(t0)
	set(t1)
	go func() {
		defer close(finished)
		controller.step(t1)
	}()
	<-entered

	// capacity pruning runs while the builder is still blocked
	store.mu.Lock()
	for i := range 4 * udpStateMaxEntries {
		at := t1.Add(time.Second)
		store.entries[strconv.Itoa(i)] = &proxyUDPCheckEntry{
			at: at, completedAt: at, realStatus: proxyUDPOK, result: proxyUDPResult{Status: proxyUDPOK},
		}
	}
	store.prune(t1.Add(time.Second))
	kept := store.entries[key] != nil
	store.mu.Unlock()
	close(release)
	<-finished

	if !pinnedDuringBuild {
		t.Fatal("the entry was not pinned while the builder ran")
	}
	if !kept {
		t.Fatal("capacity pruning evicted the entry of a transport that was starting")
	}
	if !transport.hasExtra() {
		t.Fatal("the extra IPv6 transport did not start")
	}
	if _, _, state := store.measurement(key, t1.Add(2*time.Second)); state != measurementReady {
		t.Fatalf("the next poll does not read a measurement: %v", state)
	}
	controller.step(t1.Add(2 * time.Second))
	if !transport.hasExtra() {
		t.Fatal("the transport was stopped after its start although its entry is in place")
	}
}
