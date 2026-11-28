package extender

import (
	"context"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

// Each relayed byte is counted once on its carrier, on the side it travelled, and
// the carriers always sum to the totals.
func TestExtenderRelayCountsBytesPerCarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		server := &ExtenderServer{settings: settings}
		cases := []struct {
			carrier string
			in, out int
		}{
			{ExtenderCarrierTcp443, 100, 40},
			{ExtenderCarrierUdp443, 7, 900},
			{ExtenderCarrierUdp4053, 55, 66},
			{ExtenderCarrierUdp53, 1, 2},
		}
		for _, tc := range cases {
			clientConn, clientPeer := net.Pipe()
			forwardConn, forwardPeer := net.Pipe()
			ctx, cancel := context.WithCancel(newExtenderRequestContextWithCarrier(context.Background(), "", tc.carrier))
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.relay(ctx, cancel, clientConn, forwardConn)
			}()
			go clientPeer.Write(make([]byte, tc.in))
			if _, err := io.ReadFull(forwardPeer, make([]byte, tc.in)); err != nil {
				t.Fatal(err)
			}
			go forwardPeer.Write(make([]byte, tc.out))
			if _, err := io.ReadFull(clientPeer, make([]byte, tc.out)); err != nil {
				t.Fatal(err)
			}
			cancel()
			<-done
			clientPeer.Close()
			forwardPeer.Close()
		}
		stats := server.CarrierByteStats(time.Now())
		var sumIn, sumOut uint64
		for _, tc := range cases {
			got := stats[tc.carrier]
			if got.IngressBytes != uint64(tc.in) || got.EgressBytes != uint64(tc.out) {
				t.Errorf("%s: in %d out %d, want in %d out %d", tc.carrier, got.IngressBytes, got.EgressBytes, tc.in, tc.out)
			}
			if got.ConnsTotal != 1 || got.LiveConns != 0 {
				t.Errorf("%s: conns total %d live %d, want 1 and 0", tc.carrier, got.ConnsTotal, got.LiveConns)
			}
			sumIn += got.IngressBytes
			sumOut += got.EgressBytes
		}
		if sumIn != uint64(server.ingressByteCount.Load()) || sumOut != uint64(server.egressByteCount.Load()) {
			t.Errorf("carriers sum to in %d out %d, totals are in %d out %d", sumIn, sumOut, server.ingressByteCount.Load(), server.egressByteCount.Load())
		}
	})
}

type mockAddrConn struct {
	net.Conn
	remote net.Addr
}

func (c *mockAddrConn) RemoteAddr() net.Addr {
	return c.remote
}

// Relayed traffic splits bytes and connection counts by IP family.
func TestExtenderRelayCountsBytesPerFamily(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultExtenderSettings()
		server := &ExtenderServer{settings: settings}

		cases := []struct {
			carrier  string
			remoteIP string
			family   string
			in, out  int
		}{
			{ExtenderCarrierTcp443, "192.0.2.1", "v4", 120, 80},
			{ExtenderCarrierTcp443, "2001:db8::1", "v6", 60, 40},
			{ExtenderCarrierUdp443, "192.0.2.2", "v4", 200, 100},
			{ExtenderCarrierUdp443, "2001:db8::2", "v6", 300, 150},
		}

		for _, tc := range cases {
			clientConn, clientPeer := net.Pipe()
			forwardConn, forwardPeer := net.Pipe()
			wrappedClient := &mockAddrConn{
				Conn:   clientConn,
				remote: &net.TCPAddr{IP: net.ParseIP(tc.remoteIP), Port: 12345},
			}
			ctx, cancel := context.WithCancel(newExtenderRequestContextWithCarrier(context.Background(), "", tc.carrier))
			done := make(chan struct{})
			go func() {
				defer close(done)
				server.relay(ctx, cancel, wrappedClient, forwardConn)
			}()
			go clientPeer.Write(make([]byte, tc.in))
			if _, err := io.ReadFull(forwardPeer, make([]byte, tc.in)); err != nil {
				t.Fatal(err)
			}
			go forwardPeer.Write(make([]byte, tc.out))
			if _, err := io.ReadFull(clientPeer, make([]byte, tc.out)); err != nil {
				t.Fatal(err)
			}
			cancel()
			<-done
			clientPeer.Close()
			forwardPeer.Close()
		}

		stats := server.CarrierByteStats(time.Now())

		// tcp-443 checks
		tcp := stats[ExtenderCarrierTcp443]
		if tcp.IngressBytes != 180 || tcp.EgressBytes != 120 {
			t.Errorf("tcp-443 total in %d out %d, want 180 and 120", tcp.IngressBytes, tcp.EgressBytes)
		}
		if tcp.IngressBytesV4 != 120 || tcp.EgressBytesV4 != 80 {
			t.Errorf("tcp-443 v4 in %d out %d, want 120 and 80", tcp.IngressBytesV4, tcp.EgressBytesV4)
		}
		if tcp.IngressBytesV6 != 60 || tcp.EgressBytesV6 != 40 {
			t.Errorf("tcp-443 v6 in %d out %d, want 60 and 40", tcp.IngressBytesV6, tcp.EgressBytesV6)
		}
		if tcp.ConnsTotal != 2 || tcp.ConnsTotalV4 != 1 || tcp.ConnsTotalV6 != 1 {
			t.Errorf("tcp-443 conns total %d (v4: %d, v6: %d), want 2 (1, 1)", tcp.ConnsTotal, tcp.ConnsTotalV4, tcp.ConnsTotalV6)
		}
		if tcp.LiveConns != 0 || tcp.LiveConnsV4 != 0 || tcp.LiveConnsV6 != 0 {
			t.Errorf("tcp-443 live conns %d (v4: %d, v6: %d), want 0 (0, 0)", tcp.LiveConns, tcp.LiveConnsV4, tcp.LiveConnsV6)
		}

		// udp-443 checks
		udp := stats[ExtenderCarrierUdp443]
		if udp.IngressBytes != 500 || udp.EgressBytes != 250 {
			t.Errorf("udp-443 total in %d out %d, want 500 and 250", udp.IngressBytes, udp.EgressBytes)
		}
		if udp.IngressBytesV4 != 200 || udp.EgressBytesV4 != 100 {
			t.Errorf("udp-443 v4 in %d out %d, want 200 and 100", udp.IngressBytesV4, udp.EgressBytesV4)
		}
		if udp.IngressBytesV6 != 300 || udp.EgressBytesV6 != 150 {
			t.Errorf("udp-443 v6 in %d out %d, want 300 and 150", udp.IngressBytesV6, udp.EgressBytesV6)
		}
		if udp.ConnsTotal != 2 || udp.ConnsTotalV4 != 1 || udp.ConnsTotalV6 != 1 {
			t.Errorf("udp-443 conns total %d (v4: %d, v6: %d), want 2 (1, 1)", udp.ConnsTotal, udp.ConnsTotalV4, udp.ConnsTotalV6)
		}
	})
}
