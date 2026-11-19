package urnettools

import (
	"testing"
	"time"
)

func TestTopGraphRefreshColumns(t *testing.T) {
	for _, interval := range []time.Duration{100 * time.Millisecond, time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			m, clock := liveModel(t, interval)
			for i := 0; i < 2; i++ {
				clock.Advance(interval)
				m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 2000}, nil)
			}
			before := len(m.trafficHistory.billable)
			zoom := len(m.zoom.billable)
			for i := 0; i < 10; i++ {
				clock.Advance(interval)
				m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i+2) * 1000, TotalBytes: uint64(i+2) * 2000}, nil)
				m.applyInternals(m.gen, &NodeInternals{AtUnixNano: clock.Now().UnixNano(), Goroutines: uint64(i + 1)}, nil)
			}
			if got := len(m.trafficHistory.billable) - before; got != 10 {
				t.Fatalf("traffic added %d columns", got)
			}
			if got := len(m.zoom.billable) - zoom; got != 10 {
				t.Fatalf("zoom added %d columns", got)
			}
			if got := len(m.rt.gor); got != 10 {
				t.Fatalf("goroutine trend added %d columns", got)
			}
			sr := m.series()
			if !sr.columns || len(sr.billable) != len(sr.total) {
				t.Fatal("traffic series is not paired columns")
			}
		})
	}
}

func TestTopGraphRefreshSpansAndLabels(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	for i := 0; i < 11; i++ {
		clock.Advance(100 * time.Millisecond)
		m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 2000}, nil)
	}
	h := &m.trafficHistory
	spans := append([]time.Duration(nil), h.spans...)
	m.interval = time.Second
	clock.Advance(time.Second)
	m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: 11000, TotalBytes: 22000}, nil)
	for i, span := range spans {
		if h.spans[i] != span {
			t.Fatal("interval change altered retained span")
		}
	}
	if h.spans[len(h.spans)-1] != time.Second {
		t.Fatal("new span is not one second")
	}
	sr := topSeries{columns: true, spans: []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond}, billable: make([]float64, 10)}
	if title := m.graphTitle("Billable", sr, sr.billable, nil, 80); title != "Billable, 1s" {
		t.Fatalf("fast title %q", title)
	}
	for i := range sr.spans {
		sr.spans[i] = time.Second
	}
	if title := m.graphTitle("Billable", sr, sr.billable, nil, 80); title != "Billable, 10s" {
		t.Fatalf("one second title %q", title)
	}
}

func TestTopGraphRefreshScaleSmoothsSingleSpike(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var bytes uint64
	for i := 0; i < 15; i++ {
		clock.Advance(100 * time.Millisecond)
		bytes += 1000
		m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: bytes, TotalBytes: bytes}, nil)
	}
	before := m.zoomScaleB.top
	clock.Advance(100 * time.Millisecond)
	bytes += 1100
	m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: bytes, TotalBytes: bytes}, nil)
	if m.zoomScaleB.top != before {
		t.Fatalf("small spike changed scale from %v to %v", before, m.zoomScaleB.top)
	}
}
