package main

import (
	"context"
	"net"

	"github.com/urnetwork/connect"
)

// countDirectControl installs accounting for the native identity's control
// streams. NAT sockets have their own accounting and use separate settings.
func countDirectControl(settings *connect.ConnectSettings, bw *connect.ProxyBandwidth) {
	base := *settings
	var packetFactory func(context.Context) (net.PacketConn, error)
	if base.DialContextSettings != nil {
		packetFactory = base.DialContextSettings.PacketConnFactory
	}
	settings.DialContextSettings = &connect.DialContextSettings{
		PacketConnFactory: packetFactory,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := base.DialContext(ctx, network, addr)
			if err != nil || conn == nil {
				return conn, err
			}
			return &directControlConn{Conn: conn, bw: bw}, nil
		},
	}
}

type directControlConn struct {
	net.Conn
	bw *connect.ProxyBandwidth
}

func (c *directControlConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.bw != nil {
		c.bw.TotalRx.Add(uint64(n))
	}
	return n, err
}
func (c *directControlConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && c.bw != nil {
		c.bw.TotalTx.Add(uint64(n))
	}
	return n, err
}
