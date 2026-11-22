package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func TestP2pSourceBindingDrops(t *testing.T) {
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
	for _, id := range []Id{ControlId, client.ClientId(), NewId()} {
		route <- forgeBarrierFrame(t, id, client.ClientId(), "forged")
	}
	route <- forgeBarrierFrame(t, peer, client.ClientId(), "peer")
	select {
	case got := <-delivered:
		if got != "peer" {
			t.Fatalf("unexpected delivery: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("peer frame not delivered")
	}
	if got := client.ReceiveStats().ForgedSourceDropCount; got != 3 {
		t.Fatalf("drops = %d, want 3", got)
	}
}
