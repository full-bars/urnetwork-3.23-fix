package connect

import (
	"context"
	"net"
	"testing"
	"time"
)

// The production receive transport reports its own type and its lane carries
// the binding the adjacent peer's signalling authenticated. A frame read
// through it is judged as a peer-to-peer lane, which is what makes the source
// rules apply outside the test gateway transport.
func TestP2pReceiveTransportReportsItsLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handleCtx, handleCancel := context.WithCancel(ctx)
	local, remote := net.Pipe()
	peer := NewId()
	transport, route := NewP2pReceiveTransport(handleCtx, handleCancel, local, NewId(), DefaultP2pTransportSettings())

	typed, ok := transport.(interface{ TransportType() TransportType })
	if !ok {
		t.Fatalf("the p2p receive transport has no TransportType, so its lane reads as unknown")
	}
	if got := typed.TransportType(); got != TransportTypeP2p {
		t.Fatalf("transport type = %q, want %q", got, TransportTypeP2p)
	}

	// registered the way the p2p transport publishes a connected lane
	rm := NewRouteManager(ctx, "p2p-receive")
	rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: peer})
	reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)
	defer rm.CloseMultiRouteReader(reader)

	frame := forgeBarrierFrame(t, ControlId, NewId(), "forged")
	go func() {
		_, _ = remote.Write(frame)
	}()
	data, disposition, err := reader.readWithDisposition(ctx, 5*time.Second)
	if err != nil || len(data) != len(frame) {
		t.Fatalf("read = %d bytes, %v", len(data), err)
	}
	if disposition.transportType != TransportTypeP2p || disposition.boundSourceId != peer {
		t.Fatalf("lane = %+v, want p2p bound to the peer", disposition)
	}
	// the frame is judged by the enforced rule, which needs the lane type
	if kind := forgedSourceKind(disposition, NewId(), ControlId); kind != "platform" {
		t.Fatalf("forgedSourceKind = %q, want platform", kind)
	}
}

// The p2p receive transport hands frames over one at a time and closes its
// route when the connection ends, so the frame that arrives with the close is
// still read with the lane it arrived on.
func TestP2pReceiveTransportFrameAtCloseKeepsItsLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handleCtx, handleCancel := context.WithCancel(ctx)
	local, remote := net.Pipe()
	peer := NewId()
	transport, route := NewP2pReceiveTransport(handleCtx, handleCancel, local, NewId(), DefaultP2pTransportSettings())
	rm := NewRouteManager(ctx, "p2p-close")
	rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: peer})
	reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)
	defer rm.CloseMultiRouteReader(reader)

	frame := forgeBarrierFrame(t, ControlId, NewId(), "last")
	go func() {
		_, _ = remote.Write(frame)
		_ = remote.Close()
	}()
	data, disposition, err := reader.readWithDisposition(ctx, 5*time.Second)
	if err != nil || len(data) != len(frame) {
		t.Fatalf("read = %d bytes, %v", len(data), err)
	}
	if disposition.transportType != TransportTypeP2p || disposition.boundSourceId != peer {
		t.Fatalf("lane = %+v, want p2p bound to the peer", disposition)
	}
}
