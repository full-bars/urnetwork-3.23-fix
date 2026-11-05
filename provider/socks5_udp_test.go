package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/proxy"

	"github.com/urnetwork/connect"
)

// fakeSocks5 is a minimal SOCKS5 server that speaks UDP ASSOCIATE, with a real
// UDP relay, so the tests can prove that datagrams reach their destination
// through the relay and not from the client's own socket.
type fakeSocks5 struct {
	listener  net.Listener
	user      string
	password  string
	refuseUDP bool
	// reportIP, when set, is the relay address the proxy claims in its reply
	// (a hostile or NATed proxy may name any host)
	reportIP net.IP
	// tcpExitBody, when set, is what a CONNECT-then-GET through this proxy
	// answers (a stand-in for an "what is my address" service)
	tcpExitBody string

	// accepts counts control connections; associations counts the UDP
	// associations that are open right now
	accepts      atomic.Int64
	associations atomic.Int64

	mu        sync.Mutex
	relayPort int
	relayOnce chan struct{}
	dropAssoc chan struct{}
}

func startFakeSocks5(t *testing.T, user, password string, refuseUDP bool) *fakeSocks5 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeSocks5{
		listener:  listener,
		user:      user,
		password:  password,
		refuseUDP: refuseUDP,
		relayOnce: make(chan struct{}),
		dropAssoc: make(chan struct{}),
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server.accepts.Add(1)
			go server.serve(conn)
		}
	}()
	return server
}

func (self *fakeSocks5) address() string { return self.listener.Addr().String() }

func (self *fakeSocks5) waitRelayPort(t *testing.T) int {
	t.Helper()
	select {
	case <-self.relayOnce:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never opened a relay")
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.relayPort
}

func (self *fakeSocks5) serve(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, head[1])); err != nil {
		return
	}
	if self.user != "" {
		conn.Write([]byte{5, 2})
		auth := make([]byte, 2)
		if _, err := io.ReadFull(conn, auth); err != nil {
			return
		}
		user := make([]byte, auth[1])
		io.ReadFull(conn, user)
		plen := make([]byte, 1)
		io.ReadFull(conn, plen)
		password := make([]byte, plen[0])
		io.ReadFull(conn, password)
		if string(user) != self.user || string(password) != self.password {
			conn.Write([]byte{1, 1})
			return
		}
		conn.Write([]byte{1, 0})
	} else {
		conn.Write([]byte{5, 0})
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	switch request[3] {
	case 1:
		io.ReadFull(conn, make([]byte, 4+2))
	case 3:
		length := make([]byte, 1)
		io.ReadFull(conn, length)
		io.ReadFull(conn, make([]byte, int(length[0])+2))
	case 4:
		io.ReadFull(conn, make([]byte, 16+2))
	}
	if request[1] == 1 && self.tcpExitBody != "" {
		conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		reader := make([]byte, 1)
		var seen []byte
		for !(len(seen) >= 4 && string(seen[len(seen)-4:]) == "\r\n\r\n") {
			if _, err := io.ReadFull(conn, reader); err != nil {
				return
			}
			seen = append(seen, reader[0])
		}
		conn.Write([]byte("HTTP/1.0 200 OK\r\nContent-Type: text/plain\r\n\r\n" + self.tcpExitBody + "\n"))
		return
	}
	if request[1] != 3 || self.refuseUDP {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		conn.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer relay.Close()
	self.associations.Add(1)
	defer self.associations.Add(-1)
	port := relay.LocalAddr().(*net.UDPAddr).Port
	self.mu.Lock()
	self.relayPort = port
	self.mu.Unlock()
	reply := []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}
	if ip4 := self.reportIP.To4(); ip4 != nil {
		copy(reply[4:8], ip4)
	}
	binary.BigEndian.PutUint16(reply[8:], uint16(port))
	conn.Write(reply)
	select {
	case <-self.relayOnce:
	default:
		close(self.relayOnce)
	}

	done := make(chan struct{})
	go func() {
		// the association lives as long as the control connection
		io.Copy(io.Discard, conn)
		close(done)
	}()
	go func() {
		select {
		case <-self.dropAssoc:
			conn.Close()
		case <-done:
		}
	}()

	var client *net.UDPAddr
	buf := make([]byte, 65535)
	for {
		relay.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, from, err := relay.ReadFromUDP(buf)
		select {
		case <-done:
			return
		default:
		}
		if err != nil {
			continue
		}
		if client == nil || from.String() == client.String() {
			client = from
			if n < 10 || buf[0] != 0 || buf[1] != 0 || buf[2] != 0 || buf[3] != 1 {
				continue
			}
			destination := &net.UDPAddr{IP: net.IP(append([]byte(nil), buf[4:8]...)), Port: int(binary.BigEndian.Uint16(buf[8:10]))}
			relay.WriteToUDP(buf[10:n], destination)
			continue
		}
		// a datagram from a destination: wrap it and hand it to the client
		ip4 := from.IP.To4()
		wrapped := append([]byte{0, 0, 0, 1}, ip4...)
		wrapped = binary.BigEndian.AppendUint16(wrapped, uint16(from.Port))
		wrapped = append(wrapped, buf[:n]...)
		relay.WriteToUDP(wrapped, client)
	}
}

