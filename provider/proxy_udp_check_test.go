package main

import (
	"context"
	"encoding/binary"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

// startFakeStun answers a STUN binding request with the address it saw the
// request from, which through a relay is the relay's own address.
func startFakeStun(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 20 || binary.BigEndian.Uint16(buf[0:2]) != 1 {
				continue
			}
			resp := make([]byte, 0, 32)
			resp = binary.BigEndian.AppendUint16(resp, 0x0101)
			resp = binary.BigEndian.AppendUint16(resp, 12)
			resp = append(resp, buf[4:20]...)
			resp = binary.BigEndian.AppendUint16(resp, 0x0020)
			resp = binary.BigEndian.AppendUint16(resp, 8)
			resp = append(resp, 0, 1)
			resp = binary.BigEndian.AppendUint16(resp, uint16(from.Port)^0x2112)
			ip4 := from.IP.To4()
			cookie := []byte{0x21, 0x12, 0xA4, 0x42}
			for i := 0; i < 4; i++ {
				resp = append(resp, ip4[i]^cookie[i])
			}
			conn.WriteToUDP(resp, from)
		}
	}()
	return conn.LocalAddr().String()
}

func withUdpCheckTargets(t *testing.T, stun string) {
	t.Helper()
	previousStun, previousTcp := udpCheckStunServers, udpCheckTcpExitServers
	udpCheckStunServers = []string{stun}
	udpCheckTcpExitServers = []udpCheckTcpExitServer{{address: "exit.invalid:80", host: "exit.invalid"}}
	t.Cleanup(func() { udpCheckStunServers, udpCheckTcpExitServers = previousStun, previousTcp })
}

func proxySettingsFor(server *fakeSocks5) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: server.address()}
}

func TestStunMappedAddressParsing(t *testing.T) {
	request, transaction := newStunBindingRequest()
	if len(request) != 20 || binary.BigEndian.Uint16(request[0:2]) != 1 {
		t.Fatalf("not a binding request: % x", request)
	}
	// a response for that transaction carrying XOR-MAPPED-ADDRESS 203.0.113.9:4242
	resp := binary.BigEndian.AppendUint16(nil, 0x0101)
	resp = binary.BigEndian.AppendUint16(resp, 12)
	resp = append(resp, request[4:20]...)
	resp = binary.BigEndian.AppendUint16(resp, 0x0020)
	resp = binary.BigEndian.AppendUint16(resp, 8)
	resp = append(resp, 0, 1)
	resp = binary.BigEndian.AppendUint16(resp, 4242^0x2112)
	cookie := []byte{0x21, 0x12, 0xA4, 0x42}
	for i, b := range []byte{203, 0, 113, 9} {
		resp = append(resp, b^cookie[i])
	}
	ip, ok := parseStunMappedAddress(resp, transaction)
	if !ok || ip.String() != "203.0.113.9" {
		t.Fatalf("parsed %v ok=%v, want 203.0.113.9", ip, ok)
	}
	var other [12]byte
	if _, ok := parseStunMappedAddress(resp, other); ok {
		t.Fatalf("a response for another transaction was accepted")
	}
	if _, ok := parseStunMappedAddress(resp[:15], transaction); ok {
		t.Fatalf("a truncated response was accepted")
	}
}

// The relay works and exits from the same address as the TCP path: H3 is fine.
func TestUdpCheckAgreesWhenBothPathsExitTheSameAddress(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "127.0.0.1"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := checkProxyUDP(ctx, proxySettingsFor(server))
	if result.Status != proxyUDPOK {
		t.Fatalf("status = %s (tcp=%q udp=%q err=%v), want ok", result.Status, result.TCPExit, result.UDPExit, result.Err)
	}
	if result.TCPExit != "127.0.0.1" || result.UDPExit != "127.0.0.1" {
		t.Fatalf("exits = tcp %q udp %q", result.TCPExit, result.UDPExit)
	}
}

// The gap the TCP-only grading leaves: a proxy whose UDP relay exits from
// another address than its TCP side would put the identity somewhere the
// grade never looked. That is a mismatch, and H3 must not be used for it.
func TestUdpCheckFlagsAnExitMismatch(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "203.0.113.77"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := checkProxyUDP(ctx, proxySettingsFor(server))
	if result.Status != proxyUDPExitMismatch {
		t.Fatalf("status = %s, want exit-mismatch (tcp=%q udp=%q)", result.Status, result.TCPExit, result.UDPExit)
	}
}

func TestUdpCheckReportsAProxyThatWillNotRelay(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", true)
	server.tcpExitBody = "127.0.0.1"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if result := checkProxyUDP(ctx, proxySettingsFor(server)); result.Status != proxyUDPNoRelay {
		t.Fatalf("status = %s, want no-relay", result.Status)
	}
}

