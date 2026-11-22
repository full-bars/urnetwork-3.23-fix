package connect

import (
	"net"
	"testing"
	"time"
)

// bareConn is a net.Conn that reports how many bytes a caller wrote.
type bareConn struct{}

func (c *bareConn) Read(b []byte) (int, error)         { return 0, nil }
func (c *bareConn) Write(b []byte) (int, error)        { return len(b), nil }
func (c *bareConn) Close() error                       { return nil }
func (c *bareConn) LocalAddr() net.Addr                { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *bareConn) RemoteAddr() net.Addr               { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *bareConn) SetDeadline(t time.Time) error      { return nil }
func (c *bareConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *bareConn) SetWriteDeadline(t time.Time) error { return nil }

// TestAddrIsV6 proves the per-connection family test, which every total byte
// path stores once: it reads a v4, a v6 and a host-only address, and it never
// credits a family it cannot prove to IPv6.
func TestAddrIsV6(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want bool
	}{
		{"nil", nil, false},
		{"ipv4 host port", &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 443}, false},
		{"ipv6 host port", &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443}, true},
		{"ipv4 host only", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 9)}, false},
		{"unparseable", testAddr("not-an-address"), false},
	}
	for _, c := range cases {
		if got := addrIsV6(c.addr); got != c.want {
			t.Fatalf("%s: addrIsV6 = %v, want %v", c.name, got, c.want)
		}
	}
}

// testAddr is a net.Addr with a fixed string, for the unparseable case.
type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

// TestIPv6PacketHeader proves the packet-header family test used where no
// parsed path exists. An empty packet is never IPv6.
func TestIPv6PacketHeader(t *testing.T) {
	if ipv6Packet(nil) || ipv6Packet([]byte{}) {
		t.Fatal("an empty packet counted as IPv6")
	}
	if ipv6Packet([]byte{0x45, 0, 0, 20}) {
		t.Fatal("an IPv4 header counted as IPv6")
	}
	if !ipv6Packet([]byte{0x60, 0, 0, 0}) {
		t.Fatal("an IPv6 header did not count as IPv6")
	}
}

// TestTrackedConnSplitsTotalByFamily proves the socket byte counters credit
// TotalRxV6 and TotalTxV6 only on a connection whose remote address is IPv6,
// and that a v4 connection leaves them at zero.
func TestTrackedConnSplitsTotalByFamily(t *testing.T) {
	bw := &ProxyBandwidth{}

	v4 := &trackedConn{Conn: &bareConn{}, bw: bw, v6: addrIsV6(&net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 443})}
	if _, err := v4.Write(make([]byte, 50)); err != nil {
		t.Fatal(err)
	}
	if bw.TotalTx.Load() != 50 || bw.TotalTxV6.Load() != 0 {
		t.Fatalf("v4 write: total=%d totalV6=%d, want 50/0", bw.TotalTx.Load(), bw.TotalTxV6.Load())
	}

	v6 := &trackedConn{Conn: &bareConn{}, bw: bw, v6: addrIsV6(&net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 443})}
	if _, err := v6.Write(make([]byte, 50)); err != nil {
		t.Fatal(err)
	}
	if bw.TotalTx.Load() != 100 || bw.TotalTxV6.Load() != 50 {
		t.Fatalf("v6 write: total=%d totalV6=%d, want 100/50", bw.TotalTx.Load(), bw.TotalTxV6.Load())
	}
}

// TestCountedUDPConnAddSplitsByFamily proves the QUIC socket counters credit the
// IPv6 totals only on a socket opened for an IPv6 platform address, in both
// directions.
func TestCountedUDPConnAddSplitsByFamily(t *testing.T) {
	bw := &ProxyBandwidth{}

	v4 := &countedUDPConn{bw: bw, v6: false}
	v4.addRx(10)
	v4.addTx(20)
	if bw.TotalRx.Load() != 10 || bw.TotalRxV6.Load() != 0 || bw.TotalTx.Load() != 20 || bw.TotalTxV6.Load() != 0 {
		t.Fatalf("v4 socket: rx=%d rxV6=%d tx=%d txV6=%d, want 10/0/20/0",
			bw.TotalRx.Load(), bw.TotalRxV6.Load(), bw.TotalTx.Load(), bw.TotalTxV6.Load())
	}

	v6 := &countedUDPConn{bw: bw, v6: true}
	v6.addRx(10)
	v6.addTx(20)
	if bw.TotalRx.Load() != 20 || bw.TotalRxV6.Load() != 10 || bw.TotalTx.Load() != 40 || bw.TotalTxV6.Load() != 20 {
		t.Fatalf("v6 socket: rx=%d rxV6=%d tx=%d txV6=%d, want 20/10/40/20",
			bw.TotalRx.Load(), bw.TotalRxV6.Load(), bw.TotalTx.Load(), bw.TotalTxV6.Load())
	}
}
