package connect

import (
	"context"
	"testing"
)

// matchAllTransport matches every destination, which is all this regression
// needs: the panic is in bookkeeping, not in routing policy.
type matchAllTransport struct {
	transportId Id
}

func newMatchAllTransport() *matchAllTransport {
	return &matchAllTransport{transportId: NewId()}
}

func (self *matchAllTransport) TransportId() Id { return self.transportId }
func (self *matchAllTransport) Priority() int   { return 0 }
func (self *matchAllTransport) Weight() float32 { return 0 }
func (self *matchAllTransport) CanEvalRouteWeight(
	stats *RouteStats,
	remainingStats map[Transport]*RouteStats,
) bool {
	return true
}

func (self *matchAllTransport) RouteWeight(
	stats *RouteStats,
	remainingStats map[Transport]*RouteStats,
) float32 {
	return 1.0
}
func (self *matchAllTransport) MatchesSend(destination TransferPath) bool    { return true }
func (self *matchAllTransport) MatchesReceive(destination TransferPath) bool { return true }
func (self *matchAllTransport) Downgrade(source TransferPath)                {}

// Closing the last selector for a destination removes the transport's matched
// destination set once it empties. Opening a selector afterwards takes the
// cache-miss branch in openMultiRouteSelector, which used to leave the local
// map nil and panic with "assignment to entry in nil map".
func TestOpenMultiRouteWriterAfterCloseDoesNotPanicOnMatchedDestinations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routeManager := NewRouteManager(ctx, "nilmap-test")
	transport := newMatchAllTransport()
	routes := []Route{make(chan []byte)}
	routeManager.UpdateTransport(transport, routes)

	destination := DestinationId(NewId())

	// First open populates the transport's matched destination set.
	writer := routeManager.OpenMultiRouteWriter(destination)
	if writer == nil {
		t.Fatal("expected a multi route writer")
	}
	// Closing the only selector for this destination empties, and therefore
	// removes, the transport's entry.
	routeManager.CloseMultiRouteWriter(writer)

	// Second open hits the now-missing entry. Before the fix this panicked.
	reopened := routeManager.OpenMultiRouteWriter(destination)
	if reopened == nil {
		t.Fatal("expected a multi route writer after reopening the destination")
	}
	routeManager.CloseMultiRouteWriter(reopened)
}

// The same path through the reader side, which shares MatchState.
func TestOpenMultiRouteReaderAfterCloseDoesNotPanicOnMatchedDestinations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	routeManager := NewRouteManager(ctx, "nilmap-test-reader")
	transport := newMatchAllTransport()
	routeManager.UpdateTransport(transport, []Route{make(chan []byte)})

	// OpenMultiRouteReader requires a destination mask, not a source.
	destination := DestinationId(NewId())

	reader := routeManager.OpenMultiRouteReader(destination)
	if reader == nil {
		t.Fatal("expected a multi route reader")
	}
	routeManager.CloseMultiRouteReader(reader)

	reopened := routeManager.OpenMultiRouteReader(destination)
	if reopened == nil {
		t.Fatal("expected a multi route reader after reopening the source")
	}
	routeManager.CloseMultiRouteReader(reopened)
}
