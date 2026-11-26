package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// With no setting, a bound lane delivers its own peer, drops the platform and
// self forgeries, and drops a third party by the binding rule before decode.
func TestP2pSourceBindingDropsByDefault(t *testing.T) {
	if P2PSourceBindingDefault != "drop" || P2PSourceBinding() != P2PSourceBindingDefault {
		t.Fatalf("default p2p source binding = %q (constant %q), want drop", P2PSourceBinding(), P2PSourceBindingDefault)
	}
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
	// The receive loop handles one lane in order, so the peer frame is
	// delivered only after the three before it were decided.
	select {
	case got := <-delivered:
		if got != "peer" {
			t.Fatalf("delivered %q, want the peer frame only", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("peer frame not delivered")
	}
	select {
	case got := <-delivered:
		t.Fatalf("unexpected delivery %q", got)
	default:
	}
	stats := client.ReceiveStats()
	if stats.SourceMismatchCount != 1 || stats.SourceMismatchDroppedCount != 1 || stats.SourceMismatchDecodedCount != 0 {
		t.Fatalf("mismatch stats = %+v, want one dropped", stats)
	}
	if stats.ForgedSourceDropCount != 2 {
		t.Fatalf("forged drops = %d, want 2", stats.ForgedSourceDropCount)
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
