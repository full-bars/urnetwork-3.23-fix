package connect

import (
	"context"
	"testing"
	"time"
)

// BenchmarkReceiveDispositionGuardPass measures one receive read on a lane
// whose frame names the bound peer, the case the source policy accepts. The
// route is kept full outside the timed region so every iteration takes the
// non-blocking path and the number reflects the guard and the disposition
// lookup, not channel waits.
func BenchmarkReceiveDispositionGuardPass(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rm := NewRouteManager(ctx, "bench")
	route := make(Route, 4096)
	peer := NewId()
	rm.UpdateTransportWithProperties(
		NewReceiveGatewayTransportWithType(TransportTypeP2p),
		[]Route{route},
		TransferCarrierProperties{boundSourceId: peer},
	)
	reader := rm.OpenMultiRouteReader(TransferPath{}).(*MultiRouteSelector)
	defer rm.CloseMultiRouteReader(reader)

	frame := []byte("frame")
	fill := func() {
		for len(route) < cap(route) {
			route <- frame
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%cap(route) == 0 {
			b.StopTimer()
			fill()
			b.StartTimer()
		}
		data, disposition, err := reader.readWithDisposition(ctx, time.Second)
		if err != nil {
			b.Fatal(err)
		}
		if disposition.boundSourceId != peer || len(data) != len(frame) {
			b.Fatalf("unexpected read: %d bytes, binding %v", len(data), disposition.boundSourceId)
		}
	}
}

// TestReceiveDispositionGuardNoAlloc pins the allocation count of the source
// policy guard itself. It reads only the values the receive loop already has,
// so an accepted frame costs no allocation.
func TestReceiveDispositionGuardNoAlloc(t *testing.T) {
	local := NewId()
	peer := NewId()
	disposition := transferReceiveDisposition{
		transportType: TransportTypeP2p,
		boundSourceId: peer,
	}
	allocs := testing.AllocsPerRun(1000, func() {
		if kind := forgedSourceKind(disposition, local, peer); kind != "" {
			t.Fatalf("accepted frame reported as %q", kind)
		}
		if boundSourceMismatch(disposition, peer) {
			t.Fatal("accepted frame reported as a mismatch")
		}
	})
	if allocs != 0 {
		t.Fatalf("guard allocations = %v, want 0", allocs)
	}
}
