package main

import (
	"testing"
	"time"
)

// Session totals accumulate the same deltas the rate uses, so a proxy that was
// removed or respawned (its counters start over) never adds phantom bytes or
// subtracts real ones.
func TestRateSamplerSessionBytes(t *testing.T) {
	s := newRateSampler()
	s.sample(map[string]uint64{"a": 100, "b": 10}, snapT0) // baseline, counts nothing
	if got := s.session(); got != 0 {
		t.Fatalf("baseline counted %d bytes", got)
	}
	s.sample(map[string]uint64{"a": 150, "b": 10}, snapT0.Add(time.Second)) // a +50
	s.sample(map[string]uint64{"a": 10, "b": 30, "c": 999}, snapT0.Add(2*time.Second))
	// a went down (respawn) so it adds nothing, b +20, c is new and ignored.
	if got := s.session(); got != 70 {
		t.Fatalf("session bytes = %d, want 70", got)
	}
}

func TestSnapshotReportsBillableAndTotalTraffic(t *testing.T) {
	env := &fakeSnapshotEnv{
		now: snapT0, proxies: SnapshotProxies{Up: 1},
		billable: map[string]uint64{"p": 0}, traffic: map[string]uint64{"p": 0},
	}
	c := newNodeSnapshotCollector(env.sources())
	c.tick()
	env.now = env.now.Add(time.Second)
	env.billable, env.traffic = map[string]uint64{"p": 1000}, map[string]uint64{"p": 4000}
	c.tick()
	env.now = env.now.Add(time.Second)
	env.billable, env.traffic = map[string]uint64{"p": 3000}, map[string]uint64{"p": 9000}
	c.tick()

	snap := c.Get()
	tr := snap.Traffic
	if tr.BillableBytes != 3000 || tr.TotalBytes != 9000 {
		t.Fatalf("session totals = %d billable / %d total, want 3000 / 9000", tr.BillableBytes, tr.TotalBytes)
	}
	if tr.TotalNowBps != 5000 || tr.TotalAvg1mBps != 4500 {
		t.Fatalf("total rates now=%d avg1m=%d, want 5000 and 4500", tr.TotalNowBps, tr.TotalAvg1mBps)
	}
	if len(tr.TotalHistoryBps) != 2 || tr.TotalHistoryBps[0] != 4000 || tr.TotalHistoryBps[1] != 5000 {
		t.Fatalf("total history = %v", tr.TotalHistoryBps)
	}
	// The existing billable rate is unchanged.
	if snap.Rate.NowBps != 2000 {
		t.Fatalf("billable rate now = %d, want 2000", snap.Rate.NowBps)
	}
	if tr.LifetimeBillableBytes != nil {
		t.Fatalf("lifetime billable present without a source: %d", *tr.LifetimeBillableBytes)
	}
}

func TestSnapshotLifetimeBillableIsOptional(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0.Add(time.Hour), proxies: SnapshotProxies{Up: 1}, lifetime: 12345, hasLifetime: true}
	c := newNodeSnapshotCollector(env.sources())
	got := c.Get().Traffic.LifetimeBillableBytes
	if got == nil || *got != 12345 {
		t.Fatalf("lifetime billable = %v, want 12345", got)
	}
}

// The light reply top polls at 100ms: live counter sums plus a timestamp, no
// snapshot build. The timestamp is the provider's, so rates are computed
// against the provider's clock, not the socket round trip.
func TestLiveTrafficSample(t *testing.T) {
	at := snapT0.Add(90 * time.Minute)
	got := liveTrafficSample(at, func() (uint64, uint64) { return 700, 2100 })
	if got.AtUnixNano != at.UnixNano() || got.BillableBytes != 700 || got.TotalBytes != 2100 {
		t.Fatalf("sample = %+v", got)
	}
}
