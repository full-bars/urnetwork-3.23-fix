package connect

import (
	"context"
	"testing"
	"time"
)

func TestReceiveRouteDisposition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rm := NewRouteManager(ctx, "test")
	route := make(Route, 1)
	peer := NewId()
	transport := NewReceiveGatewayTransportWithType(TransportTypeP2p)
	rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: peer})
	reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)
	defer rm.CloseMultiRouteReader(reader)
	route <- []byte("frame")
	data, disposition, err := reader.readWithDisposition(ctx, time.Second)
	if err != nil || string(data) != "frame" || disposition.transportType != TransportTypeP2p || disposition.boundSourceId != peer {
		t.Fatalf("unexpected disposition: %q %+v %v", data, disposition, err)
	}
}

func TestReceiveRouteDispositionRebind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rm := NewRouteManager(ctx, "test")
	route := make(Route, 1)
	transport := NewReceiveGatewayTransportWithType(TransportTypeP2p)
	first, second := NewId(), NewId()
	rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: first})
	reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)
	defer rm.CloseMultiRouteReader(reader)
	for _, peer := range []Id{first, second} {
		rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: peer})
		route <- []byte("frame")
		_, disposition, err := reader.readWithDisposition(ctx, time.Second)
		if err != nil || disposition.boundSourceId != peer {
			t.Fatalf("binding = %v, want %v: %v", disposition.boundSourceId, peer, err)
		}
	}
}

// A transport that is removed while a frame is in flight must not change how
// that frame is judged. The lane's type and binding are taken with the route
// list, before the frame is read, so a peer that closes straight after a frame
// still delivers it with the identity it arrived on. Resolving the disposition
// after the read lost it, because removal deletes the lane and the zero value
// is a lane every source policy accepts.
func TestReceiveRouteDispositionSurvivesRemovalInFlight(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := NewId()
	for i := 0; i < 200; i++ {
		rm := NewRouteManager(ctx, "inflight")
		// unbuffered, the way the p2p receive transport hands frames over
		route := make(Route)
		transport := NewReceiveGatewayTransportWithType(TransportTypeP2p)
		rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: peer})
		reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)

		type readResult struct {
			data        []byte
			disposition transferReceiveDisposition
			err         error
		}
		done := make(chan readResult, 1)
		go func() {
			data, disposition, err := reader.readWithDisposition(ctx, time.Second)
			done <- readResult{data, disposition, err}
		}()

		// send a frame, then close the lane the way the p2p receive transport
		// does when the peer disconnects
		route <- []byte("frame")
		rm.RemoveTransport(transport)

		result := <-done
		if result.err != nil || string(result.data) != "frame" {
			t.Fatalf("iteration %d: read = %q, %v", i, result.data, result.err)
		}
		if result.disposition.transportType != TransportTypeP2p || result.disposition.boundSourceId != peer {
			t.Fatalf("iteration %d: frame from a lane removed in flight was judged as %+v, want p2p bound to the peer", i, result.disposition)
		}

		reader.mutex.Lock()
		_, statsRestored := reader.routeStats[route]
		reader.mutex.Unlock()
		if statsRestored {
			t.Fatalf("iteration %d: stats were recreated for a removed route", i)
		}
		rm.CloseMultiRouteReader(reader)
	}
}

// The route list and each route's disposition are one snapshot. A removal that
// lands after the snapshot cannot take the lane away from a frame read from it,
// and cannot take the lane away from the snapshot itself.
func TestReceiveRouteSnapshotHoldsDispositionAcrossRemoval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rm := NewRouteManager(ctx, "snapshot")
	route := make(Route, 2)
	peer := NewId()
	transport := NewReceiveGatewayTransportWithType(TransportTypeP2p)
	rm.UpdateTransportWithProperties(transport, []Route{route}, TransferCarrierProperties{boundSourceId: peer})
	reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)
	defer rm.CloseMultiRouteReader(reader)

	// the frame is already queued when the lane goes away
	route <- []byte("frame")
	active := reader.activeReceiveRoutes()
	rm.RemoveTransport(transport)

	if len(active) != 1 || active[0].route != route {
		t.Fatalf("snapshot = %+v, want the one active route", active)
	}
	if want := (transferReceiveDisposition{transportType: TransportTypeP2p, boundSourceId: peer}); active[0].disposition != want {
		t.Fatalf("snapshot disposition = %+v, want %+v", active[0].disposition, want)
	}
}
