package main

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/urnetwork/connect"
)

type memoryControlConn struct{ net.Conn }

func (*memoryControlConn) Read(p []byte) (int, error)  { return copy(p, "hello"), nil }
func (*memoryControlConn) Write(p []byte) (int, error) { return len(p), nil }
func TestDirectControlCountsBothFamilies(t *testing.T) {
	for _, family := range []string{"tcp4", "tcp6"} {
		t.Run(family, func(t *testing.T) {
			bw := &connect.ProxyBandwidth{}
			settings := connect.ConnectSettings{DialContextSettings: &connect.DialContextSettings{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) { return &memoryControlConn{}, nil }}}
			countDirectControl(&settings, bw)
			conn, err := settings.DialContext(context.Background(), family, "127.0.0.1:443")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = conn.Write([]byte("abc")); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 5)
			if _, err = io.ReadFull(conn, buf); err != nil {
				t.Fatal(err)
			}
			if bw.TotalRx.Load() != 5 || bw.TotalTx.Load() != 3 || bw.BillableRx.Load() != 0 || bw.BillableTx.Load() != 0 {
				t.Fatal("unexpected totals")
			}
		})
	}
}
