package urnettools

import (
	"testing"
	"time"
)

func TestTopTrafficRateRestartsAfterLongGap(t *testing.T) {
	m, clock := liveModel(t, time.Second)
	for i, delay := range []time.Duration{time.Second, time.Second, time.Minute} {
		clock.Advance(delay)
		gen, _, ok := m.wantTraffic(clock.Now())
		if !ok {
			t.Fatal("traffic poll was not requested")
		}
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 2000}, nil)
		_, _, available := m.live.rates()
		if available != (i == 1) {
			t.Fatalf("reading %d: rate available = %v", i, available)
		}
	}
}

func TestTopIntervalChangePreservesZoomHistory(t *testing.T) {
	for _, interval := range topIntervalSteps {
		t.Run(interval.String(), func(t *testing.T) {
			m, clock := liveModel(t, interval)
			for i := 0; i < 4; i++ {
				clock.Advance(interval)
				gen, _, ok := m.wantTraffic(clock.Now())
				if !ok {
					t.Fatal("traffic poll was not requested")
				}
				m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 2000}, nil)
			}
			before := len(m.zoom.billable)
			if before == 0 {
				t.Fatal("zoom history is empty before the interval change")
			}
			if interval == topIntervalSteps[len(topIntervalSteps)-1] {
				m.stepInterval(-1)
				m.stepInterval(1)
			} else {
				m.stepInterval(1)
				m.stepInterval(-1)
			}
			if got := len(m.zoom.billable); got != before {
				t.Fatalf("zoom history after interval changes has %d samples, want %d", got, before)
			}
		})
	}
}
