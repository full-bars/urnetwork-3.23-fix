package urnettools

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func pressKey(m *topModel, r rune) {
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: r})
}

// --- steady axis ---------------------------------------------------------

func TestTopScaleRisesAtOnceWithHeadroom(t *testing.T) {
	var s topScale
	now := topBase
	const mib = 1 << 20
	got := s.update(40*mib, now, true)
	if want := tui.NiceCeil(40*mib*topScaleHeadroom, true); got != want || got <= 40*mib {
		t.Fatalf("axis = %v, want %v (above the peak)", got, want)
	}
	// A burst a little above the last one fits under the headroom: the axis, and
	// so every column drawn against it, does not move.
	if again := s.update(45*mib, now.Add(time.Second), true); again != got {
		t.Fatalf("axis moved %v -> %v for a small burst", got, again)
	}
	// A real jump raises it immediately.
	if up := s.update(200*mib, now.Add(2*time.Second), true); up <= got {
		t.Fatalf("axis did not rise for a big burst: %v", up)
	}
}

func TestTopScaleFallsOnlyAfterTheHold(t *testing.T) {
	var s topScale
	now := topBase
	const mib = 1 << 20
	high := s.update(100*mib, now, true)

	// The burst scrolls off and the peak drops to a tenth. The axis holds.
	for d := time.Second; d < topScaleHold; d += time.Second {
		if got := s.update(10*mib, now.Add(d), true); got != high {
			t.Fatalf("axis fell to %v after %v; it must hold %v for %v", got, d, high, topScaleHold)
		}
	}
	// The hold is timed from the first low reading (at +1s).
	got := s.update(10*mib, now.Add(time.Second+topScaleHold), true)
	if got >= high {
		t.Fatalf("axis still %v after the hold", got)
	}
	if got < 10*mib {
		t.Fatalf("axis %v fell below the peak", got)
	}
}

func TestTopScaleHoldRestartsWhenThePeakComesBack(t *testing.T) {
	var s topScale
	now := topBase
	const mib = 1 << 20
	high := s.update(100*mib, now, true)
	s.update(10*mib, now.Add(time.Second), true)    // low spell begins
	s.update(90*mib, now.Add(20*time.Second), true) // peak returns: spell over
	// 25s of low after that is not yet a hold of 30s from the return.
	if got := s.update(10*mib, now.Add(21*time.Second), true); got != high {
		t.Fatalf("axis = %v", got)
	}
	if got := s.update(10*mib, now.Add(45*time.Second), true); got != high {
		t.Fatalf("axis fell at %v, only 24s into the new low spell", got)
	}
}

func TestTopScaleIgnoresModestDips(t *testing.T) {
	var s topScale
	now := topBase
	const mib = 1 << 20
	high := s.update(100*mib, now, true)
	// 80% of the peak is not "well under": never lowered, however long.
	for d := time.Duration(0); d < 5*time.Minute; d += 10 * time.Second {
		if got := s.update(80*mib, now.Add(d), true); got != high {
			t.Fatalf("axis moved to %v on a modest dip", got)
		}
	}
}

func TestTopScaleZeroPeak(t *testing.T) {
	var s topScale
	if got := s.update(0, topBase, true); got != 0 {
		t.Fatalf("no traffic must give no axis, got %v", got)
	}
}

// --- live recent rate ------------------------------------------------------

func TestTopLiveRecentUsesTheNewestSpan(t *testing.T) {
	var l topLive
	const step = 100 * time.Millisecond
	// 10 MB/s for a second, then 50 MB/s.
	var at int64
	var bytes uint64
	for i := 0; i < 10; i++ {
		at += int64(step)
		bytes += 1_000_000
		l.add(at, bytes, bytes)
	}
	for i := 0; i < 4; i++ {
		at += int64(step)
		bytes += 5_000_000
		l.add(at, bytes, bytes)
	}
	b, _, ok := l.recent(250 * time.Millisecond)
	if !ok || math.Abs(b-50e6) > 1 {
		t.Fatalf("recent = %v %v, want 50 MB/s from the newest 300ms", b, ok)
	}
	// The whole-window rate still averages the old and new traffic.
	if w, _, _ := l.rates(); w >= 50e6 || w <= 10e6 {
		t.Fatalf("window rate %v should sit between the two", w)
	}
}

