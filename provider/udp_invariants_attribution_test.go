package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestUDPInvariantRefusalAttributionAndThrottle(t *testing.T) {
	a := &connect.ProxySettings{Address: "127.0.0.1:24001", Index: 7, Auth: &proxy.Auth{User: "a", Password: "first"}}
	b := &connect.ProxySettings{Address: a.Address, Index: 8, Auth: &proxy.Auth{User: "b", Password: "second"}}
	stats := &proxiedUdpRefusalStats{keyRefusals: map[proxiedUdpRefusalKey]*proxiedUdpRefusal{}}
	out := captureTlog(t, func() {
		stats.note(a, proxiedUdpRefusalNoRelay)
		stats.note(a, proxiedUdpRefusalNoRelay)
		stats.note(a, proxiedUdpRefusalExitMismatch)
		stats.note(b, proxiedUdpRefusalNoRelay)
	})
	if strings.Count(out, "client udp refused") != 3 {
		t.Fatalf("expected three independently throttled lines, got %q", out)
	}
	if stats.count(a, proxiedUdpRefusalNoRelay) != 2 || stats.count(a, proxiedUdpRefusalExitMismatch) != 1 || stats.count(b, proxiedUdpRefusalNoRelay) != 1 {
		t.Fatal("refusal totals did not remain separate by identity and reason")
	}
	stats.keyRefusals[proxiedUdpRefusalKey{proxyKey: a.Key(), reason: proxiedUdpRefusalNoRelay}].lastLogTime = time.Now().Add(-proxiedUdpRefusalLogInterval)
	out = captureTlog(t, func() { stats.note(a, proxiedUdpRefusalNoRelay) })
	if !strings.Contains(out, "2 since the last line") || stats.count(a, proxiedUdpRefusalNoRelay) != 3 {
		t.Fatalf("throttled count not reported correctly: %q", out)
	}
}

func TestUDPInvariantReorderAndRemovalAttribution(t *testing.T) {
	now := time.Now()
	a := &connect.ProxySettings{Address: "127.0.0.1:24001", Index: 1}
	b := &connect.ProxySettings{Address: "127.0.0.1:24002", Index: 2}
	store := newProxyUDPCheckStore(nil)
	store.entries[a.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPNoRelay}, at: now}
	store.entries[b.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPOK}, at: now}
	a.Index, b.Index = b.Index, a.Index
	if store.Check(context.Background(), a, now).Status != proxyUDPNoRelay || store.Check(context.Background(), b, now).Status != proxyUDPOK {
		t.Fatal("reordering changed cached identity attribution")
	}
	coverage := store.coverage([]string{b.Key()}, now)
	if coverage.OK != 1 || coverage.NoRelay != 0 {
		t.Fatalf("removed identity affected coverage: %+v", coverage)
	}
}

func TestUDPInvariantStableIdentity(t *testing.T) {
	a := &connect.ProxySettings{Address: "127.0.0.1:24001", Index: 7, Auth: &proxy.Auth{User: "a", Password: "first"}}
	rotation := &connect.ProxySettings{Address: a.Address, Index: 7, Auth: &proxy.Auth{User: "a", Password: "second"}}
	other := &connect.ProxySettings{Address: a.Address, Index: 7, Auth: &proxy.Auth{User: "b", Password: "first"}}
	if udpProxyIdentity(a) != udpProxyIdentity(rotation) || udpProxyIdentity(a) == udpProxyIdentity(other) {
		t.Fatal("identity must survive password rotation and distinguish accounts")
	}
}
