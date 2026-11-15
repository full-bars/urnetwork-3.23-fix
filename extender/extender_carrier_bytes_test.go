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
