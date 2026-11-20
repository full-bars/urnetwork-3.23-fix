package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestUDPPersistFullProxyTable(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := make([]udpPersistEntry, udpStateMaxEntries+1)
	for i := range entries {
		key := udpPersistKey(fmt.Sprintf("identity-%d", i))
		entries[i] = udpPersistEntry{Key: key, Status: proxyUDPOK, At: now, TCPExit: "192.0.2.1", UDPExit: "192.0.2.1", IPv6: proxyIPv6Result{Status: proxyIPv6Yes, Source: "measured", UDP: "yes", TCPExit: "2001:db8::1", UDPExit: "2001:db8::1", At: now, Interval: 24 * time.Hour}}
	}
	path := filepath.Join(t.TempDir(), "udp.json")
	dropped, err := writeUDPPersistFile(path, entries)
	if err != nil || len(dropped) != 1 {
		t.Fatalf("write: dropped %d, error %v", len(dropped), err)
	}
	store := newProxyUDPCheckStore(nil)
	store.enablePersistence(path, now)
	if len(store.persisted) != udpStateMaxEntries {
		t.Fatalf("loaded %d entries, want %d", len(store.persisted), udpStateMaxEntries)
	}
	for _, saved := range store.persisted {
		if saved.IPv6.Status != proxyIPv6Yes || saved.IPv6.Interval != 24*time.Hour {
			t.Fatal("IPv6 state did not round trip")
		}
	}
}
