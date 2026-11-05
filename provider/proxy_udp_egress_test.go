package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Client udp of a proxied identity (proxy_udp_egress.go). Every fixture is
// local: the proxy and the destinations listen on 127.0.0.1, and addresses that
// must never be reached come from the rfc 5737 documentation ranges.

// stubUdpCheckStore is a check store that answers with a fixed status, so the
// gate is tested without the public stun and address services.
func stubUdpCheckStore(status proxyUDPStatus) *proxyUDPCheckStore {
	return newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: status}
	})
}

// proxiedIdentityNatSettings builds a proxied identity's local user nat
// settings the way provideWithProxy does.
func proxiedIdentityNatSettings(proxySettings *connect.ProxySettings, checkStore *proxyUDPCheckStore) *connect.LocalUserNatSettings {
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.ProxySettings = proxySettings
	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	applyIdentityNatConnectSettings(localUserNatSettings, clientStrategySettings.ConnectSettings, proxySettings, checkStore)
	return localUserNatSettings
}

func waitForAssociations(t *testing.T, server *fakeSocks5, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for server.associations.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open associations = %d, want %d", server.associations.Load(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The root cause: the nat dialed a proxied identity's client udp through the
// engine's SOCKS5 dialer, which only does CONNECT, so every dial failed with
// "network not implemented" before reaching the proxy. Through the nat's udp
// settings a dial must now open an association on the proxy and round trip
// through its relay.
func TestProxiedIdentityClientUdpGoesThroughTheProxyRelay(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, seen := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("udp dial through the proxied identity: %v", err)
	}
	defer conn.Close()
	relayPort := server.waitRelayPort(t)

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 1500)
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buffer[:n]) != "ping" {
		t.Fatalf("payload = %q, want ping", buffer[:n])
	}
	if from := <-seen; from.Port != relayPort {
		t.Fatalf("the destination saw the datagram from port %d, not the relay's %d", from.Port, relayPort)
	}
	if got := server.associations.Load(); got != 1 {
		t.Fatalf("open associations = %d, want 1", got)
	}
	if conn.RemoteAddr().String() != echo.LocalAddr().String() {
		t.Fatalf("remote addr = %s, want the destination %s", conn.RemoteAddr(), echo.LocalAddr())
	}
}

// The nat's tcp settings are a separate copy: tcp keeps the CONNECT path, and
// a non-udp dial on the udp settings still goes through the proxy.
func TestProxiedIdentityTcpKeepsTheConnectPath(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	server.tcpExitBody = "198.51.100.7"
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))
	if natSettings.TcpBufferSettings.DialContextSettings != nil {
		t.Fatalf("the tcp settings got the udp dialer")
	}

	for name, dial := range map[string]func(context.Context, string, string) (net.Conn, error){
		"tcp settings": natSettings.TcpBufferSettings.DialContext,
		"udp settings": natSettings.UdpBufferSettings.DialContext,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := dial(ctx, "tcp", "192.0.2.1:80")
		cancel()
		if err != nil {
			t.Fatalf("%s: tcp dial through the proxy: %v", name, err)
		}
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		response, _ := io.ReadAll(conn)
		conn.Close()
		if !strings.Contains(string(response), "198.51.100.7") {
			t.Fatalf("%s: response = %q, the dial did not go through the proxy", name, response)
		}
	}
	if got := server.associations.Load(); got != 0 {
		t.Fatalf("a tcp dial opened %d udp associations", got)
	}
}

// The direct identity keeps the engine's own udp socket.
func TestDirectIdentityUdpSettingsAreUntouched(t *testing.T) {
	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	applyIdentityNatConnectSettings(localUserNatSettings, *connect.DefaultConnectSettings(), nil, stubUdpCheckStore(proxyUDPOK))
	if localUserNatSettings.UdpBufferSettings.DialContextSettings != nil {
		t.Fatalf("the direct identity got a proxy udp dialer")
	}
}

