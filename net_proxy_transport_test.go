package connect

import (
	"context"
	"net"
	"testing"
)

func TestProxiedPlatformTransportOffersNoH3(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, c := range []struct {
		proxied        bool
		packetEndpoint bool
	}{{true, false}, {true, true}, {false, false}} {
		strategySettings := DefaultClientStrategySettings()
		if c.proxied {
			strategySettings.ProxySettings = &ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}
		}
		strategy := NewClientStrategy(ctx, strategySettings)
		settings := DefaultPlatformTransportSettings()
		settings.ModePreferences = DefaultTransportModePreferences()
		if c.packetEndpoint {
			settings.H3PacketConnFactory = func(ctx context.Context) (net.PacketConn, error) {
				return net.ListenPacket("udp4", "127.0.0.1:0")
			}
		}
		settings.StartDisabled = true
		transport := NewPlatformTransportWithTargetMode(ctx, strategy, NewRouteManager(ctx, "proxy"), "https://localhost", &ClientAuth{ByJwt: "jwt", InstanceId: NewId(), AppVersion: "1.0.0", ProvideIntent: true}, TransportModeH3, settings)
		expect := TransportModeH3
		if c.proxied {
			expect = TransportModeH1
		}
		if transport.targetMode != expect {
			t.Fatalf("proxied %t: target mode %s, expected %s", c.proxied, transport.targetMode, expect)
		}
		for _, mode := range []TransportMode{TransportModeH3, TransportModeH3Dns, TransportModeH3DnsPump} {
			if offered := transport.modePreference(mode) != modePreferenceNone; offered == c.proxied {
				t.Fatalf("proxied %t: offers %s = %t", c.proxied, mode, offered)
			}
		}
		transport.Close()
		strategy.Close()
	}
}
