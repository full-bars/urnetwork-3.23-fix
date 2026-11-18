package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect/gossip"
)

const (
	anomalyDumpMaxEvents        = 50
	anomalyDumpCooldown         = 30 * time.Minute
	anomalyRejectEventThreshold = 5
)

// discoveryLogState emits transitions and a bounded periodic summary. Counts
// restart with the local gossip role, and include duplicate valid messages.
type discoveryLogState struct {
	state           string
	hadPeers        bool
	hadMessages     bool
	lastMessages    uint64
	lastErrors      uint64
	lastSummary     time.Time
	lastCheck       time.Time
	lastAnomalyDump time.Time
	health          string
	healthSince     time.Time
	// capHold is the state the node keeps while cap hits keep arriving. A cap
	// that clears is only reported once no further hit has been seen for the
	// hold, so a busy feed does not flap the state on every refused subscriber.
	capHoldUntil time.Time
	record       func(gossip.Event) bool
}

func (s *discoveryLogState) lines(d extenderDiscoverySnapshot, now time.Time) []string {
	return s.linesWithEvents(d, nil, now)
}

func (s *discoveryLogState) linesWithEvents(d extenderDiscoverySnapshot, events []gossip.Event, now time.Time) []string {
	var out []string
	prevState := s.state
	prevCheck := s.lastCheck
	s.lastCheck = now
	entered := d.State != s.state
	var anomalyReasons []string
	if entered {
		if d.State == extenderDiscoveryStateWaiting {
			out = append(out, "🌉 [extender] discovery is waiting for the platform directory, network host and extender identity; gossip has not started yet.\n")
			if prevState == extenderDiscoveryStateRunning {
				anomalyReasons = append(anomalyReasons, "discovery returned to waiting")
			}
		} else if d.State == extenderDiscoveryStateFailed {
			anomalyReasons = append(anomalyReasons, "discovery failed")
		}
		s.state = d.State
		s.hadPeers, s.hadMessages, s.lastMessages, s.lastErrors = false, false, 0, 0
		s.lastSummary = now
	}
	if d.State != extenderDiscoveryStateRunning {
		if len(anomalyReasons) > 0 {
			out = s.appendAnomalyDump(out, strings.Join(anomalyReasons, ", "), events, now)
		}
		return out
	}
	peers := d.Peers > 0
	if peers && !s.hadPeers {
		out = append(out, fmt.Sprintf("🌉✅ [extender] gossip has %d mesh peer(s); receiving valid messages confirms records are arriving.\n", d.Peers))
	} else if !peers && s.hadPeers {
		out = append(out, "🌉⚠️ [extender] gossip lost all mesh peers; it will keep trying to connect.\n")
		anomalyReasons = append(anomalyReasons, "all mesh peers lost")
	} else if !peers && entered {
		out = append(out, "🌉 [extender] gossip started locally but has no mesh peers yet; it will keep trying to connect.\n")
	}
	s.hadPeers = peers
	if d.MessagesAccepted != nil && *d.MessagesAccepted < s.lastMessages {
		s.hadMessages = false
	}
	if d.MessagesAccepted != nil && *d.MessagesAccepted > 0 && !s.hadMessages {
		out = append(out, fmt.Sprintf("🌉✅ [extender] gossip received %d valid message(s) from peers; the local directory accepted them (duplicates included).\n", *d.MessagesAccepted))
		s.hadMessages = true
	}
	if d.MessagesAccepted != nil {
		s.lastMessages = *d.MessagesAccepted
	}
	if d.ApplyErrors != nil && *d.ApplyErrors < s.lastErrors {
		s.lastErrors = 0
	}
	if d.ApplyErrors != nil && *d.ApplyErrors > s.lastErrors {
		out = append(out, fmt.Sprintf("🌉⚠️ [extender] gossip directory application errors: %d since this role started; inspect the gossip logs for details.\n", *d.ApplyErrors))
		s.lastErrors = *d.ApplyErrors
	}
	rejectEvents, worsenedState := countNewAnomalyEvents(events, prevCheck, now)
	if worsenedState != "" {
		anomalyReasons = append(anomalyReasons, "state "+worsenedState)
	}
	if rejectEvents >= anomalyRejectEventThreshold {
		anomalyReasons = append(anomalyReasons, fmt.Sprintf("reject spike (%d sampled rejects)", rejectEvents))
	}
	healthState, healthReason := gossipHealthFromSnapshot(d)
	if capReason, ok := gossipHealthCappedReason(events, prevCheck, now); ok {
		healthState = gossipHealthCapped
		healthReason = capReason
		s.capHoldUntil = now.Add(gossipHealthCapHold)
	} else if now.Before(s.capHoldUntil) {
		healthState = gossipHealthCapped
		healthReason = "cap hits are recent"
	}
	if healthState != "" && healthState != s.health {
		prevHealth := s.health
		s.health = healthState
		s.healthSince = now
		if s.record != nil {
			s.record(gossip.Event{Time: now, Kind: "state", Reason: healthState})
		}
		if healthState == gossipHealthHealthy {
			out = append(out, fmt.Sprintf("🌉✅ [extender] gossip health is healthy: %s\n", healthReason))
		} else {
			out = append(out, fmt.Sprintf("🌉⚠️ [extender] gossip health is %s: %s\n", healthState, healthReason))
			if gossipHealthWorsened(prevHealth, healthState) {
				anomalyReasons = append(anomalyReasons, "gossip health "+healthState)
			}
		}
	}
	if len(anomalyReasons) > 0 {
		out = s.appendAnomalyDump(out, strings.Join(anomalyReasons, ", "), events, now)
	}
	if now.Sub(s.lastSummary) >= 10*time.Minute {
		received := uint64(0)
		if d.MessagesAccepted != nil {
			received = *d.MessagesAccepted
		}
		out = append(out, fmt.Sprintf("🌉 [extender] gossip: %d mesh peer(s), %d feed subscriber(s), %d valid message(s) received from peers since this role started.\n", d.Peers, d.FeedSubscribers, received))
		s.lastSummary = now
	}
	return out
}

