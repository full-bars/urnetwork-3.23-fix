package urnettools

import (
	"testing"
	"time"
)

// TestUsageLifetime verifies lifetime is segment-summed across cumulative
// snapshots, so post-drop growth that never re-exceeds a pre-drop peak (from
// proxy churn or a restart) is NOT lost to a naive running max.
func TestUsageLifetime(t *testing.T) {
	snaps := []usageSnapshot{
		{TS: time.Now().Add(-3 * time.Hour), RX: 100, TX: 90, BillableRX: 95, BillableTX: 85},
		{TS: time.Now().Add(-2 * time.Hour), RX: 500, TX: 400, BillableRX: 480, BillableTX: 380},
		{TS: time.Now().Add(-time.Hour), RX: 200, TX: 150, BillableRX: 190, BillableTX: 140}, // mid lower (proxy removed)
		{TS: time.Now(), RX: 700, TX: 600, BillableRX: 660, BillableTX: 560},
	}
	// Segment-sum: [0→{500,400}] flushed at the drop = RX+500 TX+400; then
	// [{200,150}→{700,600}] = RX+500 TX+450. Totals RX=1000 TX=850 → 1850.
	// Billable: [0→{480,380}]=+480/+380; [{190,140}→{660,560}]=+470/+420 → 1750.
	lt := usageLifetime(snaps)
	if lt.Total() != 1850 {
		t.Fatalf("lifetime total = %d, want 1850", lt.Total())
	}
	if lt.Billable() != 1750 {
		t.Fatalf("lifetime billable = %d, want 1750", lt.Billable())
	}
	if lt.Control() != 100 {
		t.Fatalf("lifetime control = %d, want 100", lt.Control())
	}
}

// TestUsageLifetimeAsymmetricDip is the H1 regression: a running-max would
// drop all post-dip growth that never re-exceeds the peak (44% undercount in
// the review); segment-sum must recover the pre-dip AND post-dip bytes.
func TestUsageLifetimeAsymmetricDip(t *testing.T) {
	base := time.Now()
	snaps := []usageSnapshot{
		{TS: base.Add(-3 * time.Hour), RX: 1000, TX: 0},
		{TS: base.Add(-2 * time.Hour), RX: 100, TX: 0}, // RX dropped (heavy-RX proxy removed)
		{TS: base.Add(-time.Hour), RX: 900, TX: 0},     // +800 growth, still < 1000 peak
	}
	lt := usageLifetime(snaps)
	// True bytes moved = 1000 (initial growth) + (900-100)=800 = 1800.
	if lt.Total() != 1800 {
		t.Fatalf("asymmetric-dip lifetime total = %d, want 1800 (running max would give 1000)", lt.Total())
	}
}

// TestUsageWindowAsymmetricMask is the H2 regression for usageWindow: when RX
// dips (heavy-RX proxy removed) but TX grows so the COMBINED sum keeps rising,
// a combined-sum-only restart check sees no drop and satSub floors the dipped
// RX field to 0 — the pre-drop RX peak is silently lost. Per-field detection
// must flag the RX dip as a segment boundary and preserve the peak.
// NOTE: a single-aggregate snapshot cannot reconstruct bytes of a REMOVED
// proxy plus ongoing TX (inherently lossy); this asserts the fix's provable
// improvement — the pre-drop RX peak (1000) is counted instead of being
// floored to the post-drop low (pre-fix combined-sum yields 900, losing it).
func TestUsageWindowAsymmetricMask(t *testing.T) {
	now := time.Now()
	snaps := []usageSnapshot{
		{TS: now.Add(-3 * time.Hour), RX: 500, TX: 0},
		{TS: now.Add(-2 * time.Hour), RX: 1000, TX: 0}, // RX peak (+500)
		{TS: now.Add(-time.Hour), RX: 200, TX: 900},    // RX dropped, TX soars → combined still rising
	}
	w := usageWindow(snaps, 24*time.Hour, now)
	// Segment: [0→{1000,0}] flushed at the RX drop = +1000 RX.
	if w.Total() != 1000 {
		t.Fatalf("asymmetric-mask window total = %d, want 1000 (pre-drop RX peak; combined-sum floor would give 900)", w.Total())
	}
}

