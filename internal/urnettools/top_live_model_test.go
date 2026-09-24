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

// The graph is the provider's per-second history with the live rate as its
// newest column, so the right edge moves with every poll. It is anchored to the
// provider's clock so completed columns never change.
func TestTopSeriesAppendsTheLiveColumnAndAnchorsToProviderTime(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	base := len(m.snap.Rate.HistoryBps)
	snapAt, _ := time.Parse(time.RFC3339, m.snap.Now)

	bill, total, anchor := m.series()
	if len(bill) != base || anchor != snapAt.Unix() {
		t.Fatalf("no live rate yet: %d samples (want %d), anchor %d (want %d)", len(bill), base, anchor, snapAt.Unix())
	}
	if len(total) != len(m.snap.Traffic.TotalHistoryBps) {
		t.Fatalf("total series has %d samples, want %d", len(total), len(m.snap.Traffic.TotalHistoryBps))
	}

	for i := 0; i < 12; i++ {
		clock.Advance(100 * time.Millisecond)
		gen, _, _ := m.wantTraffic(clock.Now())
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 3000}, nil)
	}
	bill, total, anchor = m.series()
	if len(bill) != base+1 || anchor != snapAt.Unix()+1 {
		t.Fatalf("live column missing: %d samples (want %d), anchor %d (want %d)", len(bill), base+1, anchor, snapAt.Unix()+1)
	}
	if last := bill[len(bill)-1]; last < 9_900 || last > 10_100 {
		t.Fatalf("newest billable column = %.0f, want the live 10000", last)
	}
	if last := total[len(total)-1]; last < 29_700 || last > 30_300 {
		t.Fatalf("newest total column = %.0f, want the live 30000", last)
	}
	// The snapshot's own history must not be modified by appending to a copy.
	if len(m.snap.Rate.HistoryBps) != base {
		t.Fatalf("the snapshot's history grew to %d", len(m.snap.Rate.HistoryBps))
	}
}

func TestTopSeriesWithoutATimeBaseIsUnanchored(t *testing.T) {
	m, _ := liveModel(t, 100*time.Millisecond)
	m.snap.Now = ""
	if _, _, anchor := m.series(); anchor != 0 {
		t.Fatalf("anchor = %d with no provider time, want 0", anchor)
	}
}

func TestTopSeriesForAnOldProviderHasNoTotalGraph(t *testing.T) {
	m, _ := liveModel(t, 100*time.Millisecond)
	m.snap.Traffic = nil
	bill, total, _ := m.series()
	if len(bill) == 0 || total != nil {
		t.Fatalf("old provider: billable %d samples, total %v; want billable only", len(bill), total)
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
