package connect

import (
	"strings"
	"testing"
)

// feed runs one round of stats per outstanding value for a single pool/tag and
// returns the summary of the last round.
func feed(w *poolWatch, poolSize, tag int, caller string, outstanding []int64, taken0 uint64) poolSummary {
	var s poolSummary
	for i, o := range outstanding {
		taken := taken0 + uint64(i)*1000
		s = w.observe([]poolTagStat{{PoolSize: poolSize, Tag: tag, Caller: caller, Taken: taken + uint64(o), Returned: taken, Created: 1}})
	}
	return s
}

// The pool dump prints returned/taken. A low return percentage is NOT a leak:
// buffers in flight when the dump runs count as taken but not yet returned.
// These are the real outstanding counts (taken minus returned) from a
// production node, one per minute.
func TestPoolSummary_RealSeriesArePlateausNotLeaks(t *testing.T) {
	tag147 := []int64{648, 649, 650, 654, 651, 651, 653, 634, 640, 650} // a steady ~650 held
	w := newPoolWatch()
	s := feed(w, 2048, 147, "transfer.go:3707/transfer.go:2887", tag147, 900)
	if !s.Warm || !s.Healthy() {
		t.Fatalf("a flat outstanding count read as a problem: %s", s)
	}
	if !strings.Contains(s.String(), "all clear") {
		t.Fatalf("summary = %q, want all clear", s)
	}
}

func TestPoolSummary_RampThenPlateauIsNotALeak(t *testing.T) {
	// tag=224 after a restart: outstanding climbs for a few minutes as the
	// working set fills, then flattens. Ten samples all non-decreasing except
	// the plateau wobble; it must not be flagged.
	ramp := []int64{8432, 10591, 13375, 15257, 15396, 15223, 15232, 15198, 15210, 15205}
	s := feed(newPoolWatch(), 2048, 224, "frame_protobuf.go:296/transfer.go:3522", ramp, 100000)
	if !s.Healthy() {
		t.Fatalf("ramp-then-plateau flagged: %s", s)
	}
}

func TestPoolSummary_SteadyGrowthIsAPossibleLeak(t *testing.T) {
	leak := []int64{1000, 1100, 1200, 1300, 1400, 1500, 1600, 1700, 1800, 1900}
	s := feed(newPoolWatch(), 4096, 9, "ip.go:100/ip.go:200", leak, 5000)
	if s.Healthy() || len(s.Growing) != 1 {
		t.Fatalf("a steady leak was not flagged: %s", s)
	}
	f := s.Growing[0]
	if f.PoolSize != 4096 || f.Tag != 9 || f.Caller != "ip.go:100/ip.go:200" {
		t.Fatalf("finding names the wrong culprit: %+v", f)
	}
	for _, want := range []string{"possible leak", "pool[4096] tag=9", "ip.go:100/ip.go:200", "1000 -> 1900", "10 dumps"} {
		if !strings.Contains(s.String(), want) {
			t.Fatalf("summary %q is missing %q", s, want)
		}
	}
}

func TestPoolSummary_SlowSteadyLeakIsStillCaught(t *testing.T) {
	slow := []int64{500, 520, 540, 560, 580, 600, 620, 640, 660, 680} // +20 a dump
	if s := feed(newPoolWatch(), 2048, 3, "a.go:1/b.go:2", slow, 5000); s.Healthy() {
		t.Fatalf("a slow steady leak went unnoticed: %s", s)
	}
}

func TestPoolSummary_JitterAndTinyGrowthAreNotLeaks(t *testing.T) {
	jitter := []int64{300, 320, 290, 310, 305, 330, 295, 315, 300, 320}
	if s := feed(newPoolWatch(), 2048, 4, "a.go:1", jitter, 5000); !s.Healthy() {
		t.Fatalf("jitter flagged: %s", s)
	}
	tiny := []int64{10, 10, 11, 11, 12, 12, 13, 13, 14, 14}
	if s := feed(newPoolWatch(), 2048, 5, "a.go:1", tiny, 5000); !s.Healthy() {
		t.Fatalf("growth under the floor flagged: %s", s)
	}
}

func TestPoolSummary_WarmingUpNeedsAFullWindow(t *testing.T) {
	early := []int64{1000, 1500, 2000} // rising, but only 3 dumps of evidence
	s := feed(newPoolWatch(), 2048, 6, "a.go:1", early, 5000)
	if s.Warm || !s.Healthy() {
		t.Fatalf("judged a trend from 3 dumps: warm=%v %s", s.Warm, s)
	}
	if !strings.Contains(s.String(), "needs 10 dumps") || !strings.Contains(s.String(), "have 3") {
		t.Fatalf("warmup message = %q", s)
	}
}

func TestPoolSummary_LowReuseNeedsRealVolume(t *testing.T) {
	w := newPoolWatch()
	// 30% reuse over 50k takes is allocation churn worth a look.
	churn := poolTagStat{PoolSize: 16384, Tag: 8, Caller: "x.go:1/y.go:2", Taken: 50000, Returned: 50000, Created: 35000}
	// 0% reuse over one take is just a first allocation.
	first := poolTagStat{PoolSize: 4096, Tag: 0, Taken: 1, Returned: 1, Created: 1}
	s := w.observe([]poolTagStat{churn, first})
	if len(s.LowReuse) != 1 || s.LowReuse[0].Tag != 8 {
		t.Fatalf("low reuse findings = %+v", s.LowReuse)
	}
	if !strings.Contains(s.String(), "low reuse") || !strings.Contains(s.String(), "30%") {
		t.Fatalf("summary = %q", s)
	}
}

func TestPoolSummary_ReportsWhatIsHeld(t *testing.T) {
	s := newPoolWatch().observe([]poolTagStat{
		{PoolSize: 2048, Tag: 1, Taken: 1000, Returned: 500, Created: 1}, // 500 x 2 KiB
		{PoolSize: 65536, Tag: 2, Taken: 100, Returned: 84, Created: 1},  // 16 x 64 KiB
		{PoolSize: 4096, Tag: 3, Taken: 10, Returned: 12, Created: 1},    // returned > taken clamps to 0
	})
	if s.HeldBuffers != 516 || s.HeldBytes != 500*2048+16*65536 {
		t.Fatalf("held = %d buffers / %d bytes", s.HeldBuffers, s.HeldBytes)
	}
	if !strings.Contains(s.String(), "holding 2.0 MiB in 516 buffers") {
		t.Fatalf("summary = %q", s)
	}
}

func TestPoolSummary_CounterResetDoesNotCrashOrFlag(t *testing.T) {
	// ResetMessagePoolStats drops the counters to zero mid-history.
	w := newPoolWatch()
	feed(w, 2048, 7, "a.go:1", []int64{1000, 1100, 1200, 1300, 1400}, 5000)
	s := w.observe([]poolTagStat{{PoolSize: 2048, Tag: 7, Caller: "a.go:1", Taken: 10, Returned: 10, Created: 1}})
	if !s.Healthy() {
		t.Fatalf("counter reset flagged: %s", s)
	}
}

func TestPoolCallerLabelIsStable(t *testing.T) {
	// The label used to come out of a map in random order, so the same tag
	// flipped between two spellings from dump to dump and could not be grepped.
	for i := 0; i < 50; i++ {
		if got := poolCallerLabel(map[string]bool{"transfer.go:3522": true, "frame_protobuf.go:296": true}); got != "frame_protobuf.go:296/transfer.go:3522" {
			t.Fatalf("label = %q", got)
		}
	}
}