func TestTopLiveRecentNeedsASpan(t *testing.T) {
	var l topLive
	if _, _, ok := l.recent(time.Second); ok {
		t.Fatal("no samples must not give a rate")
	}
	l.add(int64(time.Second), 100, 100)
	if _, _, ok := l.recent(time.Second); ok {
		t.Fatal("one sample must not give a rate")
	}
}

// --- zoom ------------------------------------------------------------------

// feed applies n traffic readings 100ms apart at the given byte rate.
func feedTraffic(t *testing.T, m *topModel, clock *fakeClock, n int, bytesPerStep uint64, start *int64, bytes *uint64) {
	t.Helper()
	for i := 0; i < n; i++ {
		clock.Advance(100 * time.Millisecond)
		gen, _, ok := m.wantTraffic(clock.Now())
		if !ok {
			t.Fatal("traffic not requested")
		}
		*start += int64(100 * time.Millisecond)
		*bytes += bytesPerStep
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: *start, BillableBytes: *bytes, TotalBytes: *bytes * 2}, nil)
	}
}

func TestTopZoomFillsAtThePollRateAndStopsAtTheWindow(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	if got := m.zoomCapacity(); got != 150 {
		t.Fatalf("capacity at 100ms = %d, want 150 (15s)", got)
	}
	feedTraffic(t, m, clock, 200, 1_000_000, &at, &bytes) // 10 MB/s
	if n := len(m.zoom.billable); n != 150 {
		t.Fatalf("ring holds %d columns, want it capped at 150", n)
	}
	if m.zoom.seq != 199 { // the first reading has no span yet
		t.Fatalf("seq = %d, want 199 columns ever pushed", m.zoom.seq)
	}
	last := m.zoom.billable[len(m.zoom.billable)-1]
	if math.Abs(last-10e6) > 1 {
		t.Fatalf("newest column = %v, want 10 MB/s", last)
	}
	if tot := m.zoom.total[len(m.zoom.total)-1]; math.Abs(tot-20e6) > 1 {
		t.Fatalf("total column = %v, want 20 MB/s", tot)
	}
}

func TestTopZoomCapacityFollowsTheInterval(t *testing.T) {
	m, _ := liveModel(t, time.Second)
	if got := m.zoomCapacity(); got != 15 {
		t.Fatalf("capacity at 1s = %d, want 15", got)
	}
	m.interval = 10 * time.Second
	if got := m.zoomCapacity(); got != topZoomMinCols {
		t.Fatalf("capacity at 10s = %d, want the floor %d", got, topZoomMinCols)
	}
}

func TestTopZoomRestartsWhenItsTimeBaseWouldBreak(t *testing.T) {
	setup := func() (*topModel, *fakeClock) {
		m, clock := liveModel(t, 100*time.Millisecond)
		var at int64 = 1_000_000_000_000
		var bytes uint64
		feedTraffic(t, m, clock, 20, 1_000_000, &at, &bytes)
		if len(m.zoom.billable) == 0 {
			t.Fatal("zoom ring empty after feeding")
		}
		return m, clock
	}
	t.Run("rate change", func(t *testing.T) {
		m, _ := setup()
		pressKey(m, '-')
		if len(m.zoom.billable) != 0 {
			t.Fatal("columns of another poll period must not share one axis")
		}
	})
	t.Run("connection lost", func(t *testing.T) {
		m, clock := setup()
		m.lost(clock.Now(), errors.New("gone"))
		if len(m.zoom.billable) != 0 {
			t.Fatal("a window from before the outage would read as current")
		}
	})
	t.Run("provider switch", func(t *testing.T) {
		m, clock := setup()
		m.providers = topProviders(2)
		_ = clock
		m.selectProvider(1)
		if len(m.zoom.billable) != 0 || m.zoomScaleB.top != 0 {
			t.Fatal("history belongs to one provider")
		}
	})
}

