package main

import (
	"fmt"
	"time"

	"github.com/urnetwork/connect/gossip"
)

// The gossip health states reported in the status and the log: waiting,
// isolated, bootstrapping, degraded, healthy and capped. An empty state means
// gossip has not started, so there is nothing to report.
const (
	gossipHealthWaiting       = "waiting"
	gossipHealthIsolated      = "isolated"
	gossipHealthBootstrapping = "bootstrapping"
	gossipHealthDegraded      = "degraded"
	gossipHealthHealthy       = "healthy"
	gossipHealthCapped        = "capped"
)

// gossipHealthFloor is the least number of mesh peers that counts as a working
// mesh. A node below the floor still exchanges records, so it is degraded
// rather than isolated.
const gossipHealthFloor = 2

// gossipHealthCapHold is how long a cap hit keeps the node reported capped,
// without new hits arriving.
const gossipHealthCapHold = 10 * time.Minute

// gossipHealthRank orders the states from most to least reachable. waiting is
// the state before gossip starts and ranks below everything running.
func gossipHealthRank(state string) int {
	switch state {
	case gossipHealthHealthy:
		return 4
	case gossipHealthCapped:
		return 3
	case gossipHealthDegraded:
		return 2
	case gossipHealthBootstrapping:
		return 1
	case gossipHealthIsolated:
		return 0
	case gossipHealthWaiting:
		return -1
	}
	return -1
}

// gossipHealthWorsened reports that a running node moved into a less reachable
// state. Starting up from waiting into bootstrapping is progress, not a
// worsening.
func gossipHealthWorsened(prev, next string) bool {
	if prev == "" || prev == gossipHealthWaiting {
		return false
	}
	return gossipHealthRank(next) < gossipHealthRank(prev)
}

// dominantDialFailure names the failure class with the largest count, so the
// reason answers why the dials fail rather than only that they do.
func dominantDialFailure(stats *gossip.DialStats) (string, uint64) {
	if stats == nil {
		return "", 0
	}
	class := "timeout"
	count := stats.Timeout
	for _, candidate := range []struct {
		name string
		n    uint64
	}{
		{"refused", stats.Refused},
		{"canceled", stats.Canceled},
		{"other", stats.Other},
	} {
		if candidate.n > count {
			class, count = candidate.name, candidate.n
		}
	}
	return class, count
}

// gossipHealthFromSnapshot derives the health state and its reason from one
// discovery snapshot. An empty state means gossip has not started yet.
func gossipHealthFromSnapshot(d extenderDiscoverySnapshot) (state string, reason string) {
	switch d.State {
	case extenderDiscoveryStateOff:
		return "", ""
	case extenderDiscoveryStateWaiting:
		return gossipHealthWaiting, "the platform directory or the extender identity is not ready"
	case extenderDiscoveryStateFailed:
		return gossipHealthWaiting, "discovery failed: " + d.Failure
	case extenderDiscoveryStateFeedOnly:
		return gossipHealthWaiting, "the role runs the feed only"
	}
	if d.Peers <= 0 {
		if d.DialStats == nil || d.DialStats.Attempts == 0 {
			return gossipHealthIsolated, "no bootstrap peers known"
		}
		if d.Connecting != nil && *d.Connecting {
			return gossipHealthBootstrapping, "dialing bootstrap peers"
		}
		class, count := dominantDialFailure(d.DialStats)
		return gossipHealthIsolated, fmt.Sprintf("all dials failing (%s=%d)", class, count)
	}
	if d.Peers < gossipHealthFloor {
		return gossipHealthDegraded, fmt.Sprintf("%d mesh peer(s), below the floor of %d", d.Peers, gossipHealthFloor)
	}
	return gossipHealthHealthy, ""
}

// gossipHealthCappedReason reports the cap state when the interval holds any cap
// hit. Cap hits name the first reason seen, so the reason stays useful without
// counting every hit.
func gossipHealthCappedReason(events []gossip.Event, since time.Time, now time.Time) (string, bool) {
	hits := 0
	firstReason := ""
	for _, ev := range events {
		if ev.Kind != "cap" {
			continue
		}
		if !since.IsZero() && !ev.Time.After(since) {
			continue
		}
		if ev.Time.After(now) {
			continue
		}
		hits++
		if firstReason == "" {
			firstReason = ev.Reason
		}
	}
	if hits == 0 {
		return "", false
	}
	if firstReason == "" {
		firstReason = "cap reached"
	}
	return fmt.Sprintf("%d cap hit(s) since the last check (%s)", hits, firstReason), true
}
