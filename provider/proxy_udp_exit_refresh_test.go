package main

import (
	"context"
	"testing"
)

func TestUDPCheckRefreshesDifferentCachedTCPExit(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "127.0.0.1"
	ctx := context.WithValue(context.Background(), cachedProxyTCPExitKey{}, "203.0.113.77")
	result := checkProxyUDP(ctx, proxySettingsFor(server))
	if result.Status != proxyUDPOK || result.TCPExit != "127.0.0.1" {
		t.Fatalf("status = %s, TCP exit = %q, want ok and current exit", result.Status, result.TCPExit)
	}
}
