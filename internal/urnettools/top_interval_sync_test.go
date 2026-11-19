package urnettools

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestTopTenMinuteHistoryAdvancesWithTrafficPolls(t *testing.T) {
	for _, interval := range topIntervalSteps {
		t.Run(interval.String(), func(t *testing.T) {
			m, clock := liveModel(t, interval)
			for i := 0; i < 3; i++ {
				clock.Advance(interval)
				gen, _, ok := m.wantTraffic(clock.Now())
				if !ok {
					t.Fatal("traffic poll was not requested")
				}
				m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i*i) * 1000, TotalBytes: uint64(i*i) * 2000}, nil)
			}
			before := append([]float64(nil), m.series().billable...)
			for elapsed, i := time.Duration(0), uint64(1); elapsed < 20*time.Second; elapsed, i = elapsed+interval, i+1 {
				clock.Advance(interval)
				gen, _, ok := m.wantTraffic(clock.Now())
				if !ok {
					t.Fatal("traffic poll was not requested")
				}
				m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: 4000 + i*1000, TotalBytes: 8000 + i*2000}, nil)
			}
			if reflect.DeepEqual(before, m.series().billable) {
				t.Fatal("ten minute graph history did not advance with the traffic poll")
			}
		})
	}
}

func TestTopZoomGridUsesSampleTimes(t *testing.T) {
	for _, interval := range topIntervalSteps {
		t.Run(interval.String(), func(t *testing.T) {
			var z topZoom
			base := int64(time.Minute)
			z.push(base, 1, 2)
			z.push(base+int64(interval), 3, 4)
			b, total, anchor := z.grid()
			if anchor != (base+int64(interval))/int64(topMinInterval) || len(b) != 150 {
				t.Fatalf("anchor=%d length=%d", anchor, len(b))
			}
			if b[149] != 3 || total[149] != 4 {
				t.Fatalf("newest values %v %v", b[149], total[149])
			}
			old := 149 - int(interval/topMinInterval)
			if old >= 0 && b[old] != 1 {
				t.Fatalf("older value at %d = %v", old, b[old])
			}
			if interval > topMinInterval && !math.IsNaN(b[148]) {
				t.Fatalf("missing time slot = %v", b[148])
			}
		})
	}
}

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
