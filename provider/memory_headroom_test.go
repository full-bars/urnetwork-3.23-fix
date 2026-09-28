package main

import (
	"strings"
	"testing"
)

// A 2 GiB box's free memory dipped to 132 MB at 590 connected clients and nothing said
// so: the pressure score read a fixed 1.00 all day, so it carried no signal. A
// headroom tracker on real free memory logs when the box gets short and again
// when it recovers, with hysteresis so a reading that hovers at the line does
// not spam the log.
func TestHeadroomLowThreshold(t *testing.T) {
	cases := map[int64]int64{
		512:   150, // tiny box: the absolute floor
		963:   150, // 1 GB box
		1930:  193, // 2 GiB box: 10% of RAM
		4000:  400, // capped
		24000: 400,
	}
	for ram, want := range cases {
		if got := headroomLowThresholdMiB(ram); got != want {
			t.Errorf("RAM %d MiB: threshold %d, want %d", ram, got, want)
		}
	}
}

func TestHeadroomTrackerEntersAfterTwoSamplesAndLeavesAfterFourWithHysteresis(t *testing.T) {
	var h headroomTracker
	const low = int64(193)
	observe := func(avail int64) string { return h.Observe(avail, low) }

	if got := observe(600); got != "" {
		t.Fatalf("healthy: %q", got)
	}
	if got := observe(142); got != "" {
		t.Fatalf("one low sample must not trigger: %q", got)
	}
	if got := observe(150); got != "low" {
		t.Fatalf("second consecutive low sample must trigger, got %q", got)
	}
	if got := observe(120); got != "" {
		t.Fatalf("already low: must not re-announce, got %q", got)
	}
	// Recovery needs to clear the threshold by 25% (241 MiB) four times running.
	for i, avail := range []int64{230, 300, 300, 300} {
		if got := observe(avail); got != "" {
			t.Fatalf("recovery sample %d (%d MiB) announced early: %q", i, avail, got)
		}
	}
	// 230 MiB is below the 241 MiB recovery line and resets the streak; the four
	// consecutive readings that follow (300, 300, 300, then 320) are the clear run.
	if got := observe(320); got != "recovered" {
		t.Fatalf("fourth consecutive clear sample must announce recovery, got %q", got)
	}
	if h.low {
		t.Fatalf("tracker must be back to normal")
	}
}

func TestHeadroomTrackerDoesNotFlapAtTheLine(t *testing.T) {
	var h headroomTracker
	const low = int64(200)
	transitions := 0
	// Hovering across the threshold but never clearing it by the recovery margin.
	for i := 0; i < 60; i++ {
		avail := int64(190)
		if i%2 == 1 {
			avail = 210
		}
		if h.Observe(avail, low) != "" {
			transitions++
		}
	}
	if transitions > 1 {
		t.Fatalf("hovering at the line produced %d transitions, want at most the single entry", transitions)
	}
}

func TestHeadroomTrackerIgnoresUnknownReadings(t *testing.T) {
	var h headroomTracker
	h.Observe(100, 193)
	for i := 0; i < 10; i++ {
		if got := h.Observe(-1, 193); got != "" {
			t.Fatalf("unknown reading produced %q", got)
		}
	}
	if got := h.Observe(100, 193); got != "low" {
		t.Fatalf("an unknown reading must not reset the streak, got %q", got)
	}
}

func TestHeadroomLogLine(t *testing.T) {
	low := headroomLogLine("low", 132, 193, 2001, 77780)
	for _, want := range []string{"[proxy][resources]", "low memory headroom", "132 MiB available", "below 193 MiB", "2001 proxies", "77780 goroutines"} {
		if !strings.Contains(low, want) {
			t.Errorf("low line %q missing %q", low, want)
		}
	}
	rec := headroomLogLine("recovered", 420, 193, 2001, 41000)
	if !strings.Contains(rec, "memory headroom recovered") || !strings.Contains(rec, "420 MiB available") {
		t.Errorf("recovered line: %q", rec)
	}
	if headroomLogLine("", 1, 1, 1, 1) != "" {
		t.Errorf("no transition must produce no line")
	}
}
