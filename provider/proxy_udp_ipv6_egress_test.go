package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// An ipv6 destination goes through a proxied identity only when the measured
// result of that proxy says the relay carries ipv6 datagrams
// (proxyUDPCheckStore.ipv6UDPAllowed, proxy_udp_egress.go).

func measuredIPv6(status proxyIPv6Status, udp string, at time.Time, interval time.Duration) proxyIPv6Result {
	return proxyIPv6Result{Status: status, Source: "measured", UDP: udp, TCPExit: "2001:db8::1", UDPExit: "2001:db8::1", At: at, Interval: interval}
}

// storeWithEntry puts a completed check into the store the way the sweep does.
func storeWithEntry(store *proxyUDPCheckStore, settings *connect.ProxySettings, status proxyUDPStatus, at time.Time, ipv6 proxyIPv6Result) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.entries[settings.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: status}, at: at, ipv6: ipv6}
}

func TestIPv6UDPAllowedDecision(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	hour := time.Hour
	for _, test := range []struct {
		name   string
		status proxyUDPStatus
		ipv6   proxyIPv6Result
		want   bool
	}{
		{"measured yes for tcp and udp", proxyUDPOK, measuredIPv6(proxyIPv6Yes, "yes", now.Add(-hour), 12*hour), true},
		{"never measured", proxyUDPOK, proxyIPv6Result{}, false},
		{"tcp ipv6 no", proxyUDPOK, measuredIPv6(proxyIPv6No, "unknown", now.Add(-hour), 24*hour), false},
		{"tcp ipv6 unknown", proxyUDPOK, measuredIPv6(proxyIPv6Unknown, "unknown", now.Add(-hour), hour), false},
		{"tcp yes but udp unknown", proxyUDPOK, measuredIPv6(proxyIPv6Yes, "unknown", now.Add(-hour), 12*hour), false},
		{"tcp yes but udp exit mismatch", proxyUDPOK, measuredIPv6(proxyIPv6Yes, "mismatch", now.Add(-hour), 12*hour), false},
		{"ipv4 relay has no udp", proxyUDPNoRelay, measuredIPv6(proxyIPv6Yes, "yes", now.Add(-hour), 12*hour), false},
		{"ipv4 relay exit mismatch", proxyUDPExitMismatch, measuredIPv6(proxyIPv6Yes, "yes", now.Add(-hour), 12*hour), false},
		{"ipv4 check unknown", proxyUDPUnknown, measuredIPv6(proxyIPv6Yes, "yes", now.Add(-hour), 12*hour), false},
		{"result older than twice its interval", proxyUDPOK, measuredIPv6(proxyIPv6Yes, "yes", now.Add(-25*hour), 12*hour), false},
		{"result just inside twice its interval", proxyUDPOK, measuredIPv6(proxyIPv6Yes, "yes", now.Add(-23*hour), 12*hour), true},
		{"result from the future", proxyUDPOK, measuredIPv6(proxyIPv6Yes, "yes", now.Add(hour), 12*hour), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := stubUdpCheckStore(proxyUDPOK)
			storeWithEntry(store, settings, test.status, now.Add(-time.Minute), test.ipv6)
			if got := store.ipv6UDPAllowed(settings.Key(), now); got != test.want {
				t.Fatalf("allowed = %v, want %v", got, test.want)
			}
		})
	}
	t.Run("unknown identity", func(t *testing.T) {
		if stubUdpCheckStore(proxyUDPOK).ipv6UDPAllowed("missing", now) {
			t.Fatal("an identity with no entry was allowed")
		}
	})
	t.Run("udp result expired under a fresh ipv6 record", func(t *testing.T) {
		// the ipv6 record is current, so only the udp result's age can refuse:
		// the same rule measurement applies to every other reader
		store := stubUdpCheckStore(proxyUDPOK)
		storeWithEntry(store, settings, proxyUDPOK, now.Add(-(proxyUDPOKTTL + time.Minute)), measuredIPv6(proxyIPv6Yes, "yes", now.Add(-hour), 12*hour))
		if store.ipv6UDPAllowed(settings.Key(), now) {
			t.Fatal("an expired udp result admitted an ipv6 dial")
		}
	})
}

