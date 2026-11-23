package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// newSourcePolicyClient builds a client with one receive lane. A bound lane
// carries the peer the adjacent signalling authenticated and drains every
// frame handed to it.
func newSourcePolicyClient(t *testing.T, transportType TransportType, boundSourceId Id, routeCapacity int) (*Client, Route) {
	return newSourcePolicyClientWith(t, transportType, boundSourceId, routeCapacity, nil)
}

// newSourcePolicyClientWithEncryption is newSourcePolicyClient with the
// receive client's per-peer encryption mode set, for the fail-closed plaintext
// gate.
func newSourcePolicyClientWithEncryption(t *testing.T, transportType TransportType, boundSourceId Id, routeCapacity int, mode EncryptionMode) (*Client, Route) {
	return newSourcePolicyClientWith(t, transportType, boundSourceId, routeCapacity, func(settings *ClientSettings) {
		settings.EncryptionSettings.Mode = mode
	})
}

func newSourcePolicyClientWith(t *testing.T, transportType TransportType, boundSourceId Id, routeCapacity int, configure func(*ClientSettings)) (*Client, Route) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	settings.ControlPingTimeout = 0
	if configure != nil {
		configure(settings)
	}
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	t.Cleanup(func() { cancel(); client.Close() })
	route := make(Route, routeCapacity)
	client.RouteManager().UpdateTransportWithProperties(
		NewReceiveGatewayTransportWithType(transportType),
		[]Route{route},
		TransferCarrierProperties{boundSourceId: boundSourceId},
	)
	return client, route
}

// waitP2PSourceStats polls the receive snapshot until the condition holds,
// because the receive loop counts on its own goroutine.
func waitP2PSourceStats(t *testing.T, client *Client, condition func(ClientReceiveStatsSnapshot) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if condition(client.ReceiveStats()) {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("timed out waiting for %s, stats = %+v", what, client.ReceiveStats())
		}
		time.Sleep(time.Millisecond)
	}
}

func setSourcePolicy(t *testing.T, mode string, forgedDrop bool) {
	t.Helper()
	if err := SetP2PSourceBinding(mode); err != nil {
		t.Fatal(err)
	}
	SetP2PForgedSourceDrop(forgedDrop)
	t.Cleanup(func() {
		_ = SetP2PSourceBinding("count")
		SetP2PForgedSourceDrop(true)
	})
}

// deliverContent collects the content of every SimpleMessage the client
// delivers.
func deliverContent(client *Client) chan string {
	delivered := make(chan string, 16)
	client.AddReceiveCallback(func(_ TransferPath, frames []*protocol.Frame, _ Peer) {
		for _, f := range frames {
			if m, err := FromFrame(f); err == nil {
				if sm, ok := m.(*protocol.SimpleMessage); ok {
					delivered <- sm.Content
				}
			}
		}
	})
	return delivered
}

func expectDelivery(t *testing.T, delivered chan string, want string) {
	t.Helper()
	select {
	case got := <-delivered:
		if got != want {
			t.Fatalf("delivered %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%q not delivered", want)
	}
}

func expectNoDelivery(t *testing.T, delivered chan string) {
	t.Helper()
	select {
	case got := <-delivered:
		t.Fatalf("unexpected delivery %q", got)
	case <-time.After(250 * time.Millisecond):
	}
}

// A third party frame on a bound lane reaches the receive path in count mode,
// decodes and is counted as decoded. This is what a legitimate multihop frame
// looks like at its final destination.
func TestP2PSourceMismatchDecodedInCountMode(t *testing.T) {
	setSourcePolicy(t, "count", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	route <- forgeBarrierFrame(t, third, client.ClientId(), "relayed")

	expectDelivery(t, delivered, "relayed")
	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchDecodedCount == 1
	}, "one decoded mismatch")
	if stats := client.ReceiveStats(); stats.SourceMismatchCount != 1 ||
		stats.SourceMismatchRejectedCount != 0 ||
		stats.SourceMismatchDroppedCount != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// A third party frame on a bound lane that cannot be decrypted is counted as
// rejected. A frame cannot hold a session it has no key for, so this is the
// spoofing signal.
func TestP2PSourceMismatchRejectedInCountMode(t *testing.T) {
	setSourcePolicy(t, "count", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
	third := NewId()
	sessionRole := protocol.SequenceRole_SequenceRoleClient
	sealed, err := proto.Marshal(&protocol.TransferFrame{
		TransferPath:           TransferPath{SourceId: third, DestinationId: client.ClientId()}.ToProtobuf(),
		EncryptedTransferFrame: make([]byte, 32),
		SessionRole:            &sessionRole,
	})
	if err != nil {
		t.Fatal(err)
	}

	route <- sealed

	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchRejectedCount == 1
	}, "one rejected mismatch")
	if stats := client.ReceiveStats(); stats.SourceMismatchDecodedCount != 0 || stats.SourceMismatchCount != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// A mismatched frame addressed to another destination is handed to a forward
// callback and leaves this client. This client neither decoded nor dropped it,
// so it records no mismatch outcome: the frame was not evidence about traffic
// this client accepted. With no callback the frame is discarded and counted
// dropped.
func TestP2PSourceMismatchAddressedElsewhere(t *testing.T) {
	t.Run("forwarded by callback", func(t *testing.T) {
		setSourcePolicy(t, "count", true)
		peer := NewId()
		client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
		forwarded := make(chan TransferPath, 2)
		client.AddForwardCallback(func(path TransferPath, _ []byte) {
			forwarded <- path
		})
		third := NewId()
		destination := NewId()

		route <- forgeBarrierFrame(t, third, destination, "transit")

		select {
		case path := <-forwarded:
			if path.SourceId != third || path.DestinationId != destination {
				t.Fatalf("forwarded %v -> %v", path.SourceId, path.DestinationId)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("frame not forwarded")
		}
		// give the receive loop a moment to reach the policy check
		time.Sleep(100 * time.Millisecond)
		if stats := client.ReceiveStats(); stats.SourceMismatchCount != 0 ||
			stats.SourceMismatchDecodedCount != 0 ||
			stats.SourceMismatchRejectedCount != 0 ||
			stats.SourceMismatchDroppedCount != 0 {
			t.Fatalf("forwarded frame recorded an outcome it did not have: %+v", stats)
		}
	})

	t.Run("discarded without a callback", func(t *testing.T) {
		setSourcePolicy(t, "count", true)
		peer := NewId()
		client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
		third := NewId()

		route <- forgeBarrierFrame(t, third, NewId(), "transit")

		waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
			return stats.SourceMismatchDroppedCount == 1
		}, "one dropped mismatch")
	})
}