// TestUsageWindow verifies the delta-based window math.
func TestUsageWindow(t *testing.T) {
	now := time.Now()
	snaps := []usageSnapshot{
		{TS: now.Add(-10 * 24 * time.Hour), RX: 1000, TX: 900, BillableRX: 950, BillableTX: 850}, // >7d ago → 7d base
		{TS: now.Add(-26 * time.Hour), RX: 2000, TX: 1800, BillableRX: 1900, BillableTX: 1700},
		{TS: now.Add(-2 * time.Hour), RX: 3000, TX: 2700, BillableRX: 2850, BillableTX: 2550},
	}
	// 24h window: reference=3000/2700 (newest), base=newest < 24h ago = 2000/1800
	// (the -26h snapshot). Delta = (3000+2700)-(2000+1800) = 1900.
	w := usageWindow(snaps, 24*time.Hour, now)
	if w.Total() != 1900 {
		t.Fatalf("24h window total = %d, want 1900", w.Total())
	}
	if w.Billable() != 1800 { // (2850+2550)-(1900+1700)=5400-3600=1800
		t.Fatalf("24h window billable = %d, want 1800", w.Billable())
	}
	// 7d window: base is newest < 7d ago = 1000/900 (the -10d snapshot).
	w7 := usageWindow(snaps, 7*24*time.Hour, now)
	if w7.Total() != 3800 { // (3000+2700)-(1000+900)=5700-1900=3800
		t.Fatalf("7d window total = %d, want 3800", w7.Total())
	}
}

// TestUsageWindowPredatesHistory: window before any snapshot → full life.
func TestUsageWindowNoBase(t *testing.T) {
	now := time.Now()
	snaps := []usageSnapshot{
		{TS: now.Add(-2 * time.Hour), RX: 300, TX: 200, BillableRX: 280, BillableTX: 190},
	}
	w := usageWindow(snaps, 24*time.Hour, now)
	if w.Total() != 500 {
		t.Fatalf("total = %d, want 500", w.Total())
	}
}

// TestDeltaBuckets: cumulative snapshots become per-bucket deltas.
func TestDeltaBuckets(t *testing.T) {
	now := time.Now()
	// Two day buckets: day0 (older) and day1 (newer).
	day0 := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	day1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	snaps := []usageSnapshot{
		{TS: day0, RX: 1000, TX: 900, BillableRX: 950, BillableTX: 850},                  // day0
		{TS: day0.Add(time.Hour), RX: 1200, TX: 1000, BillableRX: 1150, BillableTX: 950}, // day0 late
		{TS: day1, RX: 2000, TX: 1800, BillableRX: 1900, BillableTX: 1700},               // day1
	}
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}, 10, now)
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}
	// day0 moved: 1200+1000-0 = 2200 total (first bucket, base 0).
	if buckets[0].total != 2200 {
		t.Fatalf("day0 total = %d, want 2200", buckets[0].total)
	}
	if buckets[0].billable != 2100 {
		t.Fatalf("day0 billable = %d, want 2100", buckets[0].billable)
	}
	// day1 moved: 2000+1800 - (1200+1000) = 1600 total.
	if buckets[1].total != 1600 {
		t.Fatalf("day1 total = %d, want 1600", buckets[1].total)
	}
}

