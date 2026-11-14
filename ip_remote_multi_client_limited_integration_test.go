package connect

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type limitedIntegrationGenerator struct {
	testingEmptyMultiClientGenerator
	legacy atomic.Bool
}

func (g *limitedIntegrationGenerator) SelectorMode() (bool, bool) { return !g.legacy.Load(), true }
func (g *limitedIntegrationGenerator) NextDestinationsWithCapabilities(_ int, _ []MultiHopId, _ string, _ IpFamilyFilter, _ FindProvidersSelect) (map[MultiHopId]DestinationStats, bool, error) {
	return nil, !g.legacy.Load(), nil
}

func TestLimitedWindowConstructorAndResize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultMultiClientSettings()
	settings.LimitedWindowSizeFraction = .5
	settings.WindowResizeTimeout = 10 * time.Millisecond
	settings.WindowExpandTimeout = 10 * time.Millisecond
	generator := &limitedIntegrationGenerator{}
	client := NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Network, settings)
	defer client.Close()
	limited := client.windows[WindowTypeLimited]
	if limited == nil || limited.limitedHooks.Load() == nil {
		t.Fatal("limited window or hooks missing")
	}
	deadline := time.Now().Add(3 * time.Second)
	for client.LimitedWindowTargetSize() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if client.LimitedWindowTargetSize() == 0 {
		t.Fatal("limited window never sized from siblings")
	}
	generator.legacy.Store(true)
	limited.resizeMonitor.NotifyAll()
	deadline = time.Now().Add(3 * time.Second)
	for client.LimitedWindowTargetSize() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if client.LimitedWindowTargetSize() != 0 {
		t.Fatal("legacy selector did not disable limited window")
	}
}