// The relay accepts the association but nothing comes back (a proxy that
// cannot reach the STUN server over UDP, or drops it): also no relay.
func TestUdpCheckReportsARelayThatStaysSilent(t *testing.T) {
	deadStun, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer deadStun.Close()
	withUdpCheckTargets(t, deadStun.LocalAddr().String())
	previous := udpCheckReplyTimeout
	udpCheckReplyTimeout = 200 * time.Millisecond
	defer func() { udpCheckReplyTimeout = previous }()
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "127.0.0.1"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if result := checkProxyUDP(ctx, proxySettingsFor(server)); result.Status != proxyUDPNoRelay {
		t.Fatalf("status = %s, want no-relay for a silent relay", result.Status)
	}
}

// When the TCP exit cannot be learned the two cannot be compared. That is
// unknown, not a failure: the relay itself works and still goes through the
// proxy, so H3 is allowed but the check is repeated sooner.
func TestUdpCheckIsUnknownWhenTheTcpExitCannotBeLearned(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", false) // no tcpExitBody: CONNECT is refused

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := checkProxyUDP(ctx, proxySettingsFor(server))
	if result.Status != proxyUDPUnknown {
		t.Fatalf("status = %s, want unknown", result.Status)
	}
	if result.UDPExit != "127.0.0.1" {
		t.Fatalf("the udp exit that was learned is lost: %q", result.UDPExit)
	}
}

// One check per identity at a time, remembered for a while, and a different
// answer for the same identity replaces the old one only when it expires.
func TestUdpCheckStoreRemembersAndDeduplicates(t *testing.T) {
	var runs atomic.Int32
	store := newProxyUDPCheckStore(func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		runs.Add(1)
		time.Sleep(50 * time.Millisecond)
		return proxyUDPResult{Status: proxyUDPOK}
	})
	settings := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}

	done := make(chan proxyUDPResult, 3)
	for i := 0; i < 3; i++ {
		go func() { done <- store.Check(context.Background(), settings, time.Now()) }()
	}
	for i := 0; i < 3; i++ {
		if result := <-done; result.Status != proxyUDPOK {
			t.Fatalf("status = %s", result.Status)
		}
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("%d concurrent checks for one identity ran the probe %d times, want 1", 3, got)
	}
	store.Check(context.Background(), settings, time.Now())
	if got := runs.Load(); got != 1 {
		t.Fatalf("a remembered result was probed again (%d runs)", got)
	}
	store.Check(context.Background(), settings, time.Now().Add(proxyUDPLazyTTL+time.Second))
	if got := runs.Load(); got != 2 {
		t.Fatalf("an expired result was not probed again (%d runs)", got)
	}
}

func TestUdpCheckStoreKeysByIdentityNotAddress(t *testing.T) {
	var runs atomic.Int32
	store := newProxyUDPCheckStore(func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		runs.Add(1)
		return proxyUDPResult{Status: proxyUDPOK}
	})
	a := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	b := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
	a.Auth = authFor("alice")
	b.Auth = authFor("bob")
	store.Check(context.Background(), a, time.Now())
	store.Check(context.Background(), b, time.Now())
	if got := runs.Load(); got != 2 {
		t.Fatalf("two accounts on one gateway address shared a check (%d runs): the store is keyed by address", got)
	}
}

// The factory acts on the check: H3 is opened only when the UDP path is
// verified or merely unverifiable, never for a mismatch or a dead relay.
func TestH3FactoryRefusesAnIdentityWhoseUdpExitDiffers(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "203.0.113.77"
	previous := globalProxyUDPCheck
	globalProxyUDPCheck = newProxyUDPCheckStore(checkProxyUDP)
	defer func() { globalProxyUDPCheck = previous }()

	factory := newH3PacketConnFactory(proxySettingsFor(server))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if pc, err := factory(ctx); err == nil {
		pc.Close()
		t.Fatalf("H3 was opened through a proxy whose UDP exit differs from its TCP exit")
	}
}

func TestH3FactoryOpensWhenTheUdpPathIsVerified(t *testing.T) {
	stun := startFakeStun(t)
	withUdpCheckTargets(t, stun)
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "127.0.0.1"
	previous := globalProxyUDPCheck
	globalProxyUDPCheck = newProxyUDPCheckStore(checkProxyUDP)
	defer func() { globalProxyUDPCheck = previous }()

	factory := newH3PacketConnFactory(proxySettingsFor(server))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pc, err := factory(ctx)
	if err != nil {
		t.Fatalf("H3 was refused for a verified proxy: %v", err)
	}
	pc.Close()
}

func authFor(user string) *proxy.Auth { return &proxy.Auth{User: user, Password: "pw"} }
