package main

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/gossip"
)

var connectingTrue = true
var connectingFalse = false

func TestGossipHealthFromSnapshotTable(t *testing.T) {
	cases := []struct {
		name   string
		d      extenderDiscoverySnapshot
		state  string
		substr string
	}{
		{`off`, extenderDiscoverySnapshot{State: extenderDiscoveryStateOff}, "", ""},
		{`waiting`, extenderDiscoverySnapshot{State: extenderDiscoveryStateWaiting}, gossipHealthWaiting, "platform directory"},
		{`failed`, extenderDiscoverySnapshot{State: extenderDiscoveryStateFailed, Failure: "network"}, gossipHealthWaiting, "discovery failed: network"},
		{`feed-only`, extenderDiscoverySnapshot{State: extenderDiscoveryStateFeedOnly}, gossipHealthWaiting, "feed only"},
		{`running no dials`, extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 0}, gossipHealthIsolated, "no bootstrap peers known"},
		{`running dialing`, extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 0, Connecting: &connectingTrue, DialStats: &gossip.DialStats{Attempts: 4}}, gossipHealthBootstrapping, "dialing"},
		{`running dials failing`, extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 0, DialStats: &gossip.DialStats{Attempts: 4, Timeout: 3, Refused: 1}}, gossipHealthIsolated, "all dials failing (timeout=3)"},
		{`running one peer`, extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 1}, gossipHealthDegraded, "below the floor"},
		{`running healthy`, extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 2}, gossipHealthHealthy, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.d
			gotState, gotReason := gossipHealthFromSnapshot(d)
			if gotState != tc.state {
				t.Fatalf("state = %q, want %q", gotState, tc.state)
			}
			if !strings.Contains(gotReason, tc.substr) {
				t.Fatalf("reason = %q, want substring %q", gotReason, tc.substr)
			}
		})
	}
}

func TestDominantDialFailureTable(t *testing.T) {
	cases := []struct {
		stats *gossip.DialStats
		class string
		count uint64
	}{
		{nil, "", 0},
		{&gossip.DialStats{Attempts: 10, Timeout: 2, Refused: 7, Canceled: 1}, "refused", 7},
		{&gossip.DialStats{Attempts: 10, Timeout: 2, Refused: 0, Canceled: 0, Other: 9}, "other", 9},
		{&gossip.DialStats{Attempts: 10, Timeout: 2, Refused: 1, Canceled: 8}, "canceled", 8},
	}
	for _, tc := range cases {
		stats := tc.stats
		cls, cnt := dominantDialFailure(stats)
		if cls != tc.class || cnt != tc.count {
			t.Fatalf("dominantDialFailure(%+v) = (%s,%d), want (%s,%d)", stats, cls, cnt, tc.class, tc.count)
		}
	}
}

func TestGossipHealthRankIsOrdered(t *testing.T) {
	best := gossipHealthRank(gossipHealthHealthy)
	for _, s := range []string{gossipHealthCapped, gossipHealthDegraded, gossipHealthBootstrapping, gossipHealthIsolated, gossipHealthWaiting} {
		if gossipHealthRank(s) >= best {
			t.Fatalf("gossipHealthRank(%s)=%d not below healthy=%d", s, gossipHealthRank(s), best)
		}
	}
}

func TestGossipHealthWorsened(t *testing.T) {
	if gossipHealthWorsened("", gossipHealthIsolated) {
		t.Fatalf("transition from empty state is not a worsening")
	}
	if gossipHealthWorsened(gossipHealthWaiting, gossipHealthBootstrapping) {
		t.Fatalf("starting up from waiting is not a worsening")
	}
	if !gossipHealthWorsened(gossipHealthHealthy, gossipHealthDegraded) {
		t.Fatalf("healthy to degraded is a worsening")
	}
	if !gossipHealthWorsened(gossipHealthDegraded, gossipHealthIsolated) {
		t.Fatalf("degraded to isolated is a worsening")
	}
	if gossipHealthWorsened(gossipHealthIsolated, gossipHealthHealthy) {
		t.Fatalf("recovery is not a worsening")
	}
}

func TestGossipHealthTransitionLinesAndRingEvents(t *testing.T) {
	base := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	var s discoveryLogState
	recorded := make([]gossip.Event, 0)
	s.record = func(ev gossip.Event) bool {
		recorded = append(recorded, ev)
		return true
	}
	waiting := extenderDiscoverySnapshot{State: extenderDiscoveryStateWaiting}
	if lines := strings.Join(s.linesWithEvents(waiting, nil, base), ""); lines == "" {
		t.Fatalf("expected waiting line, got empty")
	}
	if got := strings.Join(s.linesWithEvents(waiting, nil, base.Add(time.Second)), ""); got != "" {
		t.Fatalf("repeated waiting logged: %q", got)
	}
	running := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 0, DialStats: &gossip.DialStats{Attempts: 2, Timeout: 1}}
	if lines := strings.Join(s.linesWithEvents(running, nil, base.Add(2*time.Second)), ""); !strings.Contains(lines, "gossip health is isolated") {
		t.Fatalf("missing isolated line: %q", lines)
	}
	healthy := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 3}
	if lines := strings.Join(s.linesWithEvents(healthy, nil, base.Add(3*time.Second)), ""); !strings.Contains(lines, "gossip health is healthy") {
		t.Fatalf("missing healthy line: %q", lines)
	}
	if got := strings.Join(s.linesWithEvents(healthy, nil, base.Add(4*time.Second)), ""); got != "" {
		t.Fatalf("repeated healthy logged: %q", got)
	}
	wantReasons := []string{gossipHealthIsolated, gossipHealthHealthy}
	if len(recorded) != len(wantReasons) {
		t.Fatalf("recorded %d events, want %d: %+v", len(recorded), len(wantReasons), recorded)
	}
	for i, want := range wantReasons {
		if recorded[i].Kind != "state" || recorded[i].Reason != want {
			t.Fatalf("recorded event = %+v, want kind=state reason=%s", recorded[i], want)
		}
	}
}