// no-relay and exit-mismatch refuse at once: no connection to the proxy (so no
// control connection to leak), no datagram leaves from the host, and the error
// says which.
func TestProxiedIdentityUdpRefusals(t *testing.T) {
	for _, test := range []struct {
		status proxyUDPStatus
		want   error
		reason proxiedUdpRefusalReason
	}{
		{status: proxyUDPNoRelay, want: errProxiedUdpNoRelay, reason: proxiedUdpRefusalNoRelay},
		{status: proxyUDPExitMismatch, want: errProxiedUdpExitMismatch, reason: proxiedUdpRefusalExitMismatch},
	} {
		t.Run(test.status.String(), func(t *testing.T) {
			server := startFakeSocks5(t, "", "", false)
			echo, seen := startEchoUdp(t)
			proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
			natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(test.status))
			refusedBefore := proxiedUdpRefusals.count(proxySettings, test.reason)

			start := time.Now()
			conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", echo.LocalAddr().String())
			if conn != nil {
				conn.Close()
				t.Fatalf("a %s proxy returned a conn", test.status)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("err = %v, want %v", err, test.want)
			}
			if elapsed := time.Since(start); time.Second < elapsed {
				t.Fatalf("the refusal took %s, want it at once", elapsed)
			}
			if got := server.accepts.Load(); got != 0 {
				t.Fatalf("a refused flow connected to the proxy %d times", got)
			}
			select {
			case from := <-seen:
				t.Fatalf("a refused flow reached the destination from %s", from)
			case <-time.After(200 * time.Millisecond):
			}
			if proxiedUdpRefusals.count(proxySettings, test.reason) != refusedBefore+1 {
				t.Fatalf("the refusal was not counted")
			}
		})
	}
}

// A proxy that passes the check but refuses this association is an error too,
// never a socket on the host, and its control connection is closed.
func TestProxiedIdentityUdpAssociationRefusedIsNotAHostSocket(t *testing.T) {
	server := startFakeSocks5(t, "", "", true)
	echo, seen := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPUnknown))
	refusedBefore := proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalAssociation)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", echo.LocalAddr().String())
	if err == nil {
		conn.Close()
		t.Fatalf("got a conn although the proxy refused UDP ASSOCIATE")
	}
	if !strings.Contains(err.Error(), "relay dial failed") {
		t.Fatalf("err = %v, want a relay dial failure", err)
	}
	select {
	case from := <-seen:
		t.Fatalf("the flow reached the destination from %s", from)
	case <-time.After(200 * time.Millisecond):
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalAssociation) != refusedBefore+1 {
		t.Fatalf("the association failure was not counted")
	}
}

// Closing the flow's conn ends the association and its control connection on
// the proxy.
func TestProxiedIdentityUdpCloseEndsTheAssociation(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server.waitRelayPort(t)
	waitForAssociations(t, server, 1)
	conn.Close()
	waitForAssociations(t, server, 0)
	if _, err := conn.Write([]byte("after close")); err == nil {
		t.Fatalf("a write after close succeeded")
	}
}

// A canceled flow context ends the dial promptly, whether it is waiting for the
// udp check or in the proxy handshake, and leaves no connection behind.
func TestProxiedIdentityUdpCanceledDialJoins(t *testing.T) {
	t.Run("waiting for the check", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		checkStore := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
			close(started)
			select {
			case <-ctx.Done():
			case <-release:
			}
			return proxyUDPResult{Status: proxyUDPNoRelay}
		})
		server := startFakeSocks5(t, "", "", false)
		proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
		natSettings := proxiedIdentityNatSettings(proxySettings, checkStore)

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			conn, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", "192.0.2.1:53")
			if conn != nil {
				conn.Close()
			}
			result <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatalf("the dial never asked for the udp check")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the dial did not return after cancel")
		}
		if got := server.accepts.Load(); got != 0 {
			t.Fatalf("a canceled flow connected to the proxy %d times", got)
		}
	})

	t.Run("in the handshake", func(t *testing.T) {
		// a proxy that accepts and never answers
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		controlClosed := make(chan struct{})
		accepted := make(chan struct{})
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			close(accepted)
			io.Copy(io.Discard, conn)
			close(controlClosed)
		}()
		proxySettings := &connect.ProxySettings{Network: "tcp", Address: listener.Addr().String()}
		natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))

		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			conn, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", "192.0.2.1:53")
			if conn != nil {
				conn.Close()
			}
			result <- err
		}()
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatalf("the dial never reached the proxy")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("the dial did not return after cancel")
		}
		select {
		case <-controlClosed:
		case <-time.After(5 * time.Second):
			t.Fatalf("the control connection was left open")
		}
	})
}

// Reads only return the flow's own destination: a datagram the relay says came
// from anywhere else is dropped, as a connected udp socket would drop it.
func TestProxiedIdentityUdpReadsOnlyTheDestination(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", echo.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	relayPort := server.waitRelayPort(t)

	// teach the relay its client, then have another host write to the relay
	conn.Write([]byte("first"))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, 1500)
	if _, err := conn.Read(buffer); err != nil {
		t.Fatalf("read: %v", err)
	}
	other.WriteToUDP([]byte("not the destination"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: relayPort})
	// let the foreign datagram reach the conn first, so the filter has to drop it
	time.Sleep(100 * time.Millisecond)
	conn.Write([]byte("second"))
	n, err := conn.Read(buffer)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buffer[:n]) != "second" {
		t.Fatalf("read %q, want only the destination's datagrams", buffer[:n])
	}
}

