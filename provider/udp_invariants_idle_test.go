package main

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

type invariantIdleConn struct {
	closed chan struct{}
	once   sync.Once
}

func (c *invariantIdleConn) Read([]byte) (int, error)         { <-c.closed; return 0, io.EOF }
func (c *invariantIdleConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *invariantIdleConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *invariantIdleConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *invariantIdleConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *invariantIdleConn) SetDeadline(time.Time) error      { return nil }
func (c *invariantIdleConn) SetReadDeadline(time.Time) error  { return nil }
func (c *invariantIdleConn) SetWriteDeadline(time.Time) error { return nil }

func TestUDPInvariantIdleClosesFlowAt120Seconds(t *testing.T) {
	warm := connect.MessagePoolCopy([]byte("warm"))
	connect.MessagePoolReturn(warm)
	synctest.Test(t, func(t *testing.T) {
		settings := proxiedIdentityNatSettings(&connect.ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}, stubUdpCheckStore(proxyUDPOK))
		if settings.UdpBufferSettings.IdleTimeout != 120*time.Second {
			t.Fatalf("idle timeout = %s", settings.UdpBufferSettings.IdleTimeout)
		}
		conn := &invariantIdleConn{closed: make(chan struct{})}
		settings.UdpBufferSettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{DialContext: func(context.Context, string, string) (net.Conn, error) { return conn, nil }}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		nat := connect.NewLocalUserNat(ctx, "test", nil, settings)
		defer nat.Close()
		packet := ipv4UdpPacket(&net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 40000}, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 53}, []byte("test"))
		if !nat.SendPacket(connect.SourceId(connect.NewId()), protocol.ProvideMode_Network, connect.MessagePoolCopy(packet), time.Second) {
			t.Fatal("packet not accepted")
		}
		synctest.Wait()
		time.Sleep(119 * time.Second)
		synctest.Wait()
		select {
		case <-conn.closed:
			t.Fatal("flow closed before idle limit")
		default:
		}
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case <-conn.closed:
		default:
			t.Fatal("flow remained open at idle limit")
		}
	})
}