func TestGossipHealthWorseningTriggersAnomalyDump(t *testing.T) {
	base := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	var s discoveryLogState
	events := []gossip.Event{{Time: base, Kind: "peer", Peer: "peer1", Direction: "out", Reason: "connect"}}
	healthy := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 3}
	_ = s.linesWithEvents(healthy, events, base)
	running := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 0, DialStats: &gossip.DialStats{Attempts: 1, Timeout: 1}}
	dump := strings.Join(s.linesWithEvents(running, events, base.Add(time.Second)), "")
	if !strings.Contains(dump, "gossip anomaly") || !strings.Contains(dump, "gossip health isolated") {
		t.Fatalf("expected anomaly dump with health reason: %q", dump)
	}
}

func TestGossipHealthWorseningCooldownSuppressesRepeat(t *testing.T) {
	base := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	var s discoveryLogState
	healthy := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 3}
	peerEvents := []gossip.Event{{Time: base, Kind: "peer", Peer: "peer1", Direction: "out", Reason: "connect"}}
	_ = s.linesWithEvents(healthy, peerEvents, base)
	degraded := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 1}
	dump := strings.Join(s.linesWithEvents(degraded, peerEvents, base.Add(time.Minute)), "")
	if !strings.Contains(dump, "gossip health degraded") {
		t.Fatalf("expected worsening dump with health reason: %q", dump)
	}
	running := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 0, DialStats: &gossip.DialStats{Attempts: 1, Timeout: 1}}
	peerEvents = append(peerEvents, gossip.Event{Time: base.Add(2 * time.Minute), Kind: "dial"})
	sup := strings.Join(s.linesWithEvents(running, peerEvents, base.Add(2*time.Minute)), "")
	if strings.Contains(sup, "gossip anomaly") {
		t.Fatalf("worsening inside cooldown dumped: %q", sup)
	}
}

func TestGossipHealthRejectSpikeCooldown(t *testing.T) {
	base := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	var s discoveryLogState
	rejects := func(n int, at time.Time) []gossip.Event {
		var evs []gossip.Event
		for i := 0; i < n; i++ {
			evs = append(evs, gossip.Event{Time: at, Kind: "reject", Peer: "bad", Direction: "in", Reason: "signature", Size: 64})
		}
		return evs
	}
	healthy := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 3}
	peerEvents := []gossip.Event{{Time: base, Kind: "peer", Peer: "peer1", Direction: "out", Reason: "connect"}}
	_ = s.linesWithEvents(healthy, peerEvents, base)
	degraded := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 1}
	spike := rejects(5, base.Add(time.Minute))
	dump := strings.Join(s.linesWithEvents(degraded, spike, base.Add(time.Minute)), "")
	if !strings.Contains(dump, "gossip anomaly") || !strings.Contains(dump, "reject spike") {
		t.Fatalf("expected reject spike dump: %q", dump)
	}
	dump = strings.Join(s.linesWithEvents(degraded, rejects(5, base.Add(2*time.Minute)), base.Add(2*time.Minute)), "")
	if strings.Contains(dump, "gossip anomaly") {
		t.Fatalf("reject spike inside cooldown dumped: %q", dump)
	}
	dump = strings.Join(s.linesWithEvents(degraded, rejects(5, base.Add(32*time.Minute)), base.Add(32*time.Minute)), "")
	if !strings.Contains(dump, "reject spike") {
		t.Fatalf("expected reject spike after cooldown: %q", dump)
	}
}

func TestRejectFloodStaysBounded(t *testing.T) {
	base := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	var s discoveryLogState
	d := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 3}
	flood := make([]gossip.Event, 0, 10000)
	for i := 0; i < 10000; i++ {
		flood = append(flood, gossip.Event{Time: base, Kind: "reject", Peer: "flood", Direction: "in", Reason: "signature", Size: 64})
	}
	dump := strings.Join(s.linesWithEvents(d, flood, base), "")
	if !strings.Contains(dump, "reject spike") {
		t.Fatalf("expected a reject spike dump: %q", dump[len(dump)-200:])
	}
	if count := strings.Count(dump, "kind=reject"); count > anomalyDumpMaxEvents {
		t.Fatalf("dump shows %d reject events, want at most %d", count, anomalyDumpMaxEvents)
	}
}
