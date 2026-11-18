package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestUDPInvariantGateTransitions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initial, next proxyUDPStatus
		want          error
	}{
		{"ok", proxyUDPOK, proxyUDPOK, nil},
		{"unknown", proxyUDPUnknown, proxyUDPUnknown, nil},
		{"no-relay", proxyUDPOK, proxyUDPNoRelay, errProxiedUdpNoRelay},
		{"mismatch", proxyUDPOK, proxyUDPExitMismatch, errProxiedUdpExitMismatch},
		{"recovered-relay", proxyUDPNoRelay, proxyUDPOK, nil},
		{"converged-exit", proxyUDPExitMismatch, proxyUDPOK, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startFakeSocks5(t, "", "", false)
			settings := proxySettingsFor(server)
			now := time.Now()
			var calls atomic.Int32
			store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
				calls.Add(1)
				return proxyUDPResult{Status: tc.next}
			})
			store.entries[settings.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: tc.initial}, at: now.Add(-2 * proxyUDPLazyTTL)}
			conn, err := dialProxiedUdp(context.Background(), settings, store, "udp", "192.0.2.1:53")
			if conn != nil {
				conn.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("after refresh: err=%v, want %v", err, tc.want)
			}
			if calls.Load() != 1 {
				t.Fatalf("probe count=%d, want 1", calls.Load())
			}
		})
	}
}

func TestUDPInvariantWrongIdentityCannotRefuse(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	settings := proxySettingsFor(server)
	other := &connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}
	store := stubUdpCheckStore(proxyUDPOK)
	store.entries[other.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPNoRelay}, at: time.Now()}
	conn, err := dialProxiedUdp(context.Background(), settings, store, "udp", "192.0.2.1:53")
	if err != nil {
		t.Fatalf("another identity refused this flow: %v", err)
	}
	conn.Close()
}

func TestUDPInvariantNoFallbackDial(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           proxyUDPStatus
		refuse           bool
		network, address string
		cancel           bool
	}{
		{"no-relay", proxyUDPNoRelay, false, "udp", "192.0.2.1:53", false},
		{"mismatch", proxyUDPExitMismatch, false, "udp", "192.0.2.1:53", false},
		{"association-refused", proxyUDPOK, true, "udp", "192.0.2.1:53", false},
		{"ipv6", proxyUDPOK, false, "udp", "[2001:db8::1]:53", false},
		{"cancelled", proxyUDPOK, false, "udp", "192.0.2.1:53", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := startFakeSocks5(t, "", "", tc.refuse)
			settings := proxySettingsFor(server)
			var direct atomic.Int32
			cs := connect.DefaultConnectSettings()
			cs.DialContextSettings = &connect.DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) {
				direct.Add(1)
				return nil, errors.New("unexpected direct dial")
			}}
			installProxiedUdpEgress(cs, settings, stubUdpCheckStore(tc.status))
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				cancel()
			}
			defer cancel()
			conn, err := cs.DialContext(ctx, tc.network, tc.address)
			if conn != nil {
				conn.Close()
				t.Fatal("unexpected connection")
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			if n := direct.Load(); n != 0 {
				t.Fatalf("direct dial count=%d", n)
			}
		})
	}
}

func TestUDPInvariantSweepRefreshesAllBeforeExpiry(t *testing.T) {
	for _, status := range []proxyUDPStatus{proxyUDPOK, proxyUDPUnknown, proxyUDPNoRelay, proxyUDPExitMismatch} {
		t.Run(status.String(), func(t *testing.T) {
			start := time.Unix(100000, 0)
			const count = 100
			candidates := make([]udpProbeCandidate, count)
			for i := range candidates {
				candidates[i] = udpProbeCandidate{key: fmt.Sprint(i), online: true, status: status, lastChecked: start}
			}
			expiry := start.Add(proxyUDPTTL(status))
			for now := start.Add(5 * time.Minute); now.Before(expiry); now = now.Add(5 * time.Minute) {
				batch := selectUDPProbeBatch(now, candidates, 24*time.Hour, 5*time.Minute, nil)
				for _, selected := range batch {
					for i := range candidates {
						if candidates[i].key == selected.key {
							candidates[i].lastChecked = now
							break
						}
					}
				}
			}
			for _, c := range candidates {
				if c.lastChecked.Equal(start) {
					t.Fatalf("%s not refreshed before expiry", c.key)
				}
			}
		})
	}
}
