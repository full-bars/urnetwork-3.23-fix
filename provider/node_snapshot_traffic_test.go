package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
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

// The per-direction block is present only when the provider reports the
// directional counters. An older provider leaves it nil, which is how top knows
// to hide the direction pages.
func TestSnapshotDirectionsBlock(t *testing.T) {
	old := &fakeSnapshotEnv{now: snapT0.Add(time.Hour), proxies: SnapshotProxies{Up: 1}}
	if tr := newNodeSnapshotCollector(old.sources()).Get().Traffic; tr.Directions != nil {
		t.Fatalf("provider without directional counters reported a block: %+v", tr.Directions)
	}

	var state map[string]directionLanes
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 1}, dirSource: func() map[string]directionLanes { return state }}
	c := newNodeSnapshotCollector(env.sources())

	state = map[string]directionLanes{"p": {billableRx: 1000, billableTx: 100, totalRx: 5000, totalTx: 500}}
	c.tick() // baseline, counts nothing
	env.now = env.now.Add(time.Second)
	state = map[string]directionLanes{"p": {billableRx: 1300, billableTx: 200, totalRx: 5400, totalTx: 700}}
	c.tick()
	env.now = env.now.Add(time.Second)
	state = map[string]directionLanes{"p": {billableRx: 1600, billableTx: 400, totalRx: 5800, totalTx: 1000}}
	c.tick()

	d := c.Get().Traffic.Directions
	if d == nil {
		t.Fatal("provider with directional counters did not report a block")
	}
	// rx is download (bytes received) and tx is upload (bytes sent). The four
	// lanes carry distinct deltas so a swapped mapping cannot pass.
	wantBillableRx := []int64{300, 300}
	wantBillableTx := []int64{100, 200}
	wantTotalRx := []int64{400, 400}
	wantTotalTx := []int64{200, 300}
	if !reflect.DeepEqual(d.BillableRx.HistoryBps, wantBillableRx) || !reflect.DeepEqual(d.BillableTx.HistoryBps, wantBillableTx) ||
		!reflect.DeepEqual(d.TotalRx.HistoryBps, wantTotalRx) || !reflect.DeepEqual(d.TotalTx.HistoryBps, wantTotalTx) {
		t.Fatalf("history = %v/%v/%v/%v, want %v/%v/%v/%v",
			d.BillableRx.HistoryBps, d.BillableTx.HistoryBps, d.TotalRx.HistoryBps, d.TotalTx.HistoryBps,
			wantBillableRx, wantBillableTx, wantTotalRx, wantTotalTx)
	}
	if d.BillableRx.NowBps != 300 || d.BillableRx.Avg1mBps != 300 ||
		d.BillableTx.NowBps != 200 || d.BillableTx.Avg1mBps != 150 ||
		d.TotalRx.NowBps != 400 || d.TotalRx.Avg1mBps != 400 ||
		d.TotalTx.NowBps != 300 || d.TotalTx.Avg1mBps != 250 {
		t.Fatalf("rates = %+v", d)
	}
}

// A respawned proxy restarts its counters at zero. The direction sampler must
// add nothing for that tick instead of wrapping the uint64 subtraction.
func TestDirectionSamplerSkipsDecreases(t *testing.T) {
	s := newDirectionSampler()
	s.sample(map[string]directionLanes{"p": {billableRx: 1000, billableTx: 100, totalRx: 5000, totalTx: 500}}, snapT0) // baseline
	s.sample(map[string]directionLanes{"p": {billableRx: 10, billableTx: 20, totalRx: 30, totalTx: 40}}, snapT0.Add(time.Second))

	for lane := 0; lane < directionLaneCount; lane++ {
		if got := s.history(lane); len(got) != 1 || got[0] != 0 {
			t.Fatalf("lane %d history = %v, want [0]", lane, got)
		}
	}

	// A proxy that disappears adds nothing, and a new key contributes from its
	// own baseline, not from zero.
	s.sample(map[string]directionLanes{}, snapT0.Add(2*time.Second))
	s.sample(map[string]directionLanes{"q": {billableRx: 9000, billableTx: 9000, totalRx: 9000, totalTx: 9000}}, snapT0.Add(3*time.Second))
	if got := s.history(dirTotalTx); !reflect.DeepEqual(got, []int64{0, 0, 0}) {
		t.Fatalf("total tx history = %v, want [0 0 0]", got)
	}
}

