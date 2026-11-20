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
