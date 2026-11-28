package connect

import (
	"net"
	"testing"
)

type fakeBufferedPacketConn struct {
	net.PacketConn
	readBuffer int
}

func (self *fakeBufferedPacketConn) SetReadBuffer(bytes int) error {
	self.readBuffer = bytes
	return nil
}

func TestCountedPacketConnCountsBothDirections(t *testing.T) {
	a, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	bw := &ProxyBandwidth{}
	counted := &countedPacketConn{PacketConn: a, bw: bw}
	if _, err := counted.WriteTo([]byte("hello"), b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 64)
	n, from, err := b.ReadFrom(buffer)
	if err != nil || n != 5 {
		t.Fatalf("read n=%d err=%v", n, err)
	}
	if _, err := b.WriteTo([]byte("reply!!"), from); err != nil {
		t.Fatal(err)
	}
	if n, _, err := counted.ReadFrom(buffer); err != nil || n != 7 {
		t.Fatalf("counted read n=%d err=%v", n, err)
	}
	if bw.TotalTx.Load() != 5 || bw.TotalRx.Load() != 7 {
		t.Fatalf("tx=%d rx=%d, want 5 and 7", bw.TotalTx.Load(), bw.TotalRx.Load())
	}
}

func TestCountedPacketConnForwardsBufferSizes(t *testing.T) {
	inner := &fakeBufferedPacketConn{}
	counted := &countedPacketConn{PacketConn: inner, bw: &ProxyBandwidth{}}
	if err := counted.SetReadBuffer(1234); err != nil || inner.readBuffer != 1234 {
		t.Fatalf("read buffer not forwarded (%d, %v)", inner.readBuffer, err)
	}
	// an endpoint without the setter is not an error
	plain := &countedPacketConn{PacketConn: struct{ net.PacketConn }{}, bw: &ProxyBandwidth{}}
	if err := plain.SetWriteBuffer(1); err != nil {
		t.Fatal(err)
	}
}

func TestCountInjectedH3ConnWithoutRecordIsIdentity(t *testing.T) {
	a, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	transport := &PlatformTransport{}
	if got := transport.countInjectedH3Conn(a); got != a {
		t.Fatalf("wrapped with no bandwidth record")
	}
	if transport.countInjectedH3Conn(nil) != nil {
		t.Fatalf("nil must stay nil")
	}
}

// familyPacketConn reports a different address for every datagram so a wrapper
// that decides the family once from LocalAddr cannot pass this test.
type familyPacketConn struct {
	net.PacketConn
	readAddr  net.Addr
	writeAddr net.Addr
}

func (c *familyPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	copy(b, "abcdef")
	return 6, c.readAddr, nil
}

func (c *familyPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.writeAddr = addr
	return len(b), nil
}

func TestCountedPacketConnClassifiesEachDatagram(t *testing.T) {
	v4 := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 443}
	v6 := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}
	bw := &ProxyBandwidth{}
	inner := &familyPacketConn{readAddr: v6}
	counted := &countedPacketConn{PacketConn: inner, bw: bw}
	buffer := make([]byte, 64)
	if _, _, err := counted.ReadFrom(buffer); err != nil {
		t.Fatal(err)
	}
	if bw.TotalRx.Load() != 6 || bw.TotalRxV6.Load() != 6 {
		t.Fatalf("rx after v6 read: total=%d v6=%d, want 6 and 6", bw.TotalRx.Load(), bw.TotalRxV6.Load())
	}
	if _, err := counted.WriteTo([]byte("abc"), v4); err != nil {
		t.Fatal(err)
	}
	if bw.TotalTx.Load() != 3 || bw.TotalTxV6.Load() != 0 {
		t.Fatalf("tx after v4 write: total=%d v6=%d, want 3 and 0", bw.TotalTx.Load(), bw.TotalTxV6.Load())
	}
	inner.readAddr = v4
	if _, _, err := counted.ReadFrom(buffer); err != nil {
		t.Fatal(err)
	}
	if bw.TotalRx.Load() != 12 || bw.TotalRxV6.Load() != 6 {
		t.Fatalf("rx after v4 read: total=%d v6=%d, want 12 and 6", bw.TotalRx.Load(), bw.TotalRxV6.Load())
	}
	if _, err := counted.WriteTo([]byte("abc"), v6); err != nil {
		t.Fatal(err)
	}
	if bw.TotalTx.Load() != 6 || bw.TotalTxV6.Load() != 3 {
		t.Fatalf("tx after v6 write: total=%d v6=%d, want 6 and 3", bw.TotalTx.Load(), bw.TotalTxV6.Load())
	}
}
