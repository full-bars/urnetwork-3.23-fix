package connect

import (
	"io"
	"net"
	"testing"
)

type trafficTestConn struct{ net.Conn }

func (*trafficTestConn) Read(b []byte) (int, error)  { return copy(b, "reply"), nil }
func (*trafficTestConn) Write(b []byte) (int, error) { return len(b), nil }
func (*trafficTestConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 80}
}

type trafficTestPacketConn struct{ net.PacketConn }

func (*trafficTestPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	return copy(b, "reply"), &net.UDPAddr{IP: net.ParseIP("::1")}, nil
}
func (*trafficTestPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }

func TestTrafficAccountingWithoutNetwork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		packet bool
	}{{"direct-tcp", false}, {"relay-udp", false}, {"h3-datagram", true}} {
		t.Run(tc.name, func(t *testing.T) {
			bw := &ProxyBandwidth{}
			if tc.packet {
				conn := &countedPacketConn{PacketConn: &trafficTestPacketConn{}, bw: bw}
				if _, err := conn.WriteTo([]byte("out"), &net.UDPAddr{IP: net.ParseIP("::1")}); err != nil {
					t.Fatal(err)
				}
				if _, _, err := conn.ReadFrom(make([]byte, 5)); err != nil {
					t.Fatal(err)
				}
			} else {
				conn := trackDirect(&trafficTestConn{}, bw)
				if _, err := conn.Write([]byte("out")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(conn, make([]byte, 5)); err != nil {
					t.Fatal(err)
				}
				if trackDirect(conn, bw) != conn {
					t.Fatal("already tracked connection wrapped twice")
				}
			}
			if bw.TotalRx.Load() != 5 || bw.TotalTx.Load() != 3 || bw.BillableRx.Load() != 0 || bw.BillableTx.Load() != 0 {
				t.Fatalf("unexpected totals rx=%d tx=%d", bw.TotalRx.Load(), bw.TotalTx.Load())
			}
		})
	}
}
