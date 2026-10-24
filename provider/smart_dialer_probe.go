package main

import (
	"context"
	"math/rand"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

const (
	// smartDialerProbeInterval is how often each proxy's strategy checks
	// whether any transport lacks a current connect-cost measurement. A check
	// with nothing to probe (smart dialer off, or everything measured) does no
	// network work, so this can be short.
	smartDialerProbeInterval = 5 * time.Minute
	// smartDialerProbeMinDelay and smartDialerProbeJitter spread the first
	// probe of a large proxy set across a minute or so after start, so the
	// api is not hit by every proxy at once and a proxy is probed only after
	// it has had time to authenticate.
	smartDialerProbeMinDelay = 20 * time.Second
	smartDialerProbeJitter   = 60 * time.Second
)

// applyPersistedSmartDialer hands the persisted preference to the dialer, once,
// before any identity starts. This is the only writer of the runtime flag from
// control state, and it is why smart_dialer is restart-required rather than
// live (see liveEffectKeys in control_socket.go): the probe loop that feeds a
// measurement is created per identity when it starts, so a `set` on a running
// provider cannot create one and only persists the value for the next start.
//
// Replayed in both directions, including the off default: an unset key must put
// the flag at off rather than leave whatever an earlier replay left behind.
func applyPersistedSmartDialer(state *controlState) {
	on := false
	if v, ok := state.get("smart_dialer"); ok {
		on = strings.EqualFold(strings.TrimSpace(v), "on")
	}
	connect.SetSmartDialer(on)
}

// startSmartDialerProbes measures, in the background, the connect cost of
// every transport the strategy has not been able to measure on its own (the
// first-choice transport always wins where it works, so the others are never
// dialed; see connect.ClientStrategy.ProbeDialers).
//
// A node with the smart dialer off starts nothing at all: no goroutine and no
// timer. The per-round check inside ProbeDialers already made an idle round
// cost no network work, but the loop itself still cost one goroutine and its
// stack per proxy, and on a node with thousands of proxies that is thousands
// of goroutines that can never do anything. The preference is read here, when
// the identity starts, so a node that turns it on at runtime begins probing on
// its next start rather than within one interval.
func startSmartDialerProbes(ctx context.Context, strategy *connect.ClientStrategy, apiUrl string, tag string) {
	if !connect.SmartDialerEnabled() {
		return
	}
	delay := smartDialerProbeMinDelay + time.Duration(rand.Int63n(int64(smartDialerProbeJitter)))
	go connect.HandleError(func() {
		runSmartDialerProbes(ctx, delay, smartDialerProbeInterval, func(ctx context.Context) int {
			return strategy.ProbeDialers(ctx, apiUrl)
		}, func(format string, args ...any) {
			tlog("[smart-dialer]["+tag+"] "+format, args...)
		})
	})
}

// runSmartDialerProbes waits out the initial delay, then runs probe on every
// interval until ctx ends. A round that attempted nothing is silent; a round
// that probed logs how many transports it measured.
func runSmartDialerProbes(ctx context.Context, initialDelay time.Duration, interval time.Duration, probe func(context.Context) int, logf func(string, ...any)) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if probed := probe(ctx); 0 < probed {
			logf("measured connect cost of %d transports\n", probed)
		}
		timer.Reset(interval)
	}
}
