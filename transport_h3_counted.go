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
}

func newCountedUDPConn(conn *net.UDPConn, bw *ProxyBandwidth) *countedUDPConn {
	return &countedUDPConn{UDPConn: conn, bw: bw, batch: ipv4.NewPacketConn(conn)}
}

func (self *countedUDPConn) addRx(n int) {
	if 0 < n {
		self.bw.TotalRx.Add(uint64(n))
	}
}

func (self *countedUDPConn) addTx(n int) {
	if 0 < n {
		self.bw.TotalTx.Add(uint64(n))
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
func (self *PlatformTransport) countH3Socket(conn *net.UDPConn) net.PacketConn {
	if idx, ok := self.proxyIndex(); ok {
		if bw := RegisteredProxyBandwidth(idx); bw != nil {
			return newCountedUDPConn(conn, bw)
		}
	}
	return conn
}