// The IP family split reports the IPv6 lane as counted and IPv4 as the base
// lane minus it. The two are separate counters, so a tick can read the IPv6
// delta ahead of its base and drive the difference negative; the split clamps
// it to zero rather than reporting a negative rate.
func TestSnapshotFamilySplit(t *testing.T) {
	var state map[string]directionLanes
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 1}, dirSource: func() map[string]directionLanes { return state }}
	src := env.sources()
	src.familyCounters = true
	c := newNodeSnapshotCollector(src)

	state = map[string]directionLanes{"p": {billableRx: 1000, billableTx: 1000, totalRx: 1000, totalTx: 1000, billableRxV6: 100, billableTxV6: 100, totalRxV6: 100, totalTxV6: 100}}
	c.tick() // baseline
	env.now = env.now.Add(time.Second)
	// All v4: the base lanes move and the IPv6 lanes stand still.
	state = map[string]directionLanes{"p": {billableRx: 1400, billableTx: 1400, totalRx: 1400, totalTx: 1400, billableRxV6: 100, billableTxV6: 100, totalRxV6: 100, totalTxV6: 100}}
	c.tick()
	env.now = env.now.Add(time.Second)
	// Mixed: base +1000, IPv6 +400, so IPv4 is 600.
	state = map[string]directionLanes{"p": {billableRx: 2400, billableTx: 2400, totalRx: 2400, totalTx: 2400, billableRxV6: 500, billableTxV6: 500, totalRxV6: 500, totalTxV6: 500}}
	c.tick()
	env.now = env.now.Add(time.Second)
	// The IPv6 counter arrives one sample before its base: +300 IPv6 over a
	// +100 base. IPv4 must read 0, not -200.
	state = map[string]directionLanes{"p": {billableRx: 2500, billableTx: 2500, totalRx: 2500, totalTx: 2500, billableRxV6: 800, billableTxV6: 800, totalRxV6: 800, totalTxV6: 800}}
	c.tick()

	d := c.Get().Traffic.Directions
	if d == nil || d.Families == nil {
		t.Fatalf("family counters present but no split reported: %+v", d)
	}
	for _, q := range []struct {
		name   string
		split  SnapshotFamilySplit
		v4Want []int64
		v6Want []int64
	}{
		{"billable rx", d.Families.BillableRx, []int64{400, 600, 0}, []int64{0, 400, 300}},
		{"billable tx", d.Families.BillableTx, []int64{400, 600, 0}, []int64{0, 400, 300}},
		{"total rx", d.Families.TotalRx, []int64{400, 600, 0}, []int64{0, 400, 300}},
		{"total tx", d.Families.TotalTx, []int64{400, 600, 0}, []int64{0, 400, 300}},
	} {
		if !reflect.DeepEqual(q.split.V4.HistoryBps, q.v4Want) || !reflect.DeepEqual(q.split.V6.HistoryBps, q.v6Want) {
			t.Fatalf("%s history = v4 %v v6 %v, want v4 %v v6 %v", q.name, q.split.V4.HistoryBps, q.split.V6.HistoryBps, q.v4Want, q.v6Want)
		}
		if q.split.V4.NowBps != q.v4Want[len(q.v4Want)-1] || q.split.V6.NowBps != q.v6Want[len(q.v6Want)-1] {
			t.Fatalf("%s now = v4 %d v6 %d, want v4 %d v6 %d", q.name, q.split.V4.NowBps, q.split.V6.NowBps, q.v4Want[len(q.v4Want)-1], q.v6Want[len(q.v6Want)-1])
		}
	}
}

// A source that does not count IPv6 traffic reports directional rates without a
// family split, so a reader draws the old page instead of a flat IPv6 line.
func TestSnapshotFamilySplitAbsentWithoutCounters(t *testing.T) {
	state := map[string]directionLanes{"p": {billableRx: 1000, totalRx: 1000}}
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 1}, dirSource: func() map[string]directionLanes { return state }}
	c := newNodeSnapshotCollector(env.sources())
	c.tick()
	env.now = env.now.Add(time.Second)
	state = map[string]directionLanes{"p": {billableRx: 2000, totalRx: 2000}}
	c.tick()

	d := c.Get().Traffic.Directions
	if d == nil {
		t.Fatal("directional block missing")
	}
	if d.Families != nil {
		t.Fatalf("family split reported without family counters: %+v", d.Families)
	}
}

// The light reply top polls at 100ms: live counter sums plus a timestamp, no
// snapshot build. The timestamp is the provider's, so rates are computed
// against the provider's clock, not the socket round trip.
func TestLiveTrafficSample(t *testing.T) {
	at := snapT0.Add(90 * time.Minute)
	got := liveTrafficSample(at, func() (billableRx, billableTx, totalRx, totalTx uint64) {
		return 300, 400, 900, 1200
	}, func() (uint64, uint64) { return 5, 7 }, func() connect.ProxyBandwidthFamily {
		return connect.ProxyBandwidthFamily{BillableRx: 10, BillableTx: 12, TotalRx: 20, TotalTx: 30, BillableRxPackets: 1, BillableTxPackets: 2}
	})
	if got.AtUnixNano != at.UnixNano() {
		t.Fatalf("timestamp = %d, want %d", got.AtUnixNano, at.UnixNano())
	}
	if got.BillableBytes != 700 || got.TotalBytes != 2100 {
		t.Fatalf("sums = %d/%d, want 700/2100", got.BillableBytes, got.TotalBytes)
	}
	if got.BillableRxBytes != 300 || got.BillableTxBytes != 400 || got.TotalRxBytes != 900 || got.TotalTxBytes != 1200 {
		t.Fatalf("directions = %+v", got)
	}
	if got.BillableRxBytesV6 != 10 || got.BillableTxBytesV6 != 12 ||
		got.TotalRxBytesV6 != 20 || got.TotalTxBytesV6 != 30 ||
		got.BillableRxPacketsV6 != 1 || got.BillableTxPacketsV6 != 2 {
		t.Fatalf("ipv6 counters = %+v", got)
	}
}

