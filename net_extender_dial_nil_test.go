package connect

import (
	"context"
	"net"
	"testing"
)

func TestOpenExtenderPacketConnWithoutSettings(t *testing.T) {
	conn, err := openExtenderPacketConn(context.Background(), nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Fatalf("nil settings must open a plain local endpoint: %v", err)
	}
	conn.Close()
}