// TestDeltaBucketsRestartWithinBucket: a restart (cumulative drop) inside a
// single bucket must NOT lose the pre-restart bytes.
func TestDeltaBucketsRestartWithinBucket(t *testing.T) {
	now := time.Now()
	day0 := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	day1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	snaps := []usageSnapshot{
		// day0: steady traffic, then restart (cum drops), then post-restart traffic.
		{TS: day0, RX: 1000, TX: 2000, BillableRX: 900, BillableTX: 1800},                   // cum=3000
		{TS: day0.Add(time.Hour), RX: 200, TX: 300, BillableRX: 180, BillableTX: 270},       // cum=500 (restart!)
		{TS: day0.Add(2 * time.Hour), RX: 1200, TX: 800, BillableRX: 1100, BillableTX: 700}, // cum=2000 (post-restart)
		// day1: steady.
		{TS: day1, RX: 2500, TX: 1500, BillableRX: 2400, BillableTX: 1400}, // cum=4000
	}
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}, 10, now)
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}
	// day0: pre-restart 3000 + post-restart 1500 (500 baseline→2000) = 4500 total.
	if buckets[0].total != 4500 {
		t.Fatalf("day0 total = %d, want 4500", buckets[0].total)
	}
	// day0 billable: 2700 + (1800-450)=1350 = 4050
	if buckets[0].billable != 4050 {
		t.Fatalf("day0 billable = %d, want 4050", buckets[0].billable)
	}
	// day1: 4000 - 2000 = 2000 (delta from last snapshot of day0).
	if buckets[1].total != 2000 {
		t.Fatalf("day1 total = %d, want 2000", buckets[1].total)
	}
	if buckets[1].billable != 2000 { // (2400+1400)-(1100+700) = 3800-1800
		t.Fatalf("day1 billable = %d, want 2000", buckets[1].billable)
	}
}

// TestDeltaBucketsTwoRestartsWithinSameBucket is the regression for the
// CodeRabbit finding: bucketInfo tracked only ONE (restartCum,
// maxCumPostRestart) pair. A SECOND same-bucket restart overwrote that pair,
// silently discarding the growth of the first post-restart segment (between
// the two restarts). Realistic on repeated proxy trim/reload or reaper
// eviction landing within one hour bucket.
func TestDeltaBucketsTwoRestartsWithinSameBucket(t *testing.T) {
	now := time.Now()
	day0 := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	day1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	snaps := []usageSnapshot{
		{TS: day0, RX: 1000, TX: 2000, BillableRX: 900, BillableTX: 1800},                   // cum=3000 (seg1)
		{TS: day0.Add(time.Hour), RX: 200, TX: 300, BillableRX: 180, BillableTX: 270},       // cum=500 (restart 1)
		{TS: day0.Add(2 * time.Hour), RX: 1200, TX: 800, BillableRX: 1100, BillableTX: 700}, // cum=2000 (seg2 peak)
		{TS: day0.Add(3 * time.Hour), RX: 100, TX: 150, BillableRX: 90, BillableTX: 130},    // cum=250 (restart 2)
		{TS: day0.Add(4 * time.Hour), RX: 900, TX: 600, BillableRX: 800, BillableTX: 550},   // cum=1500 (seg3 peak)
		{TS: day1, RX: 2500, TX: 1500, BillableRX: 2400, BillableTX: 1400},                  // cum=4000
	}
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}, 10, now)
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}
	// day0 = seg1(3000-0) + seg2(2000-500=1500) + seg3(1500-250=1250) = 5750.
	// Before the fix, seg2's 1500 was silently discarded by the second
	// restart overwriting the tracked pair: total = 3000 + (1500-250) = 4250.
	if buckets[0].total != 5750 {
		t.Fatalf("day0 total = %d, want 5750 (bytes lost to second same-bucket restart, want %d got dropped)", buckets[0].total, 5750-buckets[0].total)
	}
	// day0 billable = seg1(2700-0) + seg2(1800-450=1350) + seg3(1350-220=1130) = 5180.
	if buckets[0].billable != 5180 {
		t.Fatalf("day0 billable = %d, want 5180", buckets[0].billable)
	}
	// day1 = 4000 - 1500 (day0's final post-restart max) = 2500.
	if buckets[1].total != 2500 {
		t.Fatalf("day1 total = %d, want 2500", buckets[1].total)
	}
	if buckets[1].billable != 2450 {
		t.Fatalf("day1 billable = %d, want 2450", buckets[1].billable)
	}
}

