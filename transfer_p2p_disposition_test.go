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