func TestTopZoomIgnoresAStalledClockAndCounterResets(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTraffic(t, m, clock, 10, 1_000_000, &at, &bytes)
	seq := m.zoom.seq
	// The provider clock did not advance: no new column.
	gen, _, _ := m.wantTraffic(clock.Now().Add(time.Second))
	m.applyTraffic(gen, &LiveTraffic{AtUnixNano: at, BillableBytes: bytes + 5, TotalBytes: bytes + 5}, nil)
	if m.zoom.seq != seq {
		t.Fatal("a reading on a stalled clock added a column")
	}
	// A counter that went backwards (a proxy respawned) restarts the window and
	// never becomes a negative or huge column.
	clock.Advance(time.Second)
	gen, _, _ = m.wantTraffic(clock.Now())
	m.applyTraffic(gen, &LiveTraffic{AtUnixNano: at + int64(100*time.Millisecond), BillableBytes: 10, TotalBytes: 10}, nil)
	for _, v := range m.zoom.billable {
		if v < 0 || v > 1e9 {
			t.Fatalf("column %v after a counter reset", v)
		}
	}
}

// The rule the graph exists to keep, for the zoom window too: once a column is
// complete it does not change as the window scrolls.
func TestTopZoomColumnsHoldStillAsTheWindowScrolls(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	const cols = 100
	seen := map[int64]float64{}
	changes := 0
	for i := 0; i < 400; i++ {
		// Bursty traffic, so a shifted column would show.
		step := uint64(burst(int64(i))) / 10
		feedTraffic(t, m, clock, 1, step, &at, &bytes)
		sr := m.series()
		_ = sr
		z := m.zoom
		ids, vals := tui.GraphBuckets(z.billable, cols, z.seq, m.zoomCapacity())
		for j := 0; j < len(ids)-1; j++ {
			if prev, ok := seen[ids[j]]; ok && math.Abs(prev-vals[j]) > 1e-6 {
				changes++
			}
			seen[ids[j]] = vals[j]
		}
	}
	if changes > 0 {
		t.Fatalf("%d completed zoom columns changed while scrolling", changes)
	}
}

func TestTopZoomKeyTogglesTheGraphs(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64

	pressKey(m, 'w')
	if sr := m.series(); !sr.zoom || len(sr.billable) != 0 {
		t.Fatalf("zoom before any reading: %+v", sr.zoom)
	}
	txt, _ := showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "Billable, last 15s (collecting)") || !strings.Contains(txt, "collecting live readings") {
		t.Fatalf("empty zoom must say it is collecting:\n%s", txt)
	}

	feedTraffic(t, m, clock, 30, 1_000_000, &at, &bytes)
	txt, _ = showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "Billable, last 15s") || strings.Contains(txt, "(collecting)") {
		t.Fatalf("zoom title:\n%s", txt)
	}
	if !strings.Contains(txt, "Total traffic, last 15s") {
		t.Fatalf("total graph did not zoom:\n%s", txt)
	}

	pressKey(m, 'w')
	txt, _ = showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "Billable, 10m") || strings.Contains(txt, "last 15s") {
		t.Fatalf("zoom did not toggle off:\n%s", txt)
	}
}

func TestTopZoomFallsBackForAProviderWithoutTheTrafficCommand(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	gen, _, _ := m.wantTraffic(clock.Now())
	m.applyTraffic(gen, nil, errTrafficUnsupported)
	pressKey(m, 'w')
	if sr := m.series(); sr.zoom {
		t.Fatal("zoom needs the live counters; without them the graphs stay on the provider's history")
	}
}

// --- runtime internals -----------------------------------------------------

