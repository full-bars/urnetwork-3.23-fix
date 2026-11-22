package urnettools

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestTopTrafficHistoryTimeSlots(t *testing.T) {
	var h topTrafficHistory
	base := int64(time.Minute)
	h.push(base, 1, 2)
	h.push(base+int64(100*time.Millisecond), 3, 4)
	if len(h.billable) != 1 || h.billable[0] != 3 {
		t.Fatalf("same second: %v", h.billable)
	}
	h.push(base+int64(3*time.Second), 5, 6)
	if len(h.billable) != 4 || !math.IsNaN(h.billable[1]) || !math.IsNaN(h.billable[2]) || h.billable[3] != 5 {
		t.Fatalf("gap: %v", h.billable)
	}
	h.push(base+int64(1000*time.Second), 7, 8)
	if len(h.billable) != topGraphCapacity || !math.IsNaN(h.billable[0]) || h.billable[len(h.billable)-1] != 7 {
		t.Fatalf("long gap: length %d, first %v, last %v", len(h.billable), h.billable[0], h.billable[len(h.billable)-1])
	}
}

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

func TestTopGraphWindowPlacesReadingsOnATimeAxis(t *testing.T) {
	for _, interval := range topIntervalSteps {
		if interval >= 10*time.Second {
			continue // the longest level is the shortest span a reading can be placed on
		}
		t.Run(interval.String(), func(t *testing.T) {
			base := int64(time.Minute)
			at := []int64{base, base + int64(interval)}
			spans := []time.Duration{interval, interval}
			vals := []float64{1, 3}
			const n = 100 // 10s of 100ms columns
			got := spreadSeries(at, spans, vals, at[len(at)-1], 10*time.Second, n)
			if got[n-1] != 3 {
				t.Fatalf("newest column = %v, want the newest reading", got[n-1])
			}
			// The older reading ends where the newer one starts, so the switch
			// sits at the column of that instant.
			split := int((10*time.Second - interval) / topMinInterval)
			if split < 1 || split >= n {
				t.Fatalf("split column %d of %d", split, n)
			}
			if got[split] != 3 || got[split-1] != 1 {
				t.Fatalf("columns %d and %d are %v and %v, want the two readings either side of %v",
					split-1, split, got[split-1], got[split], interval)
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

func TestTopIntervalChangePreservesGraphWindowHistory(t *testing.T) {
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
			before := len(m.winRing.at)
			if before == 0 {
				t.Fatal("graph window history is empty before the interval change")
			}
			if interval == topIntervalSteps[len(topIntervalSteps)-1] {
				m.stepInterval(-1)
				m.stepInterval(1)
			} else {
				m.stepInterval(1)
				m.stepInterval(-1)
			}
			if got := len(m.winRing.at); got != before {
				t.Fatalf("graph window history after interval changes has %d readings, want %d", got, before)
			}
		})
	}
}