// Drop mode is the strict rule: the mismatching frame does not reach the
// receive path and is counted as dropped.
func TestP2PSourceMismatchDroppedInDropMode(t *testing.T) {
	setSourcePolicy(t, "drop", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	route <- forgeBarrierFrame(t, third, client.ClientId(), "dropped")

	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchDroppedCount == 1
	}, "one dropped mismatch")
	expectNoDelivery(t, delivered)
	if stats := client.ReceiveStats(); stats.SourceMismatchCount != 1 || stats.SourceMismatchDecodedCount != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

// Off mode delivers the frame and counts nothing.
func TestP2PSourceBindingOffCountsNothing(t *testing.T) {
	setSourcePolicy(t, "off", true)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 2)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	route <- forgeBarrierFrame(t, third, client.ClientId(), "unmeasured")

	expectDelivery(t, delivered, "unmeasured")
	if stats := client.ReceiveStats(); stats.SourceMismatchCount != 0 {
		t.Fatalf("mismatches counted while off: %+v", stats)
	}
}

// The enforced half counts a platform-named frame and a self-named frame
// under their own kinds, and drops both in every binding mode.
func TestP2PForgedSourceDropsByKind(t *testing.T) {
	for _, mode := range []string{"off", "count", "drop"} {
		t.Run(mode, func(t *testing.T) {
			setSourcePolicy(t, mode, true)
			peer := NewId()
			client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 4)
			delivered := deliverContent(client)

			route <- forgeBarrierFrame(t, ControlId, client.ClientId(), "platform")
			route <- forgeBarrierFrame(t, client.ClientId(), client.ClientId(), "self")

			waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
				return stats.ForgedSourceDropCount == 2
			}, "two forged drops")
			if stats := client.ReceiveStats(); stats.ForgedSourcePlatformDropCount != 1 ||
				stats.ForgedSourceSelfDropCount != 1 {
				t.Fatalf("unexpected kinds: %+v", stats)
			}
			expectNoDelivery(t, delivered)
		})
	}
}

// The kill switch turns off the enforced half only. A platform-named frame is
// then delivered, and the measured half keeps counting.
func TestP2PForgedSourceDropKillSwitch(t *testing.T) {
	setSourcePolicy(t, "count", false)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 4)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	route <- forgeBarrierFrame(t, ControlId, client.ClientId(), "platform")
	route <- forgeBarrierFrame(t, third, client.ClientId(), "relayed")

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case got := <-delivered:
			seen[got] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("delivered: %v", seen)
		}
	}
	if !seen["platform"] || !seen["relayed"] {
		t.Fatalf("delivered: %v", seen)
	}
	// both frames name a source other than the bound peer, so the measured
	// half counts both, independent of the enforced switch
	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchDecodedCount == 2
	}, "the measured half still counts")
	if stats := client.ReceiveStats(); stats.ForgedSourceDropCount != 0 {
		t.Fatalf("drops counted with the switch off: %+v", stats)
	}
}

// A relayed transport has no bound peer and carries any source, so neither
// half applies and nothing is counted.
func TestP2PRelayedTransportHasNoBinding(t *testing.T) {
	setSourcePolicy(t, "drop", true)
	client, route := newSourcePolicyClient(t, TransportTypeH1, Id{}, 2)
	delivered := deliverContent(client)
	third := NewId()
	client.ContractManager().AddNoContractPeer(third)

	route <- forgeBarrierFrame(t, third, client.ClientId(), "relayed")

	expectDelivery(t, delivered, "relayed")
	if stats := client.ReceiveStats(); stats.SourceMismatchCount != 0 || stats.ForgedSourceDropCount != 0 {
		t.Fatalf("relayed transport counted: %+v", stats)
	}
}
