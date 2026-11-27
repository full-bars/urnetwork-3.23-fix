package connect

import (
	"net"

	"golang.org/x/net/ipv4"
)

// countedUDPConn credits the bytes of a UDP socket to a proxy's total traffic,
// the way trackedConn does for a TCP connection. The H3 transport owns a raw
// UDP socket that bypasses trackedConn, so without this a direct identity's H3
// traffic never reached total (billable is counted at the IP layer and is not
// affected).
//
// It must keep satisfying quic-go's OOBCapablePacketConn, which the embedded
// *net.UDPConn does, or quic-go silently drops ECN, the DF bit and batching.
// quic-go reads through ipv4.NewPacketConn(c), which unwraps the file
// descriptor via SyscallConn and would bypass any read method overridden here,
// so the receive side is only counted because this type also implements
// ReadBatch, which quic-go uses directly when the conn provides it.
type countedUDPConn struct {
	*net.UDPConn
	bw    *ProxyBandwidth
	batch *ipv4.PacketConn
	// v6 is the family of the platform address this socket was opened for,
	// decided once at creation.
	v6 bool
}

func newCountedUDPConn(conn *net.UDPConn, bw *ProxyBandwidth, v6 bool) *countedUDPConn {
	return &countedUDPConn{UDPConn: conn, bw: bw, v6: v6, batch: ipv4.NewPacketConn(conn)}
}

func (self *countedUDPConn) addRx(n int) {
	if 0 < n {
		self.bw.TotalRx.Add(uint64(n))
		if self.v6 {
			self.bw.TotalRxV6.Add(uint64(n))
		}
	}
}

func (self *countedUDPConn) addTx(n int) {
	if 0 < n {
		self.bw.TotalTx.Add(uint64(n))
		if self.v6 {
			self.bw.TotalTxV6.Add(uint64(n))
		}
	}
}

func (self *countedUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := self.UDPConn.ReadFrom(b)
	self.addRx(n)
	return n, addr, err
}

func (self *countedUDPConn) ReadMsgUDP(b []byte, oob []byte) (int, int, int, *net.UDPAddr, error) {
	n, oobn, flags, addr, err := self.UDPConn.ReadMsgUDP(b, oob)
	self.addRx(n)
	return n, oobn, flags, addr, err
}

// ReadBatch is the path quic-go reads packets through.
func (self *countedUDPConn) ReadBatch(ms []ipv4.Message, flags int) (int, error) {
	n, err := self.batch.ReadBatch(ms, flags)
	for i := 0; i < n; i += 1 {
		self.addRx(ms[i].N)
	}
	return n, err
}

func (self *countedUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := self.UDPConn.WriteTo(b, addr)
	self.addTx(n)
	return n, err
}

func (self *countedUDPConn) WriteMsgUDP(b []byte, oob []byte, addr *net.UDPAddr) (int, int, error) {
	n, oobn, err := self.UDPConn.WriteMsgUDP(b, oob, addr)
	self.addTx(n)
	return n, oobn, err
}

// countH3Socket wraps the H3 UDP socket so its bytes count into the total of
// the identity this transport runs for. Without a registered bandwidth record
// the socket is returned as is.
func (self *PlatformTransport) countH3Socket(conn *net.UDPConn, v6 bool) net.PacketConn {
	if idx, ok := self.proxyIndex(); ok {
		if bw := RegisteredProxyBandwidth(idx); bw != nil {
			return newCountedUDPConn(conn, bw, v6)
		}
	}
	return conn
}

// countedPacketConn counts the bytes of an injected H3 endpoint (see
// PlatformTransportSettings.H3PacketConnFactory) the way countedUDPConn counts a
// host socket. An injected endpoint is a plain datagram conn (a proxy relay), so
// there is no batch read path to cover; the socket buffer setters quic-go asks
// for are forwarded when the endpoint has them.
type countedPacketConn struct {
	net.PacketConn
	bw *ProxyBandwidth
}

func (self *countedPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := self.PacketConn.ReadFrom(b)
	if 0 < n {
		self.bw.TotalRx.Add(uint64(n))
		if addrIsV6(addr) {
			self.bw.TotalRxV6.Add(uint64(n))
		}
	}
	return n, addr, err
}

func (self *countedPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := self.PacketConn.WriteTo(b, addr)
	if 0 < n {
		self.bw.TotalTx.Add(uint64(n))
		if addrIsV6(addr) {
			self.bw.TotalTxV6.Add(uint64(n))
		}
	}
	return n, err
}

func (self *countedPacketConn) SetReadBuffer(bytes int) error {
	if c, ok := self.PacketConn.(interface{ SetReadBuffer(int) error }); ok {
		return c.SetReadBuffer(bytes)
	}
	return nil
}

func (self *countedPacketConn) SetWriteBuffer(bytes int) error {
	if c, ok := self.PacketConn.(interface{ SetWriteBuffer(int) error }); ok {
		return c.SetWriteBuffer(bytes)
	}
	return nil
}

// countInjectedH3Conn wraps an injected H3 endpoint so its bytes count into the
// total of the identity this transport runs for, like countH3Socket. Without a
// registered bandwidth record the endpoint is returned as is.
func (self *PlatformTransport) countInjectedH3Conn(conn net.PacketConn) net.PacketConn {
	if conn == nil {
		return nil
	}
	if idx, ok := self.proxyIndex(); ok {
		if bw := RegisteredProxyBandwidth(idx); bw != nil {
			return &countedPacketConn{PacketConn: conn, bw: bw}
		}
	}
	return conn
}
