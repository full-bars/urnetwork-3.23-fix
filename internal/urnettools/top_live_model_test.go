package urnettools

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

func liveModel(t *testing.T, interval time.Duration) (*topModel, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.interval = interval
	gen, _, ok := m.wantFetch(clock.Now())
	if !ok {
		t.Fatal("first fetch not requested")
	}
	m.apply(gen, topFlowing(t), nil)
	return m, clock
}

// top polls the cheap traffic counters every interval, down to 100ms, but the
// full snapshot (cached for a second by the provider, and costly to build) at
// most once a second however fast the rate is set.
func TestTopPollsTrafficEveryIntervalButSnapshotAtMostOnceASecond(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	snapshots, traffics := 0, 0
	for i := 0; i < 30; i++ { // three seconds of 100ms ticks
		clock.Advance(100 * time.Millisecond)
		if gen, _, ok := m.wantFetch(clock.Now()); ok {
			snapshots++
			m.apply(gen, topFlowing(t), nil)
		}
		if gen, _, ok := m.wantTraffic(clock.Now()); ok {
			traffics++
			m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 2000}, nil)
		}
	}
	if snapshots < 2 || snapshots > 4 {
		t.Fatalf("snapshots fetched = %d in 3s at 100ms, want about 3", snapshots)
	}
	if traffics != 30 {
		t.Fatalf("traffic polls = %d in 3s at 100ms, want 30", traffics)
	}
}

func TestTopTrafficIsNotPolledBeforeConnectedOrWhileInFlight(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	if _, _, ok := m.wantTraffic(clock.Now()); ok {
		t.Fatal("traffic polled before the first snapshot")
	}
	m2, clock2 := liveModel(t, 100*time.Millisecond)
	if _, _, ok := m2.wantTraffic(clock2.Now()); !ok {
		t.Fatal("first traffic poll not requested once connected")
	}
	clock2.Advance(time.Second)
	if _, _, ok := m2.wantTraffic(clock2.Now()); ok {
		t.Fatal("a second traffic poll was started while one is in flight")
	}
}

// An older provider answers "unknown command". top must stop asking, and carry
// on with the snapshot's own once-a-second rates.
func TestTopStopsAskingAnOldProviderForTraffic(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	gen, _, _ := m.wantTraffic(clock.Now())
	m.applyTraffic(gen, nil, errTrafficUnsupported)
	clock.Advance(time.Second)
	if _, _, ok := m.wantTraffic(clock.Now()); ok {
		t.Fatal("kept polling a provider that has no traffic command")
	}
	if m.conn != topConnected {
		t.Fatalf("an old provider was reported %v, want still connected", m.conn)
	}
	if r := m.rates(); r.billable != m.snap.Rate.NowBps {
		t.Fatalf("no fallback to the snapshot rate: %.0f vs %.0f", r.billable, m.snap.Rate.NowBps)
	}
}

func TestTopTrafficErrorsAreNotProviderLoss(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	gen, _, _ := m.wantTraffic(clock.Now())
	m.applyTraffic(gen, nil, errors.New("live status unavailable: timeout"))
	if m.conn != topConnected || len(m.events) != 1 { // the one restart event from the first snapshot
		t.Fatalf("a missed traffic sample changed the connection: conn=%v events=%d", m.conn, len(m.events))
	}
	clock.Advance(100 * time.Millisecond)
	if _, _, ok := m.wantTraffic(clock.Now()); !ok {
		t.Fatal("polling did not continue after a missed sample")
	}
}

func TestTopIgnoresTrafficForAnEarlierSelection(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	gen, _, _ := m.wantFetch(clock.Now())
	m.apply(gen, topFlowing(t), nil)
	old, _, _ := m.wantTraffic(clock.Now())
	m.selectProvider(1) // tab to the next provider
	m.applyTraffic(old, &LiveTraffic{AtUnixNano: 1, BillableBytes: 1}, nil)
	if len(m.live.samples) != 0 {
		t.Fatal("traffic from the previous provider was applied to the new one")
	}
}

