package connect

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestProxiedNatOpensNoIcmpSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultLocalUserNatSettings()
	settings.TcpBufferSettings.ProxySettings = &ProxySettings{Network: "tcp", Address: "127.0.0.1:1080"}
	nat := NewLocalUserNat(ctx, "test", nil, settings)
	defer nat.Close()
	count := 0
	previous := setIcmpEgressFactory(func(context.Context, int, net.IP, *IcmpBufferSettings) (icmpEgress, error) {
		count++
		return nil, errors.New("test backend")
	})
	defer setIcmpEgressFactory(previous)
	sequence := NewIcmpSequence(ctx, nil, SourceId(NewId()), protocol.ProvideMode_Public, 4, net.IPv4(10, 0, 0, 1), 1, net.IPv4(198, 51, 100, 3), nat.settings.IcmpBufferSettings)
	sequence.Run()
	if count != 0 {
		t.Fatalf("proxied NAT requested %d host ICMP sockets", count)
	}
}
