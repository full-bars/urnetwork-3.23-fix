package main

import (
	"testing"
	"time"
)

// TestNodeSnapshotCollectorIdlesWithoutReaders: the collector stops walking the
// proxy pool when nothing has read a snapshot for snapshotLazyIdleAfter, and
// the next Get re-arms sampling. The clocks and sources are the fixture's own
// (no sleeps): the tick gate is driven purely by advancing fakeSnapshotEnv.now.
func TestNodeSnapshotCollectorIdlesWithoutReaders(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0}
	src := env.sources()
	sourceBillable := src.billable
	billableCalls := 0
	src.billable = func() map[string]uint64 {
		billableCalls++
		return sourceBillable()
	}
	sourceTraffic := src.traffic
	trafficCalls := 0
	src.traffic = func() map[string]uint64 {
		trafficCalls++
		return sourceTraffic()
	}
	c := newNodeSnapshotCollector(src)

	// a reader arms sampling: the next tick walks the sources
	c.Get()
	base := billableCalls
	c.tick()
	if billableCalls == base || trafficCalls == 0 {
		t.Fatalf("a watched collector must sample (billable %d -> %d, traffic calls %d)", base, billableCalls, trafficCalls)
	}

	// idle past the threshold: ticks stop walking the pool entirely
	env.now = env.now.Add(snapshotLazyIdleAfter + time.Second)
	baseBillable, baseTraffic := billableCalls, trafficCalls
	c.tick()
	c.tick()
	if billableCalls != baseBillable || trafficCalls != baseTraffic {
		t.Fatalf("an idle collector must not walk the pool (billable +%d, traffic +%d)",
			billableCalls-baseBillable, trafficCalls-baseTraffic)
	}

	// a read re-arms it
	c.Get()
	c.tick()
	if billableCalls == baseBillable {
		t.Fatal("a read must re-arm sampling")
	}
}
