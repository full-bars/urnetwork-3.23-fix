package gossip

import (
	"context"
	"fmt"
	"github.com/libp2p/go-libp2p/core/crypto"
	sec "github.com/libp2p/go-libp2p/core/sec"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/muxer/yamux"
	"github.com/libp2p/go-libp2p/p2p/net/upgrader"
	"github.com/libp2p/go-libp2p/p2p/security/noise"
	connect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"net"
	"net/netip"
	"testing"
	"time"
)

func newTestLoopbackConnectSettings(
	t *testing.T,
	ip netip.Addr,
	loopbackAddr string,
) *connect.ConnectSettings {
	t.Helper()
	loopbackHost, _, err := net.SplitHostPort(loopbackAddr)
	if err != nil {
		t.Fatal(err)
	}
	connectSettings := connect.DefaultConnectSettings()
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(
			ctx context.Context,
			network string,
			address string,
		) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if host == ip.String() {
				address = net.JoinHostPort(loopbackHost, port)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	return connectSettings
}

func newTestExtenderServer(
	t *testing.T,
	ctx context.Context,
	key *testKey,
	tcpListener net.Listener,
	tcpPort int,
	configure func(settings *extender.ExtenderSettings),
) *extender.ExtenderServer {
	t.Helper()
	settings := extender.DefaultExtenderSettings()
	settings.IdentityKeySeed = key.seed
	settings.Listen = func(network string, address string) (net.Listener, error) {
		if address != fmt.Sprintf(":%d", tcpPort) {
			return nil, fmt.Errorf("unexpected extender listen %s %s", network, address)
		}
		return tcpListener, nil
	}
	if configure != nil {
		configure(settings)
	}
	// an open extender, which is what an operator activated one is (A4)
	server := extender.NewExtenderServer(
		ctx,
		nil,
		nil,
		map[int][]connect.ExtenderConnectMode{
			tcpPort: {connect.ExtenderConnectModeTcpTls},
		},
		&net.Dialer{},
		settings,
	)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.ListenAndServe()
	}()
	t.Cleanup(func() {
		server.CloseAndWait()
		select {
		case err := <-serveDone:
			if err != nil {
				t.Errorf("extender server: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the extender server did not stop")
		}
	})
	return server
}

func newTestUpgrader(t *testing.T, key *testKey) transport.Upgrader {
	t.Helper()
	privateKey, err := crypto.UnmarshalEd25519PrivateKey(key.privateKey)
	if err != nil {
		t.Fatal(err)
	}
	noiseTransport, err := noise.New(noise.ID, privateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	connUpgrader, err := upgrader.New(
		[]sec.SecureTransport{noiseTransport},
		[]upgrader.StreamMuxer{{ID: yamux.ID, Muxer: yamux.DefaultTransport}},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return connUpgrader
}