func fakeInternals(sec int64, goroutines, cycles, alloc uint64) *NodeInternals {
	const mib = 1 << 20
	return &NodeInternals{
		AtUnixNano: sec * int64(time.Second), Goroutines: goroutines,
		HeapObjectsBytes: 1800 * mib, HeapStacksBytes: 196 * mib, HeapGoalBytes: 2100 * mib, GOGC: 100,
		GCCycles: cycles, AllocBytes: alloc,
		IntervalSeconds: 1, GCPauseP99Ms: 1.2, SchedLatP99Ms: 0.4, GCCPUFraction: 0.031,
	}
}

func TestTopInternalsPollCadence(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	gen, _, ok := m.wantInternals(clock.Now())
	if !ok {
		t.Fatal("first read not requested")
	}
	if _, _, ok := m.wantInternals(clock.Now()); ok {
		t.Fatal("a read is in flight: no second one")
	}
	m.applyInternals(gen, fakeInternals(1, 100, 1, 1), nil)
	clock.Advance(500 * time.Millisecond)
	if _, _, ok := m.wantInternals(clock.Now()); ok {
		t.Fatal("internals are read at most once a second even at a 100ms poll")
	}
	clock.Advance(600 * time.Millisecond)
	if _, _, ok := m.wantInternals(clock.Now()); !ok {
		t.Fatal("a second passed: read again")
	}
}

func TestTopInternalsNotAskedWhileDisconnected(t *testing.T) {
	m, clock := liveModel(t, time.Second)
	m.lost(clock.Now(), errors.New("gone"))
	if _, _, ok := m.wantInternals(clock.Now().Add(time.Hour)); ok {
		t.Fatal("no runtime read against a provider that is not answering")
	}
}

func TestTopInternalsUnsupportedProviderDropsThePanel(t *testing.T) {
	m, clock := liveModel(t, time.Second)
	gen, _, _ := m.wantInternals(clock.Now())
	m.applyInternals(gen, fakeInternals(1, 100, 1, 1), nil)
	if !m.hasInternals() {
		t.Fatal("reading not applied")
	}
	pressKey(m, 'g')
	clock.Advance(2 * time.Second)
	gen, _, _ = m.wantInternals(clock.Now())
	m.applyInternals(gen, nil, errRuntimeViewUnsupported)
	if m.hasInternals() || m.rt.showing {
		t.Fatal("a provider that does not know the commands must lose the panel and the list")
	}
	clock.Advance(time.Hour)
	if _, _, ok := m.wantInternals(clock.Now()); ok {
		t.Fatal("never ask again once the provider said unknown command")
	}
	pressKey(m, 'g')
	if m.rt.showing {
		t.Fatal("g must do nothing where the provider cannot answer")
	}
}

func TestTopInternalsTransientErrorKeepsTheLastReading(t *testing.T) {
	m, clock := liveModel(t, time.Second)
	gen, _, _ := m.wantInternals(clock.Now())
	m.applyInternals(gen, fakeInternals(1, 100, 1, 1), nil)
	clock.Advance(2 * time.Second)
	gen, _, _ = m.wantInternals(clock.Now())
	m.applyInternals(gen, nil, errors.New("timeout"))
	if !m.hasInternals() || m.rt.busy {
		t.Fatal("one missed read must not drop the panel or wedge the poll")
	}
}

func TestTopInternalsStaleGenerationIgnored(t *testing.T) {
	m, clock := liveModel(t, time.Second)
	m.providers = topProviders(2)
	gen, _, _ := m.wantInternals(clock.Now())
	m.selectProvider(1)
	m.applyInternals(gen, fakeInternals(1, 100, 1, 1), nil)
	if m.rt.cur != nil {
		t.Fatal("a reading for the previous provider was applied")
	}
}

