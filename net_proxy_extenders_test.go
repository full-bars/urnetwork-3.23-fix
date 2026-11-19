package connect

import (
	"context"
	"net"
	"testing"
)

func TestDialableExtenderConfigsKeepsDialableCarriers(t *testing.T) {
	proxied := &ConnectSettings{
		ProxySettings: &ProxySettings{Network: "tcp", Address: "127.0.0.1:1"},
	}
	endpoint, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	proxiedPacket := &ConnectSettings{
		ProxySettings: &ProxySettings{Network: "tcp", Address: "127.0.0.1:1"},
		DialContextSettings: &DialContextSettings{
			PacketConnFactory: func(context.Context) (net.PacketConn, error) { return endpoint, nil },
		},
	}
	modes := []ExtenderConnectMode{
		ExtenderConnectModeTcpTls,
		ExtenderConnectModeQuic,
		ExtenderConnectModeDns,
		ExtenderConnectModeWebRtc,
	}
	configs := []*ExtenderConfig{}
	for _, mode := range modes {
		configs = append(configs, &ExtenderConfig{
			Profile: ExtenderProfile{ConnectMode: mode},
		})
	}
	{
		got := dialableExtenderConfigs(proxied, configs)
		if len(got) != 1 || got[0].Profile.ConnectMode != ExtenderConnectModeTcpTls {
			t.Fatalf("proxied configs = %+v", got)
		}
	}
	{
		got := dialableExtenderConfigs(proxiedPacket, configs)
		if len(got) != 3 {
			t.Fatalf("proxied packet-endpoint configs = %+v", got)
		}
	}
	{
		got := dialableExtenderConfigs(&ConnectSettings{}, configs)
		if len(got) != 4 {
			t.Fatalf("unproxied configs = %+v", got)
		}
	}
}
