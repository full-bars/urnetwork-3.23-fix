package main

import (
	"fmt"
	"time"
)

// discoveryLogState emits transitions and a bounded periodic summary. Counts
// restart with the local gossip role, and include duplicate valid messages.
type discoveryLogState struct {
	state        string
	hadPeers     bool
	hadMessages  bool
	lastMessages uint64
	lastErrors   uint64
	lastSummary  time.Time
}

func (s *discoveryLogState) lines(d extenderDiscoverySnapshot, now time.Time) []string {
	var out []string
	entered := d.State != s.state
	if entered {
		if d.State == extenderDiscoveryStateWaiting {
			out = append(out, "🌉 [extender] discovery is waiting for the platform directory, network host and extender identity; gossip has not started yet.\n")
		}
		s.state = d.State
		s.hadPeers, s.hadMessages, s.lastMessages, s.lastErrors = false, false, 0, 0
		s.lastSummary = now
	}
	if d.State != extenderDiscoveryStateRunning {
		return out
	}
	peers := d.Peers > 0
	if peers && !s.hadPeers {
		out = append(out, fmt.Sprintf("🌉✅ [extender] gossip has %d mesh peer(s); receiving valid messages confirms records are arriving.\n", d.Peers))
	} else if !peers && s.hadPeers {
		out = append(out, "🌉⚠️ [extender] gossip lost all mesh peers; it will keep trying to connect.\n")
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

func (m *extenderManager) logDiscoveryHealth(now time.Time) {
	role := m.Role()
	if role == nil {
		m.discoveryLog = discoveryLogState{}
		return
	}
	for _, line := range m.discoveryLog.lines(role.discoverySnapshot(), now) {
		importantLogf("%s", line)
	}
}
