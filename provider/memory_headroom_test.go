package main

import (
	"strings"
	"sync"
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
		1499:  150, // 10% is still under the floor
		1500:  150, // exactly the floor
		1509:  150, // 10% has not cleared the floor yet
		1510:  151, // first value where 10% clears the floor: 1510/10 = 151
		1930:  193, // 2 GiB box: 10% of RAM
		3999:  399, // just under the cap's 10% line
		4000:  400, // exactly the cap
		4009:  400, // 10% is 400: still at the cap, nothing clamped
		4010:  400, // first value where 10% (401) is clamped back to the cap
		24000: 400,
	}
	for ram, want := range cases {
		if got := headroomLowThresholdMiB(ram); got != want {
			t.Errorf("RAM %d MiB: threshold %d, want %d", ram, got, want)
		}
	}
}

// TestHeadroomLowForRAMLimitBranchesOnTheSource pins the whole contract: an
// unreadable RAM limit must take the 400 MiB cap, never the 150 MiB floor, while
// a real host reading of exactly the fallback's 850 MiB must still be treated as
// a reading. connect reports the fallback through EffectiveRAMLimitKnown's bool,
// so the byte count is never what decides this.
//
// The readable cases are the same arithmetic as before, at the same boundaries,
// in bytes rather than MiB.
func TestHeadroomLowForRAMLimitBranchesOnTheSource(t *testing.T) {
	// connect's placeholder when host memory is unreadable.
	const fallbackBytes = 850 * 1024 * 1024
	cases := []struct {
		name     string
		ramBytes int64
		ramKnown bool
		want     int64
		wantNote bool
	}{
		{"unreadable, the 850 MiB placeholder", fallbackBytes, false, headroomLowCapMiB, true},
		{"real 850 MiB host, same bytes", fallbackBytes, true, headroomLowFloorMiB, false},
		{"unreadable, zero", 0, false, headroomLowCapMiB, true},
		{"1499 MiB", 1499 << 20, true, 150, false},
		{"1509 MiB", 1509 << 20, true, 150, false},
		{"1510 MiB", 1510 << 20, true, 151, false},
		{"1 GiB box", 1 << 30, true, 150, false},
		{"2 GiB box", 2 << 30, true, 204, false}, // 2048 MiB: 10%
		{"3999 MiB", 3999 << 20, true, 399, false},
		{"4000 MiB", 4000 << 20, true, 400, false},
		{"4009 MiB", 4009 << 20, true, 400, false},
		{"4010 MiB", 4010 << 20, true, 400, false},
		{"8 GiB box", 8 << 30, true, 400, false},
	}
	for _, c := range cases {
		got, note := headroomLowForRAMLimit(c.ramBytes, c.ramKnown)
		if got != c.want {
			t.Errorf("%s: %d bytes (known=%v) gave %d MiB, want %d", c.name, c.ramBytes, c.ramKnown, got, c.want)
		}
		if (note != "") != c.wantNote {
			t.Errorf("%s: note %q, want note=%v", c.name, note, c.wantNote)
		}
	}
}

// TestHeadroomStartupNote pins the unexplained-threshold line: it is emitted
// only when the RAM limit was not a real reading, and it names the threshold
// actually in force (the cap).
func TestHeadroomStartupNote(t *testing.T) {
	low, note := headroomLowForRAMLimit(850*1024*1024, false)
	if low != headroomLowCapMiB {
		t.Fatalf("unreadable RAM threshold %d, want the cap %d", low, headroomLowCapMiB)
	}
	for _, want := range []string{"[proxy][resources]", "effective RAM unreadable", "400 MiB", "using the cap"} {
		if !strings.Contains(note, want) {
			t.Errorf("unreadable-RAM note %q missing %q", note, want)
		}
	}
	if !strings.HasSuffix(note, "\n") {
		t.Errorf("note must end in a newline: %q", note)
	}
	for _, ramBytes := range []int64{1 << 30, 2 << 30, 8 << 30} {
		if _, got := headroomLowForRAMLimit(ramBytes, true); got != "" {
			t.Errorf("readable RAM %d produced a note: %q", ramBytes, got)
		}
	}
}

// TestHeadroomAtStartupBranchesOnDetection checks the production wiring: a failed
// detection (reported as unknown) must reach the cap and the note, and a real
// reading of the same 850 MiB must not be mistaken for the fallback.
func TestHeadroomAtStartupBranchesOnDetection(t *testing.T) {
	restore := effectiveRAMLimit
	t.Cleanup(func() { effectiveRAMLimit = restore })

	effectiveRAMLimit = func() (int64, bool) { return 850 * 1024 * 1024, false }
	if low, note := headroomAtStartup(); low != headroomLowCapMiB || note == "" {
		t.Fatalf("unreadable RAM: threshold %d, note %q; want cap %d and a note", low, note, headroomLowCapMiB)
	}

	effectiveRAMLimit = func() (int64, bool) { return 850 * 1024 * 1024, true }
	if low, note := headroomAtStartup(); low != headroomLowFloorMiB || note != "" {
		t.Fatalf("real 850 MiB host: threshold %d, note %q; want floor %d and no note", low, note, headroomLowFloorMiB)
	}
}

// TestHeadroomStartupNoteFiresOnceAcrossRestarts covers superviseLoop restarting
// runPressureMonitor: the threshold is resolved again on every start, but the
// note must still reach the persisted log exactly once per process.
func TestHeadroomStartupNoteFiresOnceAcrossRestarts(t *testing.T) {
	oldLogf := importantLogHook
	restore := effectiveRAMLimit
	headroomStartupOnce = sync.Once{}
	var lines []string
	importantLogHook = func(line string) {
		lines = append(lines, line)
	}
	effectiveRAMLimit = func() (int64, bool) { return 850 * 1024 * 1024, false }
	t.Cleanup(func() {
		importantLogHook = oldLogf
		headroomStartupOnce = sync.Once{}
		effectiveRAMLimit = restore
	})

	for i := 0; i < 5; i++ {
		_, note := headroomAtStartup()
		logHeadroomStartupNote(note)
	}
	if len(lines) != 1 {
		t.Fatalf("note logged %d times across restarts, want once: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "using the cap") {
		t.Errorf("logged line %q does not explain the cap", lines[0])
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
