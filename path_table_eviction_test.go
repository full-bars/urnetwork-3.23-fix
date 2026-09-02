package connect

import (
	"testing"
	"time"
)

// TestPathTableEvictionKeepsActiveFlow is the regression test for the
// pathTable LRU eviction design (G-H1). CodeRabbit flagged that eviction at
// the 4096 cap could break an ACTIVE flow by ejecting its route. The design
// answer: lastUsed is refreshed on every SelectDestination hit, so a route
// that is actively carrying traffic is never the least-recently-used entry.
// This test proves evictOldest removes only the oldest (idle) entry while a
// recently-used one survives, and evictStale removes only genuinely-stale
// entries (>5min idle).
func TestPathTableEvictionKeepsActiveFlow(t *testing.T) {
	pt := newPathTable([]MultiHopId{MultiHopId{}})
	now := time.Now()

	// Two IPv4 flows: one "active" (recently used), one "idle" (old).
	activeKey := Ip4Path{Protocol: IpProtocolTcp, SourcePort: 1, DestinationPort: 81}
	idleKey := Ip4Path{Protocol: IpProtocolTcp, SourcePort: 2, DestinationPort: 82}
	pt.paths4[idleKey] = pathTableEntry{lastUsed: now.Add(-10 * time.Minute)}
	pt.paths4[activeKey] = pathTableEntry{lastUsed: now.Add(-1 * time.Second)}

	// evictOldest must pick the idle entry, not the active one.
	pt.evictOldest()
	if _, ok := pt.paths4[activeKey]; !ok {
		t.Fatal("evictOldest evicted the ACTIVE flow entry; active flows must be protected")
	}
	if _, ok := pt.paths4[idleKey]; ok {
		t.Fatal("evictOldest did not evict the idle (least-recently-used) entry")
	}

	// evictStale with a 5-minute cutoff must remove only the stale entry.
	pt.paths4[idleKey] = pathTableEntry{lastUsed: now.Add(-10 * time.Minute)}
	pt.paths4[activeKey] = pathTableEntry{lastUsed: now.Add(-1 * time.Second)}
	pt.evictStale(now)
	if _, ok := pt.paths4[activeKey]; !ok {
		t.Fatal("evictStale removed a recently-used (non-stale) entry")
	}
	if _, ok := pt.paths4[idleKey]; ok {
		t.Fatal("evictStale did not remove the stale (>5min idle) entry")
	}
}