func TestTopInternalsHistoryIsBounded(t *testing.T) {
	m, _ := liveModel(t, time.Second)
	for i := 0; i < topRuntimeHistory+50; i++ {
		m.applyInternals(m.gen, fakeInternals(int64(i), uint64(1000+i), 1, 1), nil)
	}
	if n := len(m.rt.gor); n != topRuntimeHistory {
		t.Fatalf("goroutine trend holds %d readings, want %d", n, topRuntimeHistory)
	}
	if last := m.rt.gor[len(m.rt.gor)-1]; last != float64(1000+topRuntimeHistory+49) {
		t.Fatalf("newest reading = %v", last)
	}
}

func TestTopInternalsRatesFromCounters(t *testing.T) {
	m, _ := liveModel(t, time.Second)
	const mib = 1 << 20
	m.applyInternals(m.gen, fakeInternals(10, 100, 5, 100*mib), nil)
	if _, ok := m.rt.internalsRate(func(n *NodeInternals) uint64 { return n.AllocBytes }); ok {
		t.Fatal("one reading has no rate")
	}
	m.applyInternals(m.gen, fakeInternals(12, 100, 9, 700*mib), nil)
	alloc, ok := m.rt.internalsRate(func(n *NodeInternals) uint64 { return n.AllocBytes })
	if !ok || alloc != 300*mib {
		t.Fatalf("alloc rate = %v %v, want 300 MiB/s", alloc, ok)
	}
	gc, _ := m.rt.internalsRate(func(n *NodeInternals) uint64 { return n.GCCycles })
	if gc != 2 {
		t.Fatalf("gc rate = %v/s, want 2", gc)
	}
	// A counter that went backwards (the provider restarted between reads) is
	// no rate, not a wrapped-around huge one.
	m.applyInternals(m.gen, fakeInternals(13, 100, 1, 1), nil)
	if _, ok := m.rt.internalsRate(func(n *NodeInternals) uint64 { return n.AllocBytes }); ok {
		t.Fatal("a backwards counter must give no rate")
	}
}

// --- goroutine list --------------------------------------------------------

func TestTopGroupsOnlyFetchedWhileShownAndRateLimited(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	m.applyInternals(m.gen, fakeInternals(1, 100, 1, 1), nil)
	if _, _, ok := m.wantGroups(clock.Now()); ok {
		t.Fatal("the profile must not be taken while the list is hidden")
	}
	pressKey(m, 'g')
	gen, _, ok := m.wantGroups(clock.Now())
	if !ok {
		t.Fatal("list shown: fetch it at once")
	}
	if _, _, ok := m.wantGroups(clock.Now()); ok {
		t.Fatal("one in flight")
	}
	m.applyGroups(gen, &GoroutineGroups{Total: 10, Groups: []GoroutineGroup{{"a.b", 10}}}, nil)
	clock.Advance(topGroupsEvery - time.Second)
	if _, _, ok := m.wantGroups(clock.Now()); ok {
		t.Fatal("re-asked inside the period")
	}
	clock.Advance(2 * time.Second)
	if _, _, ok := m.wantGroups(clock.Now()); !ok {
		t.Fatal("not re-asked after the period")
	}
	pressKey(m, 'g') // hide
	clock.Advance(time.Minute)
	m.rt.gBusy = false
	if _, _, ok := m.wantGroups(clock.Now()); ok {
		t.Fatal("hidden again: stop taking the profile")
	}
}

func TestTopGroupsSurviveProviderSwitchChoiceButNotData(t *testing.T) {
	m, _ := liveModel(t, time.Second)
	m.providers = topProviders(2)
	m.applyInternals(m.gen, fakeInternals(1, 100, 1, 1), nil)
	pressKey(m, 'g')
	m.applyGroups(m.gen, &GoroutineGroups{Total: 1, Groups: []GoroutineGroup{{"x.y", 1}}}, nil)
	m.selectProvider(1)
	if !m.rt.showing {
		t.Fatal("the view choice is the user's and stays")
	}
	if m.rt.groups != nil || m.rt.cur != nil {
		t.Fatal("the other provider's goroutines must not stay on screen")
	}
}

