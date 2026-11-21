package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The hover detail names the cached UDP check state of each proxy, using the
// same rules as the coverage counts: a missing, running or expired result is
// unchecked, and the direct slot has no proxy to check.
func TestProxyCellDetailUDPStatus(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	saved := globalProxyUDPCheck
	defer func() { globalProxyUDPCheck = saved }()
	store := newProxyUDPCheckStore(nil)
	store.entries = map[string]*proxyUDPCheckEntry{
		"ok":       {result: proxyUDPResult{Status: proxyUDPOK}, at: now},
		"no":       {result: proxyUDPResult{Status: proxyUDPNoRelay}, at: now},
		"mismatch": {result: proxyUDPResult{Status: proxyUDPExitMismatch}, at: now},
		"unknown":  {result: proxyUDPResult{Status: proxyUDPUnknown}, at: now},
		"expired":  {result: proxyUDPResult{Status: proxyUDPOK}, at: now.Add(-proxyUDPOKTTL)},
		"running":  {done: make(chan struct{})},
	}
	globalProxyUDPCheck = store
	for _, c := range []struct {
		index int
		key   string
		want  string
	}{
		{1, "ok", "ok"},
		{2, "no", "no_relay"},
		{3, "mismatch", "mismatch"},
		{4, "unknown", "unknown"},
		{5, "expired", "unchecked"},
		{6, "running", "unchecked"},
		{7, "missing", "unchecked"},
		{0, "direct", "direct"},
	} {
		if got := proxyCellUDPStatus(connect.ProxyCellRaw{Index: c.index, Key: c.key}, now); got != c.want {
			t.Errorf("proxy %d (%s): udp %q, want %q", c.index, c.key, got, c.want)
		}
	}
}

func TestProxyCellExitAddresses(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newProxyUDPCheckStore(nil)
	store.entries["valid"] = &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "2001:db8::1"}}
	store.entries["invalid"] = &proxyUDPCheckEntry{at: now, result: proxyUDPResult{Status: proxyUDPOK, TCPExit: "user:secret@example", UDPExit: "192.0.2.2\nsecret"}}
	if tcp, udp := store.exitsFor("valid", now); tcp != "192.0.2.1" || udp != "2001:db8::1" {
		t.Fatalf("unexpected exits: %q %q", tcp, udp)
	}
	if tcp, udp := store.exitsFor("invalid", now); tcp != "" || udp != "" {
		t.Fatal("invalid exits exposed")
	}
	if tcp, udp := store.exitsFor("valid", now.Add(proxyUDPOKTTL)); tcp != "" || udp != "" {
		t.Fatal("expired exits exposed")
	}
}

// The detail row comes from one entry state: the verdict, both exits and the
// ipv6 pair cannot be mixed across two checks, and a recheck keeps showing the
// last completed result it carries.
func TestUDPDetailForIsOneSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	store := stubUdpCheckStore(proxyUDPOK)
	entry := func(done chan struct{}, at time.Time) *proxyUDPCheckEntry {
		return &proxyUDPCheckEntry{
			done:        done,
			result:      proxyUDPResult{Status: proxyUDPOK, TCPExit: "192.0.2.1", UDPExit: "2001:db8::1"},
			at:          at,
			completedAt: at,
			ipv6:        measuredIPv6(proxyIPv6Yes, "yes", at.Add(-time.Hour), 12*time.Hour),
		}
	}
	assertSnapshot := func(t *testing.T, got proxyUDPDetail) {
		t.Helper()
		if got.Status != "ok" || got.TCPExit != "192.0.2.1" || got.UDPExit != "2001:db8::1" {
			t.Fatalf("udp row = %+v, want ok with both exits", got)
		}
		if got.IPv6 != string(proxyIPv6Yes) || got.IPv6Exit != "2001:db8::1" {
			t.Fatalf("ipv6 row = %q %q, want a yes with its exit", got.IPv6, got.IPv6Exit)
		}
	}

	store.mu.Lock()
	store.entries[settings.Key()] = entry(nil, now)
	store.mu.Unlock()
	assertSnapshot(t, store.detailFor(settings.Key(), now))

	// a recheck in flight carries the last completed result
	store.mu.Lock()
	store.entries[settings.Key()] = entry(make(chan struct{}), now)
	store.mu.Unlock()
	assertSnapshot(t, store.detailFor(settings.Key(), now))

	// an expired result is unchecked and reports no exits, while a fresh ipv6
	// measurement keeps its own fields
	store.mu.Lock()
	stale := entry(nil, now.Add(-(proxyUDPOKTTL + time.Minute)))
	stale.ipv6 = measuredIPv6(proxyIPv6Yes, "yes", now.Add(-time.Hour), 12*time.Hour)
	store.entries[settings.Key()] = stale
	store.mu.Unlock()
	expired := store.detailFor(settings.Key(), now)
	if expired.Status != "unchecked" || expired.TCPExit != "" || expired.UDPExit != "" {
		t.Fatalf("expired row = %+v, want unchecked and no exits", expired)
	}
	if expired.IPv6 != string(proxyIPv6Yes) || expired.IPv6Exit != "2001:db8::1" {
		t.Fatalf("expired udp lost the ipv6 row: %+v", expired)
	}
}