// TestBillableExceedsTotal verifies the M1 signal: when the independent
// billable (ip.go) and total (net.go) counters disagree, the mismatch is
// reported rather than silently floored by Control().
func TestBillableExceedsTotal(t *testing.T) {
	consistent := usageAggregates{TotalRX: 100, TotalTX: 100, BillableRX: 50, BillableTX: 50}
	if consistent.BillableExceedsTotal() {
		t.Fatal("consistent aggregates flagged as billable > total")
	}
	if consistent.Control() != 100 {
		t.Fatalf("consistent control = %d, want 100", consistent.Control())
	}

	inconsistent := usageAggregates{TotalRX: 100, TotalTX: 0, BillableRX: 50, BillableTX: 80}
	if !inconsistent.BillableExceedsTotal() {
		t.Fatal("billable (130) > total (100) not flagged")
	}
	// Control() floors at 0 regardless — BillableExceedsTotal is the only
	// signal a caller has that this floor is masking bad accounting.
	if inconsistent.Control() != 0 {
		t.Fatalf("inconsistent control = %d, want 0 (floored)", inconsistent.Control())
	}
}

// TestUsageLifetimeUnsortedInput verifies readUsageHistory's chronological
// sort guarantee (M2): usageLifetime must produce the same result whether
// snapshots arrive already sorted or out of order (e.g. combined rotated + main
// history files, or clock corrections). Passing unsorted input straight to
// the old (pre-fix) segment walk would misdetect restarts on the out-of-order
// rows and undercount.
func TestUsageLifetimeUnsortedInput(t *testing.T) {
	base := time.Now()
	sorted := []usageSnapshot{
		{TS: base.Add(-3 * time.Hour), RX: 100, TX: 0},
		{TS: base.Add(-2 * time.Hour), RX: 500, TX: 0},
		{TS: base.Add(-time.Hour), RX: 200, TX: 0}, // restart
		{TS: base, RX: 700, TX: 0},
	}
	shuffled := []usageSnapshot{sorted[2], sorted[0], sorted[3], sorted[1]}

	want := usageLifetime(sorted)
	got := usageLifetime(shuffled)
	if got.Total() != want.Total() {
		t.Fatalf("unsorted-input lifetime total = %d, want %d (chronological-sort input order sensitivity)", got.Total(), want.Total())
	}
}

// TestUsageWindowUnsortedInput is the usageWindow counterpart of
// TestUsageLifetimeUnsortedInput.
func TestUsageWindowUnsortedInput(t *testing.T) {
	now := time.Now()
	sorted := []usageSnapshot{
		{TS: now.Add(-10 * 24 * time.Hour), RX: 1000, TX: 900},
		{TS: now.Add(-26 * time.Hour), RX: 2000, TX: 1800},
		{TS: now.Add(-2 * time.Hour), RX: 3000, TX: 2700},
	}
	shuffled := []usageSnapshot{sorted[2], sorted[0], sorted[1]}

	want := usageWindow(sorted, 24*time.Hour, now)
	got := usageWindow(shuffled, 24*time.Hour, now)
	if got.Total() != want.Total() {
		t.Fatalf("unsorted-input window total = %d, want %d", got.Total(), want.Total())
	}
}

// TestDeltaBucketsRestartAcrossBuckets: a restart between buckets must reset
// the baseline so the new bucket counts only post-restart traffic.
func TestDeltaBucketsRestartAcrossBuckets(t *testing.T) {
	now := time.Now()
	day0 := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	day1 := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	snaps := []usageSnapshot{
		{TS: day0, RX: 3000, TX: 2000, BillableRX: 2800, BillableTX: 1800},              // cum=5000
		{TS: day1, RX: 100, TX: 50, BillableRX: 90, BillableTX: 40},                     // cum=150 (restart between buckets)
		{TS: day1.Add(time.Hour), RX: 1100, TX: 950, BillableRX: 1000, BillableTX: 850}, // cum=2050
	}
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}, 10, now)
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2", len(buckets))
	}
	// day0: 5000 (first snapshot, baseline 0).
	if buckets[0].total != 5000 {
		t.Fatalf("day0 total = %d, want 5000", buckets[0].total)
	}
	// day1: first snapshot is a restart → delta from restart is 150, then 2050-150=1900.
	// Total: 150+1900 = 2050.
	if buckets[1].total != 2050 {
		t.Fatalf("day1 total = %d, want 2050", buckets[1].total)
	}
}

