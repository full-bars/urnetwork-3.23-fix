package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/gossip"
)

func TestDiscoveryLogsDescribeProgressWithoutSpam(t *testing.T) {
	var s discoveryLogState
	now := time.Unix(1000, 0)
	check := func(d extenderDiscoverySnapshot, at time.Time, want string) {
		t.Helper()
		got := strings.Join(s.lines(d, at), "")
		if !strings.Contains(got, want) {
			t.Fatalf("log %q, want %q", got, want)
		}
	}
	d := extenderDiscoverySnapshot{State: extenderDiscoveryStateWaiting}
	check(d, now, "waiting for the platform")
	if got := s.lines(d, now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("repeated waiting: %v", got)
	}
	d.State = extenderDiscoveryStateRunning
	var messages, errors uint64
	d.MessagesAccepted, d.ApplyErrors = &messages, &errors
	check(d, now.Add(time.Second), "no mesh peers yet")
	d.Peers = 3
	check(d, now.Add(2*time.Second), "3 mesh peer")
	messages = 1
	check(d, now.Add(3*time.Second), "received 1 valid message")
	if got := s.lines(d, now.Add(4*time.Second)); len(got) != 0 {
		t.Fatalf("repeated success: %v", got)
	}
	errors = 2
	check(d, now.Add(5*time.Second), "application errors: 2")
	d.Peers = 0
	check(d, now.Add(6*time.Second), "lost all mesh peers")
	check(d, now.Add(11*time.Minute), "1 valid message")
}

func TestDiscoveryAnomalyDumpCapsAt50AndRateLimitsTo30Minutes(t *testing.T) {
	var s discoveryLogState
	base := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)

	events := make([]gossip.Event, 60)
	for i := range events {
		events[i] = gossip.Event{
			Time:      base.Add(time.Duration(i) * time.Second),
			Kind:      "peer",
			Peer:      fmt.Sprintf("peer%02d", i),
			Direction: "out",
			Reason:    "connect",
		}
	}

	d := extenderDiscoverySnapshot{State: extenderDiscoveryStateRunning, Peers: 2}
	t1 := base.Add(time.Minute)
	_ = s.linesWithEvents(d, events, t1)

	// Losing all mesh peers triggers an anomaly dump with the last 50 events.
	d.Peers = 0
	t2 := t1.Add(10 * time.Second)
	got := strings.Join(s.linesWithEvents(d, events, t2), "")
	if !strings.Contains(got, "gossip anomaly (all mesh peers lost): last 50 event(s):") {
		t.Fatalf("missing anomaly dump on peer loss:\n%s", got)
	}
	if strings.Contains(got, "peer=peer09") || !strings.Contains(got, "peer=peer10") || !strings.Contains(got, "peer=peer59") {
		t.Fatalf("expected events 10..59 only:\n%s", got)
	}

	// Within the 30-minute cooldown, another anomaly (peer loss or reject spike) does not dump again.
	d.Peers = 2
	_ = s.linesWithEvents(d, events, t2.Add(time.Minute))
	d.Peers = 0
	gotSuppressed := strings.Join(s.linesWithEvents(d, events, t2.Add(5*time.Minute)), "")
	if strings.Contains(gotSuppressed, "gossip anomaly") {
		t.Fatalf("anomaly dump fired inside 30m rate limit:\n%s", gotSuppressed)
	}

	// After 30 minutes, a reject spike triggers a fresh anomaly dump.
	d.Peers = 2
	t3 := t2.Add(30 * time.Minute)
	_ = s.linesWithEvents(d, events, t3)
	t4 := t3.Add(time.Minute)
	for i := 0; i < anomalyRejectEventThreshold; i++ {
		events = append(events, gossip.Event{
			Time:      t3.Add(time.Duration(i+1) * time.Second),
			Kind:      "reject",
			Peer:      "badpeer1",
			Direction: "in",
			Reason:    "signature",
			Size:      128,
		})
	}
	gotSpike := strings.Join(s.linesWithEvents(d, events, t4), "")
	if !strings.Contains(gotSpike, "gossip anomaly (reject spike (5 sampled rejects)): last 50 event(s):") {
		t.Fatalf("missing anomaly dump on reject spike after cooldown:\n%s", gotSpike)
	}
}
