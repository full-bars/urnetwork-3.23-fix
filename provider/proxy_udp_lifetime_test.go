package main

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProxyUDPLifetimesAndLazyRechecks(t *testing.T) {
	tests := []struct {
		name           string
		status         proxyUDPStatus
		lifetime, lazy time.Duration
	}{
		{"ok", proxyUDPOK, 24 * time.Hour, 30 * time.Minute},
		{"no-relay", proxyUDPNoRelay, 72 * time.Hour, 30 * time.Minute},
		{"mismatch", proxyUDPExitMismatch, 24 * time.Hour, 30 * time.Minute},
		{"unknown", proxyUDPUnknown, time.Hour, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := proxyUDPTTL(tt.status); got != tt.lifetime {
				t.Fatalf("lifetime %v, want %v", got, tt.lifetime)
			}
			if got := proxyUDPLazyRecheckTTL(tt.status); got != tt.lazy {
				t.Fatalf("lazy lifetime %v, want %v", got, tt.lazy)
			}
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			ps := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
			runs := 0
			store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
				runs++
				return proxyUDPResult{Status: tt.status}
			})
			store.entries[ps.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: tt.status}, at: now}
			if got := store.coverage([]string{ps.Key()}, now.Add(tt.lifetime-time.Nanosecond)); got.Unchecked != 0 {
				t.Fatal("coverage expired before its lifetime")
			}
			if got := store.coverage([]string{ps.Key()}, now.Add(tt.lifetime)); got.Unchecked != 1 {
				t.Fatal("coverage did not expire at its lifetime")
			}
			store.Check(context.Background(), ps, now.Add(tt.lazy-time.Nanosecond))
			if runs != 0 {
				t.Fatal("lazy check ran before its deadline")
			}
			store.Check(context.Background(), ps, now.Add(tt.lazy))
			if runs != 1 {
				t.Fatalf("lazy checks = %d at deadline, want 1", runs)
			}
		})
	}
}