func TestShortFunc(t *testing.T) {
	cases := map[string]string{
		"github.com/urnetwork/connect.(*Client).run": "Client.run",
		"net/http.(*persistConn).readLoop":           "http.persistConn.readLoop",
		"main.main":                                  "main.main",
		"runtime.gopark":                             "runtime.gopark",
	}
	for in, want := range cases {
		if got := shortFunc(in); got != want {
			t.Errorf("shortFunc(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- screens ---------------------------------------------------------------

func internalsModel(t *testing.T, clock *fakeClock) *topModel {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	const mib = 1 << 20
	m.applyInternals(0, fakeInternals(100, 1150, 40, 1000*mib), nil)
	m.applyInternals(0, fakeInternals(101, 1168, 41, 1312*mib), nil)
	for i := 0; i < 30; i++ { // a rising trend for the sparkline
		m.rt.gor = append(m.rt.gor, float64(1000+i*6))
	}
	return m
}

func TestTopInternalsGoldenScreens(t *testing.T) {
	clock := &fakeClock{t: topBase}
	cases := []struct {
		name string
		w, h int
		set  func(m *topModel)
	}{
		{"top_internals_120x40", 120, 40, func(m *topModel) {}},
		{"top_goroutines_120x40", 120, 40, func(m *topModel) {
			pressKey(m, 'g')
			m.applyGroups(0, &GoroutineGroups{Total: 1168, Groups: []GoroutineGroup{
				{"github.com/urnetwork/connect.(*Client).run", 640},
				{"github.com/urnetwork/connect.(*Transfer).loop", 290},
				{"github.com/urnetwork/connect.(*ProxyConn).read", 140},
				{"net/http.(*persistConn).readLoop", 98},
			}}, nil)
		}},
		{"top_goroutines_collecting_120x40", 120, 40, func(m *topModel) { pressKey(m, 'g') }},
		// Too short for the third panel: Resources keeps the column, as before.
		{"top_internals_short_100x30", 100, 30, func(m *topModel) {}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock.t = topBase
			m := internalsModel(t, clock)
			c.set(m)
			got, _ := showOnSim(t, m, c.w, c.h)
			assertGolden(t, c.name, got)
			for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				if w := tui.StringWidth(line); w > c.w {
					t.Fatalf("row %d is %d cells in a %d wide screen", i, w, c.w)
				}
			}
		})
	}
}

func TestTopZoomGoldenScreen(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	// A ramp with a burst, so the zoom graph has a shape.
	for i := 0; i < 150; i++ {
		step := uint64(2_000_000 + i*20_000)
		if i > 90 && i < 110 {
			step *= 4
		}
		feedTraffic(t, m, clock, 1, step, &at, &bytes)
	}
	pressKey(m, 'w')
	got, _ := showOnSim(t, m, 120, 40)
	assertGolden(t, "top_zoom_120x40", got)
}

// The Internals panel is one more section on the screen; it must not make the
// screen disagree with itself when the terminal changes shape.
func TestTopInternalsNeverOverflowsAnySize(t *testing.T) {
	clock := &fakeClock{t: topBase}
	for _, w := range []int{72, 80, 100, 140} {
		for h := 20; h <= 50; h += 3 {
			t.Run(fmt.Sprintf("%dx%d", w, h), func(t *testing.T) {
				m := internalsModel(t, clock)
				pressKey(m, 'g')
				m.applyGroups(0, &GoroutineGroups{Total: 50, Groups: []GoroutineGroup{
					{"github.com/urnetwork/connect.(*AVeryLongTypeNameIndeedForAGoroutineOwner).someLongMethodName", 50},
				}}, nil)
				got, _ := showOnSim(t, m, w, h)
				lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
				if len(lines) > h {
					t.Fatalf("%d rows on a %d tall screen", len(lines), h)
				}
				for i, line := range lines {
					if lw := tui.StringWidth(line); lw > w {
						t.Fatalf("row %d is %d cells wide on a %d wide screen", i, lw, w)
					}
				}
			})
		}
	}
}
