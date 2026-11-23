package connect

// A transport owns its client strategy only when the settings say so. Most
// transports share one strategy with the caller (the smart dialer probes, a
// family group's other transports), so Close must leave a shared strategy
// alone.

import (
	"context"
	"testing"
)

func newOwnershipTestStrategy(t *testing.T) *ClientStrategy {
	t.Helper()
	strategy := NewClientStrategy(context.Background(), DefaultClientStrategySettings())
	t.Cleanup(strategy.Close)
	return strategy
}

// A shared strategy survives the transport that used it.
func TestPlatformTransportCloseLeavesASharedStrategyOpen(t *testing.T) {
	strategy := newOwnershipTestStrategy(t)
	transport := &PlatformTransport{
		settings:       &PlatformTransportSettings{},
		clientStrategy: strategy,
		cancel:         func() {},
	}
	transport.Close()
	if strategy.Closed() {
		t.Fatal("a transport without the ownership flag closed a shared strategy")
	}
}

// An owned strategy is released by the transport's Close.
func TestPlatformTransportCloseClosesAnOwnedStrategy(t *testing.T) {
	strategy := newOwnershipTestStrategy(t)
	transport := &PlatformTransport{
		settings:       &PlatformTransportSettings{OwnClientStrategy: true},
		clientStrategy: strategy,
		cancel:         func() {},
	}
	transport.Close()
	if !strategy.Closed() {
		t.Fatal("an owned strategy was not closed")
	}
}

// The ownership flag does nothing when the transport was given no strategy.
func TestPlatformTransportCloseWithoutAStrategy(t *testing.T) {
	transport := &PlatformTransport{
		settings: &PlatformTransportSettings{OwnClientStrategy: true},
		cancel:   func() {},
	}
	transport.Close()
}

// Closing a strategy twice is safe and reports closed after the first call.
func TestClientStrategyCloseIsSafeTwice(t *testing.T) {
	strategy := newOwnershipTestStrategy(t)
	if strategy.Closed() {
		t.Fatal("a fresh strategy reports closed")
	}
	strategy.Close()
	strategy.Close()
	if !strategy.Closed() {
		t.Fatal("Close did not record the closed state")
	}
}