func countNewAnomalyEvents(events []gossip.Event, since time.Time, now time.Time) (rejectCount int, worsenedState string) {
	if len(events) == 0 {
		return 0, ""
	}
	cutoff := since
	if cutoff.IsZero() {
		cutoff = now.Add(-10 * time.Minute)
	}
	for _, ev := range events {
		if !since.IsZero() {
			if !ev.Time.After(cutoff) || ev.Time.After(now) {
				continue
			}
		} else if ev.Time.Before(cutoff) || ev.Time.After(now) {
			continue
		}
		switch ev.Kind {
		case "reject":
			rejectCount++
		case "state":
			if ev.Reason == "degraded" || ev.Reason == "isolated" {
				worsenedState = ev.Reason
			}
		}
	}
	return rejectCount, worsenedState
}

func (s *discoveryLogState) appendAnomalyDump(out []string, reason string, events []gossip.Event, now time.Time) []string {
	if len(events) == 0 {
		return out
	}
	if !s.lastAnomalyDump.IsZero() && now.Sub(s.lastAnomalyDump) < anomalyDumpCooldown {
		return out
	}
	s.lastAnomalyDump = now
	tail := events
	if len(tail) > anomalyDumpMaxEvents {
		tail = tail[len(tail)-anomalyDumpMaxEvents:]
	}
	parts := make([]string, 0, len(tail))
	for _, ev := range tail {
		parts = append(parts, formatGossipEventCompact(ev))
	}
	return append(out, fmt.Sprintf("🌉⚠️ [extender] gossip anomaly (%s): last %d event(s): %s\n", reason, len(tail), strings.Join(parts, "; ")))
}

func formatGossipEventCompact(ev gossip.Event) string {
	var b strings.Builder
	b.WriteString(ev.Time.UTC().Format(time.RFC3339))
	b.WriteString(" kind=")
	b.WriteString(ev.Kind)
	if ev.Peer != "" {
		b.WriteString(" peer=")
		b.WriteString(ev.Peer)
	}
	if ev.Direction != "" {
		b.WriteString(" dir=")
		b.WriteString(ev.Direction)
	}
	if ev.Reason != "" {
		b.WriteString(" reason=")
		b.WriteString(ev.Reason)
	}
	if ev.Size > 0 {
		fmt.Fprintf(&b, " size=%d", ev.Size)
	}
	return b.String()
}

func (m *extenderManager) logDiscoveryHealth(now time.Time) {
	role := m.Role()
	if role != m.discoveryLogRole {
		m.discoveryLog = discoveryLogState{}
		m.discoveryLogRole = role
	}
	if role == nil {
		return
	}
	m.discoveryLog.record = role.discoveryRecordEvent
	for _, line := range m.discoveryLog.linesWithEvents(role.discoverySnapshot(), role.discoveryEvents(), now) {
		importantLogf("%s", line)
	}
}