// startEchoUdp echoes datagrams and reports the source it saw them from.
func startEchoUdp(t *testing.T) (*net.UDPConn, chan *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan *net.UDPAddr, 16)
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			seen <- from
			conn.WriteToUDP(buf[:n], from)
		}
	}()
	return conn, seen
}

func dialForTest(t *testing.T, server *fakeSocks5, auth *proxy.Auth) net.PacketConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pc, err := dialSocks5UDP(ctx, "tcp", server.address(), auth)
	if err != nil {
		t.Fatalf("dialSocks5UDP: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	return pc
}

// The reason this exists: a datagram written to the packet conn must reach its
// destination FROM THE PROXY'S relay, never from the client's own socket, and
// the reply must come back with the SOCKS header stripped and the destination
// as the source address.
func TestSocks5UdpRoundTripGoesThroughTheRelay(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, seen := startEchoUdp(t)
	pc := dialForTest(t, server, nil)
	relayPort := server.waitRelayPort(t)

	if _, err := pc.WriteTo([]byte("hello quic"), echo.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, from, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if string(buf[:n]) != "hello quic" {
		t.Fatalf("payload = %q, want %q (header not stripped?)", buf[:n], "hello quic")
	}
	if from.String() != echo.LocalAddr().String() {
		t.Fatalf("source = %s, want the destination %s", from, echo.LocalAddr())
	}

	source := <-seen
	if source.Port != relayPort {
		t.Fatalf("the destination saw the datagram from port %d, want the relay's port %d: it did not go through the proxy", source.Port, relayPort)
	}
	clientPort := pc.LocalAddr().(*net.UDPAddr).Port
	if source.Port == clientPort {
		t.Fatalf("the destination saw the client's own socket (port %d): traffic bypassed the proxy", clientPort)
	}
}

func TestSocks5UdpAuthenticates(t *testing.T) {
	server := startFakeSocks5(t, "alice", "secret", false)
	dialForTest(t, server, &proxy.Auth{User: "alice", Password: "secret"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pc, err := dialSocks5UDP(ctx, "tcp", server.address(), &proxy.Auth{User: "alice", Password: "wrong"}); err == nil {
		pc.Close()
		t.Fatalf("a wrong password was accepted")
	}
	if pc, err := dialSocks5UDP(ctx, "tcp", server.address(), nil); err == nil {
		pc.Close()
		t.Fatalf("no credentials were accepted by a proxy that requires them")
	}
}

// A proxy that will not relay UDP is an ERROR. It must never turn into a
// silent fallback to a socket on the host, which is exactly the leak this
// replaces.
func TestSocks5UdpRefusalIsAnError(t *testing.T) {
	server := startFakeSocks5(t, "", "", true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pc, err := dialSocks5UDP(ctx, "tcp", server.address(), nil); err == nil {
		pc.Close()
		t.Fatalf("a proxy that refused UDP ASSOCIATE produced a packet conn")
	}
}

func TestSocks5UdpUnreachableProxyIsAnError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pc, err := dialSocks5UDP(ctx, "tcp", address, nil); err == nil {
		pc.Close()
		t.Fatalf("an unreachable proxy produced a packet conn")
	}
}

// The relay lives only as long as the control connection, so when the proxy
// drops it the packet conn must stop working instead of writing into the void.
func TestSocks5UdpReadEndsWhenTheProxyDropsTheAssociation(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	pc := dialForTest(t, server, nil)
	server.waitRelayPort(t)

	close(server.dropAssoc)

	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, _, err := pc.ReadFrom(make([]byte, 1500))
	if err == nil {
		t.Fatalf("ReadFrom succeeded after the proxy dropped the association")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("ReadFrom timed out (%v) instead of ending when the association dropped", time.Since(start))
	}
}

func TestSocks5UdpReadHonorsTheDeadline(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	pc := dialForTest(t, server, nil)
	pc.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, _, err := pc.ReadFrom(make([]byte, 1500))
	ne, ok := err.(net.Error)
	if !ok || !ne.Timeout() {
		t.Fatalf("ReadFrom = %v, want a timeout", err)
	}
}

// Only the relay may speak into the association: a datagram from anywhere
// else that lands on the client's socket must be ignored, or anyone who can
// reach the port could inject packets into the QUIC connection.
func TestSocks5UdpIgnoresDatagramsThatAreNotFromTheRelay(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	echo, _ := startEchoUdp(t)
	pc := dialForTest(t, server, nil)
	server.waitRelayPort(t)

	spoofer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer spoofer.Close()
	forged := append([]byte{0, 0, 0, 1, 127, 0, 0, 1, 0, 9}, []byte("forged")...)
	spoofer.WriteToUDP(forged, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pc.LocalAddr().(*net.UDPAddr).Port})

	pc.WriteTo([]byte("real"), echo.LocalAddr())
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "real" {
		t.Fatalf("got %q: a datagram that did not come from the relay was delivered", buf[:n])
	}
}

func TestSocks5UdpHeaderRoundTripsIpv4AndIpv6(t *testing.T) {
	for _, address := range []*net.UDPAddr{
		{IP: net.IPv4(203, 0, 113, 9), Port: 443},
		{IP: net.ParseIP("2001:db8::7"), Port: 4053},
	} {
		packet := append(encodeSocks5UdpHeader(address), []byte("payload")...)
		payload, from, ok := parseSocks5UdpPacket(packet)
		if !ok || string(payload) != "payload" || from.String() != address.String() {
			t.Fatalf("round trip of %s = %q from %v ok=%v", address, payload, from, ok)
		}
	}
	if _, _, ok := parseSocks5UdpPacket([]byte{0, 0, 1, 1, 1, 2, 3, 4, 0, 80, 'x'}); ok {
		t.Fatalf("a fragmented datagram was accepted")
	}
	if _, _, ok := parseSocks5UdpPacket([]byte{0, 0, 0}); ok {
		t.Fatalf("a truncated header was accepted")
	}
}

// The H3 factory for a proxied identity is the proxy or nothing: when the
// proxy cannot relay UDP it returns an error, so the platform transport falls
// back to a mode that goes through the proxy, and a socket on the host is
// never opened.
func TestH3FactoryForAProxiedIdentityNeverUsesTheHostSocket(t *testing.T) {
	stubUdpCheck(t, proxyUDPOK)
	server := startFakeSocks5(t, "", "", true)
	settings := &connect.ProxySettings{Network: "tcp", Address: server.address()}
	factory := newH3PacketConnFactory(settings)
	if factory == nil {
		t.Fatalf("a proxied identity got no factory, so H3 would use the host socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if pc, err := factory(ctx); err == nil {
		pc.Close()
		t.Fatalf("the factory returned a packet conn although the proxy refused UDP")
	}
}

func TestH3FactoryForADirectIdentityKeepsTheEngineDefault(t *testing.T) {
	if factory := newH3PacketConnFactory(nil); factory != nil {
		t.Fatalf("a direct identity got a factory; the engine default socket should be kept")
	}
}

// The relay address in the proxy's reply is never trusted: a hostile proxy
// could name an internal host, and a NATed one names an address the client
// cannot reach. The relay is the proxy's own address plus the reported port, so
// a reply naming some other host must change nothing.
func TestSocks5UdpUsesTheProxyAddressNotTheOneItReports(t *testing.T) {
	server := startFakeSocks5(t, "", "", false)
	server.reportIP = net.IPv4(10, 255, 255, 1)
	echo, _ := startEchoUdp(t)
	pc := dialForTest(t, server, nil)
	server.waitRelayPort(t)

	pc.WriteTo([]byte("still relayed"), echo.LocalAddr())
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("a proxy that reports another address broke the association: %v", err)
	}
	if string(buf[:n]) != "still relayed" {
		t.Fatalf("payload = %q", buf[:n])
	}
}

// stubUdpCheck makes the H3 factory's UDP check answer with a fixed status, so
// tests that are about something else do not reach for the public internet.
func stubUdpCheck(t *testing.T, status proxyUDPStatus) {
	t.Helper()
	previous := globalProxyUDPCheck
	globalProxyUDPCheck = newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		return proxyUDPResult{Status: status}
	})
	t.Cleanup(func() { globalProxyUDPCheck = previous })
}