func TestTopLiveRatesReplaceTheSnapshotRateWhenAvailable(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	if r := m.rates(); r.billable != m.snap.Rate.NowBps || r.total != m.snap.Traffic.TotalNowBps {
		t.Fatalf("before any traffic sample the snapshot rates are shown: %+v", r)
	}
	for i := 0; i < 12; i++ {
		clock.Advance(100 * time.Millisecond)
		gen, _, _ := m.wantTraffic(clock.Now())
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 3000}, nil)
	}
	r := m.rates()
	if !r.totalOK || r.billable < 9_900 || r.billable > 10_100 || r.total < 29_700 || r.total > 30_300 {
		t.Fatalf("live rates = %+v, want about 10000 / 30000", r)
	}
	// A lost provider must not keep showing a live rate.
	m.lost(clock.Now(), errors.New("gone"))
	if _, _, ok := m.live.rates(); ok {
		t.Fatal("live rate survived losing the provider")
	}
}

// The graph is the provider's per-second history, bucketed against the provider's
// own sample count, with the live rate as a separate tail column. The tail is not
// appended to the series: a sample at a guessed index would be replaced by the
// provider's real one a second later, changing a column that was already complete.
func TestTopSeriesKeepsTheLiveRateAsASeparateTail(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	m.snap.Rate.HistorySeq = 4242
	base := len(m.snap.Rate.HistoryBps)

	sr := m.series()
	if sr.live || len(sr.billable) != base || sr.anchor != 4242 {
		t.Fatalf("no live rate yet: live=%v %d samples (want %d), anchor %d (want the provider's sample count 4242)", sr.live, len(sr.billable), base, sr.anchor)
	}
	if len(sr.total) != len(m.snap.Traffic.TotalHistoryBps) {
		t.Fatalf("total series has %d samples, want %d", len(sr.total), len(m.snap.Traffic.TotalHistoryBps))
	}

	for i := 0; i < 12; i++ {
		clock.Advance(100 * time.Millisecond)
		gen, _, _ := m.wantTraffic(clock.Now())
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 3000}, nil)
	}
	sr = m.series()
	if !sr.live || len(sr.billable) != base || len(sr.total) != len(m.snap.Traffic.TotalHistoryBps) {
		t.Fatalf("the live rate must not be appended to the series: live=%v %d samples (want %d)", sr.live, len(sr.billable), base)
	}
	if sr.anchor != 4242 {
		t.Fatalf("the live rate moved the anchor to %d", sr.anchor)
	}
	if sr.tailBillable < 9_900 || sr.tailBillable > 10_100 || sr.tailTotal < 29_700 || sr.tailTotal > 30_300 {
		t.Fatalf("tail = %.0f / %.0f, want the live 10000 / 30000", sr.tailBillable, sr.tailTotal)
	}
}

// A provider that predates history_seq has only the snapshot's clock to bucket
// against, and a snapshot with no time at all has nothing.
func TestTopSeriesAnchorFallsBackToTheSnapshotClock(t *testing.T) {
	m, _ := liveModel(t, 100*time.Millisecond)
	m.snap.Rate.HistorySeq = 0
	snapAt, _ := time.Parse(time.RFC3339, m.snap.Now)
	if sr := m.series(); sr.anchor != snapAt.Unix() {
		t.Fatalf("anchor = %d, want the snapshot's clock second %d", sr.anchor, snapAt.Unix())
	}
	m.snap.Now = ""
	if sr := m.series(); sr.anchor != 0 {
		t.Fatalf("anchor = %d with no time base at all, want 0", sr.anchor)
	}
}

func TestTopSeriesForAnOldProviderHasNoTotalGraph(t *testing.T) {
	m, _ := liveModel(t, 100*time.Millisecond)
	m.snap.Traffic = nil
	sr := m.series()
	if len(sr.billable) == 0 || sr.total != nil {
		t.Fatalf("old provider: billable %d samples, total %v; want billable only", len(sr.billable), sr.total)
	}
	if r := m.rates(); r.totalOK {
		t.Fatal("total rate claimed with no traffic block")
	}
}

func TestTopIntervalStepsGoDownTo100ms(t *testing.T) {
	if topMinInterval != 100*time.Millisecond || topIntervalSteps[0] != 100*time.Millisecond {
		t.Fatalf("fastest rate = %v, want 100ms like btop", topMinInterval)
	}
	m, _ := liveModel(t, 250*time.Millisecond)
	m.stepInterval(-1)
	if m.interval != 100*time.Millisecond {
		t.Fatalf("stepping faster from 250ms gave %v", m.interval)
	}
	m.stepInterval(-1)
	if m.interval != 100*time.Millisecond {
		t.Fatalf("stepping past the fastest gave %v", m.interval)
	}
}