func TestProxiedUdpDestination(t *testing.T) {
	for _, test := range []struct {
		network string
		addr    string
		ok      bool
	}{
		{network: "udp", addr: "192.0.2.1:53", ok: true},
		{network: "udp4", addr: "192.0.2.1:53", ok: true},
		// the check only proves the relay over ipv4
		{network: "udp6", addr: "[2001:db8::1]:53", ok: false},
		{network: "udp", addr: "[2001:db8::1]:53", ok: false},
		{network: "udp6", addr: "192.0.2.1:53", ok: false},
		{network: "udp4", addr: "[2001:db8::1]:53", ok: false},
		// a name would be resolved off the proxy
		{network: "udp", addr: "example.com:53", ok: false},
		{network: "udp", addr: "192.0.2.1", ok: false},
	} {
		_, err := proxiedUdpDestination(test.network, test.addr)
		if (err == nil) != test.ok {
			t.Errorf("%s %s: err = %v, want ok = %v", test.network, test.addr, err, test.ok)
		}
	}
}

// ipv4UdpPacket builds the packet a client sends into the tunnel.
func ipv4UdpPacket(source *net.UDPAddr, destination *net.UDPAddr, payload []byte) []byte {
	packet := make([]byte, 20+8+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	packet[8] = 64
	packet[9] = 17
	copy(packet[12:16], source.IP.To4())
	copy(packet[16:20], destination.IP.To4())
	sum := uint32(0)
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(packet[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	binary.BigEndian.PutUint16(packet[10:12], ^uint16(sum))
	binary.BigEndian.PutUint16(packet[20:22], uint16(source.Port))
	binary.BigEndian.PutUint16(packet[22:24], uint16(destination.Port))
	binary.BigEndian.PutUint16(packet[24:26], uint16(8+len(payload)))
	copy(packet[28:], payload)
	return packet
}

// Composed: a client datagram sent into a proxied identity's local user nat
// leaves through the proxy's relay and the reply comes back to the client, and
// closing the nat ends the association.
func TestProxiedIdentityLocalUserNatCarriesUdp(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, seen := startEchoUdp(t)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bw := &connect.ProxyBandwidth{}
	nat := connect.NewLocalUserNat(ctx, "test", bw, natSettings)
	replies := make(chan []byte, 4)
	nat.AddReceivePacketCallback(func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		select {
		case replies <- append([]byte(nil), packet...):
		default:
		}
	})

	client := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 40000}
	destination := echo.LocalAddr().(*net.UDPAddr)
	source := connect.SourceId(connect.NewId())
	packet := ipv4UdpPacket(client, destination, []byte("through the tunnel"))
	if !nat.SendPacket(source, protocol.ProvideMode_Network, connect.MessagePoolCopy(packet), 5*time.Second) {
		t.Fatalf("the nat did not take the packet")
	}

	relayPort := server.waitRelayPort(t)
	select {
	case from := <-seen:
		if from.Port != relayPort {
			t.Fatalf("the destination saw the datagram from port %d, not the relay's %d", from.Port, relayPort)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the datagram never reached the destination")
	}
	select {
	case reply := <-replies:
		if len(reply) < 28 || string(reply[28:]) != "through the tunnel" {
			t.Fatalf("reply = %x", reply)
		}
		if !net.IP(reply[12:16]).Equal(destination.IP) || int(binary.BigEndian.Uint16(reply[20:22])) != destination.Port {
			t.Fatalf("the reply is not from the destination")
		}
		if !net.IP(reply[16:20]).Equal(client.IP) || int(binary.BigEndian.Uint16(reply[22:24])) != client.Port {
			t.Fatalf("the reply is not to the client")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no reply came back through the nat")
	}

	// the payload is counted once each way, as a direct socket's is
	payloadByteCount := uint64(len("through the tunnel"))
	if bw.TotalTx.Load() != payloadByteCount || bw.TotalRx.Load() != payloadByteCount {
		t.Fatalf("bandwidth tx/rx = %d/%d, want %d each", bw.TotalTx.Load(), bw.TotalRx.Load(), payloadByteCount)
	}

	nat.Close()
	waitForAssociations(t, server, 0)
}

// An ipv6 destination fails at once and never reaches the proxy, so the nat
// answers the client with an unreachable instead of a datagram that vanishes.
func TestProxiedIdentityUdpRefusesIpv6Fast(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, stubUdpCheckStore(proxyUDPOK))
	conn, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "[2001:db8::1]:443")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, errProxiedUdpIpv6Refused) {
		t.Fatalf("err = %v, want the ipv6 refusal", err)
	}
	if proxiedUdpRefusals.count(proxySettings, proxiedUdpRefusalIpv6) != 1 {
		t.Fatalf("the ipv6 refusal was not counted")
	}
	if got := server.accepts.Load(); got != 0 {
		t.Fatalf("an ipv6 flow connected to the proxy %d times", got)
	}
}

