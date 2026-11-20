package connect

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// A pinned transport of a proxied identity reaches the platform through the
// proxy over the pinned family (NewProxiedFamilyClientStrategy).

func TestNewProxiedFamilyClientStrategyKeepsTheProxy(t *testing.T) {
	proxySettings := &ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	settings := DefaultClientStrategySettings()
	settings.ConnectSettings.ProxySettings = proxySettings
	settings.MaxExtenderCount = 3
	settings.ExpandExtenderProfileCount = 2

	strategy := NewProxiedFamilyClientStrategy(context.Background(), settings, 6)
	if got := strategy.ConnectSettings().ProxySettings; got == nil || got.Address != proxySettings.Address {
		t.Fatalf("the strategy lost the proxy: %+v", got)
	}
	if strategy.ipFamily != 6 {
		t.Fatalf("strategy family = %d, want 6", strategy.ipFamily)
	}
	// the caller's settings are not mutated
	if settings.MaxExtenderCount != 3 || settings.ExpandExtenderProfileCount != 2 || settings.ConnectSettings.ProxySettings != proxySettings {
		t.Fatal("the caller's strategy settings were changed")
	}
}

func TestPinnedIpv6DialThroughProxyCarriesTheName(t *testing.T) {
	resetDnsCache()
	// the default mode: without the pin the name would be resolved here with an
	// A-only lookup, which finds nothing for a name that has only an AAAA record
	previous := SetProxyTargetFamily(ProxyTargetFamilyV4)
	defer SetProxyTargetFamily(previous)

	dest, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %s", err)
	}
	defer dest.Close()
	_, destPort, err := net.SplitHostPort(dest.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	const name = "connect-v6.example"
	go serveOneReady(dest)

	proxyAddr, seen, stop := startSocks5Stub(t, func(req socks5Connect) (string, error) {
		if req.atyp != 0x03 || req.host != name {
			return "", fmt.Errorf("CONNECT carried atyp 0x%02x host %q, want the name %q", req.atyp, req.host, name)
		}
		return net.JoinHostPort("::1", req.port), nil
	})
	defer stop()

	settings := &ProxySettings{Network: "tcp", Address: proxyAddr}
	dial := settings.NewDialContext(context.Background(), &net.Dialer{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(withPinnedIpFamily(context.Background(), 6), 10*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp6", net.JoinHostPort(name, destPort))
	if err != nil {
		t.Fatalf("a dial pinned to IPv6 failed through the proxy: %v", err)
	}
	defer conn.Close()
	if got := readAll(t, conn); got != "ready" {
		t.Fatalf("destination said %q", got)
	}
	req := awaitSocks5Request(t, seen)
	if req.atyp != 0x03 || req.host != name || req.port != destPort {
		t.Fatalf("CONNECT = atyp 0x%02x host %q port %q", req.atyp, req.host, req.port)
	}
	if _, ok := dnsCacheLookup(name); ok {
		t.Fatal("the pinned dial filled the proxy-target DNS cache")
	}
}

func TestProxyDialCarriesNameDecision(t *testing.T) {
	plain := context.Background()
	if proxyDialCarriesName(plain) {
		t.Fatal("an unpinned dial in the default mode carries the name")
	}
	if proxyDialCarriesName(withPinnedIpFamily(plain, 4)) {
		t.Fatal("a dial pinned to IPv4 carries the name")
	}
	if !proxyDialCarriesName(withPinnedIpFamily(plain, 6)) {
		t.Fatal("a dial pinned to IPv6 does not carry the name")
	}
	previous := SetProxyTargetFamily(ProxyTargetFamilyProxy)
	defer SetProxyTargetFamily(previous)
	if !proxyDialCarriesName(plain) {
		t.Fatal("proxy mode does not carry the name")
	}
}

// A pinned transport that dials through a proxy is not held by the local
// family probe: the proxy supplies the exit. A direct pin keeps the hold.
func TestProxiedPinIsNotHeldByTheLocalFamilyProbe(t *testing.T) {
	defer swapControlFamilyProbe(func(int) bool { return false })()

	settings := DefaultClientStrategySettings()
	settings.ConnectSettings.ProxySettings = &ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}

	proxied := newPinnedRouteTestTransport()
	proxied.clientStrategy = NewProxiedFamilyClientStrategy(context.Background(), settings, 6)
	proxied.refreshFamilyHold()
	if got := PlatformTransportState(proxied.familyHold.Load()); got != PlatformTransportStateConnecting {
		t.Fatalf("a proxied pin is held with state %s, want connecting", got)
	}

	direct := newPinnedRouteTestTransport()
	direct.clientStrategy = NewDirectClientStrategy(context.Background(), settings, 6)
	direct.refreshFamilyHold()
	if got := PlatformTransportState(direct.familyHold.Load()); got != PlatformTransportStateSleeping {
		t.Fatalf("a direct pin without local IPv6 has state %s, want sleeping", got)
	}
}