// The billable and total samplers read the same per-proxy counters. Building the
// proxy snapshot is the expensive part (about 2ms and 35k allocations at 5000
// proxies), so a tick that samples both must build it once, not twice.
func TestBandwidthShareBuildsOncePerTick(t *testing.T) {
	now := snapT0
	reads := 0
	share := newBandwidthShare(func() time.Time { return now }, 500*time.Millisecond, func() proxyBandwidthRead {
		reads++
		return proxyBandwidthRead{
			billable:   map[string]uint64{"p": uint64(reads)},
			total:      map[string]uint64{"p": uint64(reads) * 10},
			directions: map[string]directionLanes{"p": {billableRx: 1, billableTx: 2, totalRx: 3, totalTx: 4}},
		}
	})

	b1, t1, d1 := share.get()
	if reads != 1 || b1["p"] != 1 || t1["p"] != 10 || d1["p"].billableRx != 1 || d1["p"].totalTx != 4 {
		t.Fatalf("reads=%d billable=%v total=%v directions=%v, want one read serving both", reads, b1, t1, d1)
	}

	now = now.Add(time.Second) // the next tick
	b2, t2, d2 := share.get()
	if reads != 2 || b2["p"] != 2 || t2["p"] != 20 || d2["p"].billableTx != 2 {
		t.Fatalf("next tick: reads=%d billable=%v total=%v directions=%v, want a fresh read", reads, b2, t2, d2)
	}
}

// The graph buckets history against the absolute index of its newest sample, so
// that index must only ever grow by one per recorded sample, and must keep
// growing after the 600 slot ring wraps.
func TestRateSamplerSeqCountsEverySampleAndSurvivesTheRingWrapping(t *testing.T) {
	s := newRateSampler()
	if s.seq() != 0 {
		t.Fatalf("seq = %d before any sample", s.seq())
	}
	at := snapT0
	s.sample(map[string]uint64{"a": 0}, at) // the baseline records nothing
	if s.seq() != 0 {
		t.Fatalf("the baseline sample was counted: seq = %d", s.seq())
	}
	for i := 1; i <= snapshotRingSize+250; i++ {
		at = at.Add(time.Second)
		s.sample(map[string]uint64{"a": uint64(i)}, at)
		if got := s.seq(); got != uint64(i) {
			t.Fatalf("after %d samples seq = %d", i, got)
		}
	}
	if len(s.history()) != snapshotRingSize {
		t.Fatalf("history holds %d samples, want the ring's %d", len(s.history()), snapshotRingSize)
	}
}

func TestSnapshotCarriesTheHistorySeqOfItsNewestSample(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0, proxies: SnapshotProxies{Up: 1}, billable: map[string]uint64{"p": 0}, traffic: map[string]uint64{"p": 0}}
	c := newNodeSnapshotCollector(env.sources())
	c.tick()
	for i := 1; i <= 5; i++ {
		env.now = env.now.Add(time.Second)
		env.billable = map[string]uint64{"p": uint64(i) * 100}
		env.traffic = map[string]uint64{"p": uint64(i) * 250}
		c.tick()
	}
	snap := c.Get()
	if snap.Rate.HistorySeq != 5 || len(snap.Rate.HistoryBps) != 5 {
		t.Fatalf("history_seq = %d with %d samples, want 5 and 5", snap.Rate.HistorySeq, len(snap.Rate.HistoryBps))
	}
	// The billable and total samplers tick together, so one index serves both.
	if len(snap.Traffic.TotalHistoryBps) != 5 {
		t.Fatalf("total history has %d samples, want 5 to match the shared index", len(snap.Traffic.TotalHistoryBps))
	}
}

func TestSnapshotPacketRates(t *testing.T) {
	old := &fakeSnapshotEnv{now: snapT0}
	if newNodeSnapshotCollector(old.sources()).Get().Traffic.Packets != nil {
		t.Fatal("old source has packets")
	}
	var state map[string]directionLanes
	env := &fakeSnapshotEnv{now: snapT0, dirSource: func() map[string]directionLanes { return state }}
	src := env.sources()
	src.packetCounters = true
	c := newNodeSnapshotCollector(src)
	state = map[string]directionLanes{"p": {packetRx: 10, packetTx: 20}}
	c.tick()
	env.now = env.now.Add(time.Second)
	state = map[string]directionLanes{"p": {packetRx: 13, packetTx: 27}}
	c.tick()
	env.now = env.now.Add(time.Second)
	state = map[string]directionLanes{"p": {packetRx: 1, packetTx: 30}}
	c.tick()
	got := c.Get().Traffic.Packets
	if got == nil || !reflect.DeepEqual(got.BillableRx.HistoryPps, []int64{3, 0}) || !reflect.DeepEqual(got.BillableTx.HistoryPps, []int64{7, 3}) {
		t.Fatalf("packet rates: %+v", got)
	}
}