// TestUsageGraphsArgsPreservesTrailingFlags is the regression for the
// CodeRabbit finding on `cmdUsage`: `usage graphs --unit X` must reach
// target parsing with --unit X intact. The bug computed target args as
// args[:i] (everything before the subcommand token), which for the
// documented example `usage graphs --unit mynetwork-provider` (subcommand
// at index 0) produced an EMPTY target-args slice, silently discarding the
// flag.
func TestUsageGraphsArgsPreservesTrailingFlags(t *testing.T) {
	args := []string{"graphs", "--unit", "mynetwork-provider"}
	got := usageGraphsArgs(args, 0)
	want := []string{"--unit", "mynetwork-provider"}
	if len(got) != len(want) {
		t.Fatalf("usageGraphsArgs(%v, 0) = %v, want %v", args, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("usageGraphsArgs(%v, 0) = %v, want %v", args, got, want)
		}
	}
}

// TestUsageGraphArgsPreservesTrailingFlags mirrors the above for
// `usage graph <view> --flags`, the other documented example
// (`usage graph month --network tacogonzalez3000`).
func TestUsageGraphArgsPreservesTrailingFlags(t *testing.T) {
	args := []string{"graph", "month", "--network", "tacogonzalez3000"}
	targetArgs, view := usageGraphArgs(args, 0)
	if view != "month" {
		t.Fatalf("usageGraphArgs(%v, 0) view = %q, want %q", args, view, "month")
	}
	want := []string{"--network", "tacogonzalez3000"}
	if len(targetArgs) != len(want) {
		t.Fatalf("usageGraphArgs(%v, 0) targetArgs = %v, want %v", args, targetArgs, want)
	}
	for i := range want {
		if targetArgs[i] != want[i] {
			t.Fatalf("usageGraphArgs(%v, 0) targetArgs = %v, want %v", args, targetArgs, want)
		}
	}
}

// TestUsageGraphArgsFlagBeforeSubcommand covers `usage --unit X graph day`:
// the subcommand token is not at index 0, and no view-like flag should be
// mistaken for the view.
func TestUsageGraphArgsFlagBeforeSubcommand(t *testing.T) {
	args := []string{"--unit", "X", "graph", "day"}
	targetArgs, view := usageGraphArgs(args, 2)
	if view != "day" {
		t.Fatalf("usageGraphArgs(%v, 2) view = %q, want %q", args, view, "day")
	}
	want := []string{"--unit", "X"}
	if len(targetArgs) != len(want) {
		t.Fatalf("usageGraphArgs(%v, 2) targetArgs = %v, want %v", args, targetArgs, want)
	}
	for i := range want {
		if targetArgs[i] != want[i] {
			t.Fatalf("usageGraphArgs(%v, 2) targetArgs = %v, want %v", args, targetArgs, want)
		}
	}
}

// TestUsageGraphArgsNoView covers bare `usage graph` (no view token at all).
func TestUsageGraphArgsNoView(t *testing.T) {
	args := []string{"graph"}
	targetArgs, view := usageGraphArgs(args, 0)
	if view != "" {
		t.Fatalf("usageGraphArgs(%v, 0) view = %q, want empty", args, view)
	}
	if len(targetArgs) != 0 {
		t.Fatalf("usageGraphArgs(%v, 0) targetArgs = %v, want empty", args, targetArgs)
	}
}
