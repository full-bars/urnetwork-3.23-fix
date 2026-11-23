package urnettools

import (
	"math"
	"strings"
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
			zoom := len(m.winRing.at)
			for i := 0; i < 10; i++ {
				clock.Advance(interval)
				m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i+2) * 1000, TotalBytes: uint64(i+2) * 2000}, nil)
				m.applyInternals(m.gen, &NodeInternals{AtUnixNano: clock.Now().UnixNano(), Goroutines: uint64(i + 1)}, nil)
			}
			if got := len(m.trafficHistory.billable) - before; got != 10 {
				t.Fatalf("traffic added %d columns", got)
			}
			if got := len(m.winRing.at) - zoom; got != 10 {
				t.Fatalf("the graph window added %d readings", got)
			}
			if got := len(m.rt.gor); got != 10 {
				t.Fatalf("goroutine trend added %d columns", got)
			}
			sr := m.series()
			if !sr.window || len(sr.billable) != len(sr.total) {
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
	// The window ring keeps the same spans the traffic history kept, so a level
	// change with - and + does not rewrite the time axis of what is already
	// drawn.
	r := &m.winRing
	if len(r.spans) == 0 || r.spans[len(r.spans)-1] != time.Second {
		t.Fatalf("the graph window's newest span is %v, want the one second reading", r.spans)
	}
	for i := 1; i < len(r.spans)-1; i++ {
		if r.spans[i] > time.Second {
			t.Fatalf("reading %d spans %v after the interval change", i, r.spans[i])
		}
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
	before := m.winScaleB[topGraphWindowDefault].top
	clock.Advance(100 * time.Millisecond)
	bytes += 1100
	m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: bytes, TotalBytes: bytes}, nil)
	if m.winScaleB[topGraphWindowDefault].top != before {
		t.Fatalf("small spike changed scale from %v to %v", before, m.winScaleB[topGraphWindowDefault].top)
	}
}

func TestTopGraphRefreshSlowIntervalKeepsSecondSlots(t *testing.T) {
	m, clock := liveModel(t, 5*time.Second)
	for i := 0; i < 3; i++ {
		clock.Advance(5 * time.Second)
		m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i) * 1000, TotalBytes: uint64(i) * 2000}, nil)
	}
	h := m.trafficHistory
	n := len(h.billable)
	if n < 6 {
		t.Fatalf("only %d slots", n)
	}
	for _, v := range h.billable[n-5 : n-1] {
		if !math.IsNaN(v) {
			t.Fatalf("missing second has value %v", v)
		}
	}
	if math.IsNaN(h.billable[n-1]) {
		t.Fatal("latest slot is missing")
	}
}

func TestTopGraphRefreshFrames(t *testing.T) {
	for _, interval := range []time.Duration{100 * time.Millisecond, time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			m, clock := liveModel(t, interval)
			for i := 0; i < 25; i++ {
				clock.Advance(interval)
				m.applyTraffic(m.gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: uint64(i*i) * 1000, TotalBytes: uint64(i*i) * 2000}, nil)
			}
			frame, _ := showOnSim(t, m, 100, 30)
			if !strings.Contains(frame, "Billable,") || !strings.Contains(frame, "Total traffic,") {
				t.Fatalf("missing graph titles:\n%s", frame)
			}
			t.Logf("frame at %v:\n%s", interval, frame)
		})
	}
}
