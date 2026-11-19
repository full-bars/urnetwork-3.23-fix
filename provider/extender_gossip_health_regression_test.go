package main

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/gossip"
)

func TestGossipHealthStopClearsState(t *testing.T) {
	now := time.Unix(1000, 0)
	s := discoveryLogState{health: gossipHealthCapped, healthSince: now, capHoldUntil: now.Add(time.Hour), isolationPending: true}
	s.linesWithEvents(extenderDiscoverySnapshot{State: extenderDiscoveryStateOff}, nil, now)
	if s.health != "" || !s.healthSince.IsZero() || !s.capHoldUntil.IsZero() || s.isolationPending {
		t.Fatalf("stale health after stop: %+v", s)
	}
	lines := strings.Join(s.linesWithEvents(extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 2}, nil, now.Add(time.Second)), "")
	if !strings.Contains(lines, "health is healthy") {
		t.Fatalf("new state missing: %s", lines)
	}
}

func TestGossipHealthRetryGapAndZeroFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	active := true
	d := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Connecting: &active, DialStats: &gossip.DialStats{Attempts: 1}}
	var s discoveryLogState
	s.linesWithEvents(d, nil, now)
	active = false
	state, reason := gossipHealthFromSnapshot(d)
	if state != gossipHealthIsolated || reason != "no dial failures recorded" {
		t.Fatalf("zero failures: %s %s", state, reason)
	}
	s.linesWithEvents(d, nil, now.Add(time.Second))
	if s.health != gossipHealthBootstrapping {
		t.Fatalf("retry gap changed health: %s", s.health)
	}
	s.linesWithEvents(d, nil, now.Add(2*time.Second))
	if s.health != gossipHealthIsolated {
		t.Fatalf("persistent isolation was not reported: %s", s.health)
	}
}
