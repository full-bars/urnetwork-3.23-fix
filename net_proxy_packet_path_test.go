package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestProxyExtenderPacketPath(t *testing.T) {
	canary, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer canary.Close()
	address := canary.LocalAddr().(*net.UDPAddr)
	settings := &ConnectSettings{ProxySettings: &ProxySettings{Address: address.String()}}
	conn, err := openExtenderPacketConn(context.Background(), settings, address)
	if conn != nil {
		defer conn.Close()
		if _, writeErr := conn.WriteTo([]byte("canary"), address); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if !errors.Is(err, ErrExtenderPacketCarrierUnavailable) {
		t.Errorf("got connection %v and error %v; expected unavailable packet carrier", conn, err)
	}
	if err := canary.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	if n, _, err := canary.ReadFromUDP(buf); err == nil {
		t.Fatalf("host packet reached canary: %q", buf[:n])
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatal(err)
	}
}

func TestProxyExtenderPacketFactory(t *testing.T) {
	endpoint, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	settings := &ConnectSettings{ProxySettings: &ProxySettings{}, DialContextSettings: &DialContextSettings{
		PacketConnFactory: func(context.Context) (net.PacketConn, error) { return endpoint, nil },
	}}
	got, err := openExtenderPacketConn(context.Background(), settings, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil || got != endpoint {
		t.Fatalf("factory returned %v, %v", got, err)
	}
}