func TestIPv6UDPSettingOffRefusesEveryProxy(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	now := time.Now()
	store := stubUdpCheckStore(proxyUDPOK)
	storeWithEntry(store, settings, proxyUDPOK, now.Add(-time.Minute), measuredIPv6(proxyIPv6Yes, "yes", now.Add(-time.Hour), 12*time.Hour))
	if !store.ipv6UDPAllowed(settings.Key(), now) {
		t.Fatal("a measured proxy was refused with the default setting")
	}
	state := newControlState()
	if !proxyUDPIPv6EnabledIn(state) || !proxyUDPIPv6EnabledIn(nil) {
		t.Fatal("the setting is not on by default")
	}
	if err := state.set("proxy_udp_ipv6", "off"); err != nil {
		t.Fatal(err)
	}
	if proxyUDPIPv6EnabledIn(state) {
		t.Fatal("the setting did not turn off")
	}
	previous := globalControlState
	globalControlState = state
	defer func() { globalControlState = previous }()
	if store.ipv6UDPAllowed(settings.Key(), now) {
		t.Fatal("a measured proxy was allowed with the setting off")
	}
	if err := validateControlValue("proxy_udp_ipv6", "maybe"); err == nil {
		t.Fatal("an invalid value was accepted")
	}
}

func TestProxiedIdentityUdpRefusesIpv6WhenTheProxyIsNotMeasured(t *testing.T) {
	for _, ipv6 := range []proxyIPv6Result{
		{},
		measuredIPv6(proxyIPv6No, "unknown", time.Now().Add(-time.Hour), 24*time.Hour),
		measuredIPv6(proxyIPv6Yes, "unknown", time.Now().Add(-time.Hour), 12*time.Hour),
	} {
		server := startFakeSocks5(t, "", "", false)
		proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
		store := stubUdpCheckStore(proxyUDPOK)
		storeWithEntry(store, proxySettings, proxyUDPOK, time.Now(), ipv6)
		natSettings := proxiedIdentityNatSettings(proxySettings, store)
		conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "[2001:db8::1]:443")
		if conn != nil {
			conn.Close()
		}
		if !errors.Is(err, errProxiedUdpIpv6Refused) {
			t.Fatalf("err = %v, want the ipv6 refusal for %+v", err, ipv6)
		}
		if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalIpv6) != 1 {
			t.Fatal("the ipv6 refusal was not counted")
		}
		if got := server.accepts.Load(); got != 0 {
			t.Fatalf("a refused ipv6 flow connected to the proxy %d times", got)
		}
	}
}

// A measured proxy relays an ipv6 datagram to its destination and the reply
// comes back through the same association, never through a socket of the host.
func TestProxiedIdentityUdpRelaysIpv6ThroughAMeasuredProxy(t *testing.T) {
	echo, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no ipv6 loopback: %v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			echo.WriteToUDP(append([]byte("echo:"), buf[:n]...), from)
		}
	}()

	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	store := stubUdpCheckStore(proxyUDPOK)
	storeWithEntry(store, proxySettings, proxyUDPOK, time.Now(), measuredIPv6(proxyIPv6Yes, "yes", time.Now().Add(-time.Hour), 12*time.Hour))
	natSettings := proxiedIdentityNatSettings(proxySettings, store)

	destination := echo.LocalAddr().String()
	conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", destination)
	if err != nil {
		t.Fatalf("a measured proxy refused an ipv6 flow: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no reply through the relay: %v", err)
	}
	if got := string(buf[:n]); got != "echo:ping" {
		t.Fatalf("reply = %q", got)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalIpv6) != 0 {
		t.Fatal("a measured proxy counted an ipv6 refusal")
	}
	if got := server.accepts.Load(); got != 1 {
		t.Fatalf("control connections = %d, want 1 association", got)
	}
}

// The decision is made again after the wait for the check result: a concurrent
// change of the eligibility refuses the flow before the association opens.
func TestProxiedIdentityUdpRechecksIpv6AfterTheCheckWait(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	state := newControlState()
	previous := globalControlState
	globalControlState = state
	defer func() { globalControlState = previous }()

	store := newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		// while the flow waits for this check, an operator turns the feature off
		state.set("proxy_udp_ipv6", "off")
		return proxyUDPResult{Status: proxyUDPOK}
	})
	staleUDP := time.Now().Add(-time.Hour)
	storeWithEntry(store, proxySettings, proxyUDPOK, staleUDP, measuredIPv6(proxyIPv6Yes, "yes", time.Now().Add(-time.Hour), 12*time.Hour))
	natSettings := proxiedIdentityNatSettings(proxySettings, store)
	conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "[2001:db8::1]:443")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errProxiedUdpIpv6Refused) {
		t.Fatalf("err = %v, want the ipv6 refusal after the recheck", err)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalIpv6) != 1 {
		t.Fatal("the refusal was not counted once")
	}
	if got := server.accepts.Load(); got != 0 {
		t.Fatalf("the flow reached the proxy %d times", got)
	}
}
