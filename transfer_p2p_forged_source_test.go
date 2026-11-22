package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestP2pSourceBindingCountsByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	defer func() { cancel(); client.Close() }()
	peer := NewId()
	client.ContractManager().AddNoContractPeer(peer)
	route := make(Route, 8)
	client.RouteManager().UpdateTransportWithProperties(NewReceiveGatewayTransportWithType(TransportTypeP2p), []Route{route}, TransferCarrierProperties{boundSourceId: peer})
	delivered := make(chan string, 4)
	client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, f := range frames {
			if m, err := FromFrame(f); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					delivered <- sm.Content
				}
			}
		}
	})
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)
	for _, id := range []Id{ControlId, client.ClientId(), third} {
		route <- forgeBarrierFrame(t, id, client.ClientId(), "forged")
	}
	route <- forgeBarrierFrame(t, peer, client.ClientId(), "peer")
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case got := <-delivered:
			seen[got] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("frames delivered: %v", seen)
		}
	}
	if !seen["forged"] || !seen["peer"] {
		t.Fatalf("unexpected deliveries: %v", seen)
	}
	if got := client.ReceiveStats().SourceMismatchCount; got != 1 {
		t.Fatalf("mismatches = %d, want 1", got)
	}
	if got := client.ReceiveStats().ForgedSourceDropCount; got != 2 {
		t.Fatalf("drops = %d, want 2", got)
	}
}

func TestUnboundAndRelayedSourcesRemainAccepted(t *testing.T) {
	for _, transportType := range []TransportType{TransportTypeP2p, TransportTypeH1} {
		t.Run(string(transportType), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			settings := DefaultClientSettings()
			settings.ControlPingTimeout = 0
			client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
			defer func() { cancel(); client.Close() }()
			peer := NewId()
			client.ContractManager().AddNoContractPeer(peer)
			route := make(Route, 2)
			client.RouteManager().UpdateTransport(NewReceiveGatewayTransportWithType(transportType), []Route{route})
			delivered := make(chan bool, 1)
			client.AddReceiveCallback(func(_ TransferPath, _ []*protocol.Frame, _ Peer) { delivered <- true })
			route <- forgeBarrierFrame(t, peer, client.ClientId(), "peer")
			select {
			case <-delivered:
			case <-time.After(10 * time.Second):
				t.Fatal("frame not delivered")
			}
			if got := client.ReceiveStats().ForgedSourceDropCount; got != 0 {
				t.Fatalf("drops = %d", got)
			}
		})
	}
}