// The check result is remembered for the identity even when the flow that
// started it stopped waiting, so the next flow does not start over.
func TestProxiedIdentityUdpCheckOutlivesTheFlow(t *testing.T) {
	probes := atomic.Int64{}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseProbe)
	checkStore := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		probes.Add(1)
		<-release
		return proxyUDPResult{Status: proxyUDPNoRelay}
	})
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, checkStore)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := natSettings.UdpBufferSettings.DialContext(ctx, "udp", "192.0.2.1:53"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the flow's own deadline", err)
	}
	releaseProbe()
	deadline := time.Now().Add(5 * time.Second)
	for checkStore.size() != 1 || checkStore.Check(context.Background(), proxySettings, time.Now()).Status != proxyUDPNoRelay {
		if time.Now().After(deadline) {
			t.Fatalf("the check result was not remembered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "192.0.2.1:53"); !errors.Is(err, errProxiedUdpNoRelay) {
		t.Fatalf("err = %v, want the remembered no-relay", err)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want the one check reused", probes.Load())
	}
}

// Proxied identities hold a proxy connection per udp flow, so they close idle
// flows sooner than the nat default; the direct identity keeps the default.
func TestProxiedIdentityUdpFlowsIdleSooner(t *testing.T) {
	natSettings := proxiedIdentityNatSettings(&connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}, stubUdpCheckStore(proxyUDPOK))
	if natSettings.UdpBufferSettings.ReadTimeout > proxiedUdpFlowIdleTimeout || natSettings.UdpBufferSettings.IdleTimeout > proxiedUdpFlowIdleTimeout {
		t.Fatalf("proxied udp timeouts = %s / %s", natSettings.UdpBufferSettings.ReadTimeout, natSettings.UdpBufferSettings.IdleTimeout)
	}
	direct := connect.DefaultLocalUserNatSettings()
	want := direct.UdpBufferSettings.IdleTimeout
	applyIdentityNatConnectSettings(direct, *connect.DefaultConnectSettings(), nil, stubUdpCheckStore(proxyUDPOK))
	if direct.UdpBufferSettings.IdleTimeout != want {
		t.Fatalf("the direct identity's udp idle timeout changed")
	}
}

// A burst of flows on an identity whose check is still running shares one
// detached check instead of starting one each.
func TestProxiedIdentityUdpBurstSharesOneCheck(t *testing.T) {
	probes := atomic.Int64{}
	release := make(chan struct{})
	checkStore := newProxyUDPCheckStore(func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		probes.Add(1)
		<-release
		return proxyUDPResult{Status: proxyUDPNoRelay}
	})
	server := startFakeSocks5(t, "", "", false)
	proxySettings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	natSettings := proxiedIdentityNatSettings(proxySettings, checkStore)

	const flowCount = 50
	results := make(chan error, flowCount)
	for i := 0; i < flowCount; i += 1 {
		go func() {
			_, err := natSettings.UdpBufferSettings.DialContext(context.Background(), "udp", "192.0.2.1:53")
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for probes.Load() == 0 {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("no check started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// let every flow reach the shared run
	time.Sleep(100 * time.Millisecond)
	if got := func() int {
		checkStore.mu.Lock()
		defer checkStore.mu.Unlock()
		return len(checkStore.detachedRuns)
	}(); got != 1 {
		close(release)
		t.Fatalf("detached checks in flight = %d, want 1", got)
	}
	close(release)
	for i := 0; i < flowCount; i += 1 {
		if err := <-results; !errors.Is(err, errProxiedUdpNoRelay) {
			t.Fatalf("err = %v, want no-relay", err)
		}
	}
	if probes.Load() != 1 {
		t.Fatalf("probes = %d, want 1", probes.Load())
	}
}
