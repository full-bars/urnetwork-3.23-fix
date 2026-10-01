package urnettools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The compare maths is where an upgrade verdict is actually decided, so every
// rule the brief names gets a test on a SYNTHETIC series. Nothing here reads a
// clock or a file: rows are built with fixed timestamps, so the results are
// exact rather than approximate.

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", s, err)
	}
	return v.UTC()
}

func u64(v uint64) *uint64   { return &v }
func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

// sample builds a row with a lifetime counter, the thing the rate derives from.
func sample(t *testing.T, at string, lifetime uint64) baselineRow {
	tm := ts(t, at)
	r := baselineRow{V: 1, Kind: "sample", TS: tm.Format(time.RFC3339), Version: "v1"}
	tr := struct {
		BillableBytes         uint64  `json:"billable_bytes"`
		TotalBytes            uint64  `json:"total_bytes"`
		LifetimeBillableBytes *uint64 `json:"lifetime_billable_bytes,omitempty"`
	}{BillableBytes: lifetime, LifetimeBillableBytes: u64(lifetime)}
	r.Traffic = &tr
	return r
}

// The rate must come from the lifetime counter's delta over the time delta, NOT
// from the instantaneous rate field. A box showing 4.6 KiB/s now and 56 KiB/s a
// minute later is normal; averaging those would report a number that never
// happened. This test makes the two disagree and checks which one wins.
func TestCompareRateComesFromTheCounterDeltaNotTheInstantaneousRate(t *testing.T) {
	rows := []baselineRow{sample(t, "2026-09-30T12:00:00Z", 0), sample(t, "2026-09-30T12:10:00Z", 600*1024)}
	// Put a wildly wrong instantaneous rate on the first row. The delta must win.
	rows[0].Rate.Avg5mBps = i64(999_999_999)
	// 600 KiB over 600 s = 1 KiB/s.
	got := billableBpsFrom(rows)
	wantKiBps := 1.0
	if got*8/1024 != wantKiBps {
		t.Errorf("rate = %.4f KiB/s, want %.1f; the instantaneous rate must be ignored",
			got*8/1024, wantKiBps)
	}
}

func TestCompareIgnoresACounterThatWentDown(t *testing.T) {
	// A counter that dropped means it was reset: a fresh lifetime store, or a
	// wiped state directory. Reporting that as a rate would produce an enormous
	// fictional number, and reporting it as negative would be nonsense.
	rows := []baselineRow{sample(t, "2026-09-30T12:00:00Z", 5_000_000), sample(t, "2026-09-30T12:10:00Z", 1000)}
	if got := billableBpsFrom(rows); got != 0 {
		t.Errorf("rate = %v for a reset counter, want 0: the interval is not measurable", got)
	}
}

func TestCompareSkipsRowsWithoutALifetimeTotal(t *testing.T) {
	// The lifetime store may not have been running for the whole segment. A
	// missing total treated as zero would produce a huge rate.
	rows := []baselineRow{sample(t, "2026-09-30T12:00:00Z", 4096), sample(t, "2026-09-30T12:10:00Z", 4096)}
	rows[1].Traffic = nil // no total at all
	got := billableBpsFrom(rows)
	if got != 0 {
		t.Errorf("rate = %v with only one usable row, want 0", got)
	}
}

func TestCompareRampIsExcludedFromBothSides(t *testing.T) {
	// A pool that just restarted under-earns while it ramps. Those minutes
	// would drag the "before" figure down and make a later segment look like an
	// improvement it was not.
	before := []baselineRow{
		startRow(t, "2026-09-30T11:00:00Z"),
		// The ramp: almost no traffic for the first 20 minutes.
		sample(t, "2026-09-30T11:05:00Z", 1024),
		sample(t, "2026-09-30T11:10:00Z", 2048),
		sample(t, "2026-09-30T11:15:00Z", 3072),
		// Then it settles.
		sample(t, "2026-09-30T11:25:00Z", 3072+5*1024*1024),
		sample(t, "2026-09-30T11:35:00Z", 3072+10*1024*1024),
		sample(t, "2026-09-30T11:45:00Z", 3072+15*1024*1024),
	}
	ramped := excludeRamp(before, rampStarts(before), 20*time.Minute)
	if len(ramped) >= len(before) {
		t.Fatalf("ramp exclusion removed nothing: %d of %d rows kept", len(ramped), len(before))
	}
	// The start mark itself is KEPT: it is the boundary the reader counts
	// restarts from. Every other kept row must be past the ramp.
	start := ts(t, "2026-09-30T11:00:00Z")
	sawStart := false
	for _, r := range ramped {
		at, ok := r.timeAt()
		if !ok {
			t.Fatal("a kept row has no timestamp")
		}
		if r.Kind == "start" {
			sawStart = true
			continue
		}
		if at.Sub(start) < 20*time.Minute {
			t.Errorf("row at %s survived the ramp exclusion; it is only %v after the start",
				at.Format(time.RFC3339), at.Sub(start))
		}
	}
	if !sawStart {
		t.Error("the start mark was dropped; it is the boundary compare splits on")
	}
}

func TestExcludeRampDropsTheRampAfterEveryStart(t *testing.T) {
	// A segment that spans two restarts has two ramps. Dropping only the one
	// after the latest start leaves the first ramp's near-idle samples in the
	// averages, which is the skew the exclusion exists to prevent.
	rows := []baselineRow{
		startRow(t, "2026-09-30T11:00:00Z"),
		sample(t, "2026-09-30T11:05:00Z", 1024),        // ramp after the first start
		sample(t, "2026-09-30T11:30:00Z", 5*1024*1024), // settled
		startRow(t, "2026-09-30T12:00:00Z"),
		sample(t, "2026-09-30T12:05:00Z", 6*1024*1024),  // ramp after the second start
		sample(t, "2026-09-30T12:30:00Z", 11*1024*1024), // settled
	}
	got := excludeRamp(rows, rampStarts(rows), 20*time.Minute)

	var kept []string
	for _, r := range got {
		kept = append(kept, r.Kind+"@"+r.TS)
	}
	for _, r := range got {
		if r.Kind == "start" {
			continue
		}
		at, ok := r.timeAt()
		if !ok {
			t.Fatal("a kept row has no timestamp")
		}
		for _, s := range []string{"2026-09-30T11:00:00Z", "2026-09-30T12:00:00Z"} {
			d := at.Sub(ts(t, s))
			if d >= 0 && d < 20*time.Minute {
				t.Errorf("row at %s survived: only %v after the start at %s (kept: %v)",
					at.Format(time.RFC3339), d, s, kept)
			}
		}
	}
	starts := 0
	for _, r := range got {
		if r.Kind == "start" {
			starts++
		}
	}
	if starts != 2 {
		t.Errorf("kept %d start marks, want both: they are the boundaries restarts are counted from", starts)
	}
	if len(got) != 4 {
		t.Errorf("kept %d rows, want 4 (two starts and the two settled samples): %v", len(got), kept)
	}
}

func TestCompareReportsInsufficientDataInsteadOfPercentages(t *testing.T) {
	// Under four samples a segment may sit entirely inside one burst, and a
	// percentage from it would be trusted far more than it deserves.
	before := []baselineRow{sample(t, "2026-09-30T12:00:00Z", 0), sample(t, "2026-09-30T12:10:00Z", 1024)}
	after := []baselineRow{
		sample(t, "2026-09-30T13:00:00Z", 0), sample(t, "2026-09-30T13:10:00Z", 1024),
		sample(t, "2026-09-30T13:20:00Z", 2048), sample(t, "2026-09-30T13:30:00Z", 3072),
	}
	r := compareSegments(before, after)
	if !strings.Contains(r.Insufficient, "insufficient data") {
		t.Errorf("Insufficient = %q, want an insufficient-data message", r.Insufficient)
	}
	if !strings.Contains(r.Insufficient, "2 samples before") {
		t.Errorf("Insufficient = %q, want the actual counts so the operator can see how far short it fell", r.Insufficient)
	}
	out, err := renderCompare(r, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "insufficient data") {
		t.Errorf("the rendered output does not say the data was insufficient:\n%s", out)
	}
}

func TestCompareWarnsWhenCapacityChanged(t *testing.T) {
	// A box trimmed from 1170 to 500 is not a regression, and reading it as one
	// is exactly the misreading the warning exists to prevent.
	before := fullSegment(t, "2026-09-30T11:00:00Z", 1170, 1170)
	after := fullSegment(t, "2026-09-30T13:00:00Z", 500, 500)
	r := compareSegments(before, after)
	if len(r.Warnings) == 0 {
		t.Fatal("no warning for a capacity change; a trimmed box would read as a regression")
	}
	if !strings.Contains(r.Warnings[0], "capacity changed 1170 -> 500") {
		t.Errorf("warning = %q, want it to name the change", r.Warnings[0])
	}
	if !strings.Contains(r.Warnings[0], "rate difference includes that") {
		t.Errorf("warning = %q, want it to say the rate difference is affected", r.Warnings[0])
	}
}

func TestCompareWarnsWhenTheAvailabilitySourceChanged(t *testing.T) {
	// A docker box and a systemd box report availability from different readers,
	// so the two figures are not comparable and the reader has to be told.
	before := fullSegment(t, "2026-09-30T11:00:00Z", 500, 500)
	after := fullSegment(t, "2026-09-30T13:00:00Z", 500, 500)
	// Mark the after rows as cgroup-sourced.
	for i := range after {
		after[i].Host = &struct {
			MemAvailableMiB *int64   `json:"mem_available_mib,omitempty"`
			AvailSource     string   `json:"avail_source,omitempty"`
			SwapUsedMiB     *int64   `json:"swap_used_mib,omitempty"`
			PSISomeAvg60    *float64 `json:"psi_some_avg60,omitempty"`
			PSIFullAvg60    *float64 `json:"psi_full_avg60,omitempty"`
			PSIFullAvg300   *float64 `json:"psi_full_avg300,omitempty"`
			Load1           *float64 `json:"load1,omitempty"`
		}{MemAvailableMiB: i64(1024), AvailSource: "cgroup"}
	}
	r := compareSegments(before, after)
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "not like for like") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning about the availability source changing; warnings=%v", r.Warnings)
	}
}

func TestCompareCountsRestartsInTheSegment(t *testing.T) {
	// A rate computed across a restart mixes two processes, so the reader has
	// to see that it happened.
	rows := fullSegment(t, "2026-09-30T11:00:00Z", 100, 100)
	rows[2].Kind = "start"
	rows[2].TS = "2026-09-30T11:20:00Z"
	s := summarizeSegment(rows)
	if s.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", s.Restarts)
	}
}

func TestCompareMeasuresOOMKillDeltaAcrossTheSegment(t *testing.T) {
	rows := fullSegment(t, "2026-09-30T11:00:00Z", 100, 100)
	// The delta uses the first and last available sample counters.
	for i := range rows {
		kills := int64(2)
		if i == len(rows)-1 {
			kills = 5
		}
		rows[i].OOM = &struct {
			Kills int64  `json:"kills"`
			Scope string `json:"scope,omitempty"`
		}{Kills: kills, Scope: "cgroup:memory.events"}
	}
	s := summarizeSegment(rows)
	if s.OOMKillDelta != 3 {
		t.Errorf("OOM kill delta = %d, want 3 (2 to 5)", s.OOMKillDelta)
	}
}

func TestCompareReportsHostMinAndSwapMax(t *testing.T) {
	// The brief asks for available as a MIN (worst moment) and swap as a MAX
	// (worst moment). A mean would hide the moment a box ran out.
	rows := fullSegment(t, "2026-09-30T11:00:00Z", 100, 100)
	avails := []int64{8000, 200, 5000, 7000, 6000}
	swaps := []int64{10, 900, 20, 30, 40}
	psis := []float64{0.1, 4.5, 0.2, 0.3, 0.4}
	for i := range rows {
		rows[i].Host = &struct {
			MemAvailableMiB *int64   `json:"mem_available_mib,omitempty"`
			AvailSource     string   `json:"avail_source,omitempty"`
			SwapUsedMiB     *int64   `json:"swap_used_mib,omitempty"`
			PSISomeAvg60    *float64 `json:"psi_some_avg60,omitempty"`
			PSIFullAvg60    *float64 `json:"psi_full_avg60,omitempty"`
			PSIFullAvg300   *float64 `json:"psi_full_avg300,omitempty"`
			Load1           *float64 `json:"load1,omitempty"`
		}{MemAvailableMiB: i64(avails[i]), AvailSource: "host", SwapUsedMiB: i64(swaps[i]), PSIFullAvg60: f64(psis[i])}
	}
	s := summarizeSegment(rows)
	if s.AvailMin != 200 {
		t.Errorf("available min = %d, want 200 (the worst moment, not the mean)", s.AvailMin)
	}
	if s.SwapMax != 900 {
		t.Errorf("swap max = %d, want 900", s.SwapMax)
	}
	if s.PSIFullMax < 4.5 {
		t.Errorf("PSI full max = %v, want at least 4.5", s.PSIFullMax)
	}
}

func TestCompareRendersEveryRowTheBriefAsksFor(t *testing.T) {
	before := fullSegment(t, "2026-09-30T11:00:00Z", 100, 100)
	after := fullSegment(t, "2026-09-30T13:00:00Z", 100, 100)
	out, err := renderCompare(compareSegments(before, after), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"billable KiB/s", "proxies up", "clients", "RSS MiB (mean)", "RSS MiB (max)",
		"host available MiB (min)", "swap used MiB (max)", "PSI full avg60 (mean)",
		"PSI full avg60 (max)", "desired", "trim cap", "restarts", "OOM kills",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the comparison is missing the %q row:\n%s", want, out)
		}
	}
}

func TestCompareJSONOutputRoundTrips(t *testing.T) {
	before := fullSegment(t, "2026-09-30T11:00:00Z", 100, 100)
	after := fullSegment(t, "2026-09-30T13:00:00Z", 100, 100)
	out, err := renderCompare(compareSegments(before, after), true)
	if err != nil {
		t.Fatal(err)
	}
	var back compareResult
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("--json output does not parse: %v\n%s", err, out)
	}
	if back.Before.Rows != back.After.Rows {
		t.Errorf("round-trip lost rows: %d vs %d", back.Before.Rows, back.After.Rows)
	}
}

// fullSegment builds N samples four minutes apart with a steady lifetime
// counter, a proxy count, RSS, and host figures, so a comparison has something
// real to report.
func fullSegment(t *testing.T, startISO string, desired, trimCap int) []baselineRow {
	t.Helper()
	start := ts(t, startISO)
	rows := make([]baselineRow, 0, 5)
	var lifetime uint64
	for i := 0; i < 5; i++ {
		at := start.Add(time.Duration(i) * 4 * time.Minute)
		lifetime += 512 * 1024 // 512 KiB per 4 min = 2 KiB/s
		r := sample(t, at.Format(time.RFC3339), lifetime)
		r.Proxies.Up = 90
		r.Clients = int64(3 + i)
		r.Resources.RSSBytes = u64(400 << 20)
		r.Desired = &desired
		r.TrimCap = &trimCap
		r.Host = &struct {
			MemAvailableMiB *int64   `json:"mem_available_mib,omitempty"`
			AvailSource     string   `json:"avail_source,omitempty"`
			SwapUsedMiB     *int64   `json:"swap_used_mib,omitempty"`
			PSISomeAvg60    *float64 `json:"psi_some_avg60,omitempty"`
			PSIFullAvg60    *float64 `json:"psi_full_avg60,omitempty"`
			PSIFullAvg300   *float64 `json:"psi_full_avg300,omitempty"`
			Load1           *float64 `json:"load1,omitempty"`
		}{MemAvailableMiB: i64(4096), AvailSource: "host", SwapUsedMiB: i64(16), PSIFullAvg60: f64(0.5)}
		rows = append(rows, r)
	}
	return rows
}

func startRow(t *testing.T, at string) baselineRow {
	t.Helper()
	return baselineRow{V: 1, Kind: "start", TS: ts(t, at).Format(time.RFC3339), Version: "v1"}
}

// The real-file run exposed this: with a start mark at 13:30 and the next
// sample at 13:40, a 20 minute ramp exclusion leaves the "after" side with the
// start row and whatever is past the cut. That is correct. The bug was that
// `before` is rows[:boundary] which contains NO start row, so excludeRamp
// looked for a start, found none, and returned the rows unchanged, while
// `after` DID get cut. So the two sides were treated differently and the
// comparison reported "0 samples after" on a file that plainly had eight.
//
// This test reads the artifact the real run produced and asserts both sides
// come out non-empty, which is the property that was broken.
func TestCompareSplitGivesBothSidesSamples(t *testing.T) {
	rows := upgradeSeries(t)
	before, after, _, err := splitAtBoundary(rows, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b := excludeRamp(before, rampStarts(before), 20*time.Minute)
	a := excludeRamp(after, rampStarts(rows), 20*time.Minute)
	t.Logf("split: %d before, %d after; after ramp exclusion: %d and %d", len(before), len(after), len(b), len(a))
	if len(a) == 0 {
		t.Errorf("the after side is empty: the file has %d rows and the boundary is a real upgrade", len(rows))
	}
	if len(b) == 0 {
		t.Errorf("the before side is empty")
	}
}

// upgradeSeries builds a record with a real upgrade in the middle: six samples
// on the old version, a start marking the version change, then six on the new
// one. This is the shape `baseline compare` exists to read.
func upgradeSeries(t *testing.T) []baselineRow {
	t.Helper()
	rows := make([]baselineRow, 0, 14)
	base := ts(t, "2026-09-30T11:00:00Z")
	for i := 0; i < 6; i++ {
		r := sample(t, base.Add(time.Duration(i)*15*time.Minute).Format(time.RFC3339), uint64(i)*1024*1024)
		rows = append(rows, r)
	}
	rows = append(rows, startRow(t, "2026-09-30T13:00:00Z"))
	rows[6].Version, rows[6].PreviousVersion = "v2", "v1"
	for i := 0; i < 6; i++ {
		rows = append(rows, sample(t, base.Add(time.Duration(120+i*15)*time.Minute).Format(time.RFC3339),
			uint64(6+i)*1024*1024))
	}
	return rows
}

func TestBaselineArgsValueForms(t *testing.T) {
	for _, equals := range []bool{false, true} {
		args := []string{"compare"}
		for _, pair := range [][2]string{{"-n", "12"}, {"--from", "before upgrade"}, {"--to", "2026-09-30T13"}, {"--skip-ramp", "5m"}} {
			if equals {
				args = append(args, pair[0]+"="+pair[1])
			} else {
				args = append(args, pair[0], pair[1])
			}
		}
		args = append(args, "--json", "--user", "provider-user")
		sub, opts, err := parseBaselineArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if sub != "compare" || opts.limit != 12 || opts.from != "before upgrade" || opts.to != "2026-09-30T13" || opts.ramp != 5*time.Minute || !opts.asJSON || opts.target.User != "provider-user" || len(opts.positional) != 0 {
			t.Fatalf("equals=%v: unexpected options: %s %+v", equals, sub, opts)
		}
	}
	for _, flag := range []string{"-n", "--from", "--to", "--skip-ramp"} {
		if _, _, err := parseBaselineArgs([]string{"compare", flag}); err == nil {
			t.Errorf("missing value accepted for %s", flag)
		}
	}
	for _, pair := range [][2]string{{"-n", "0"}, {"-n", "-1"}, {"-n", "bad"}, {"--skip-ramp", "-1m"}, {"--skip-ramp", "bad"}} {
		_, _, spacedErr := parseBaselineArgs([]string{"compare", pair[0], pair[1]})
		_, _, equalsErr := parseBaselineArgs([]string{"compare", pair[0] + "=" + pair[1]})
		if spacedErr == nil || equalsErr == nil || spacedErr.Error() != equalsErr.Error() {
			t.Errorf("%v: validation differs: %v / %v", pair, spacedErr, equalsErr)
		}
	}
}

func TestBaselineCompareRampCrossesBoundary(t *testing.T) {
	start := startRow(t, "2026-09-30T12:00:00Z")
	start.PreviousVersion = "v0"
	rows := []baselineRow{
		sample(t, "2026-09-30T11:30:00Z", 100),
		start,
		sample(t, "2026-09-30T12:05:00Z", 200),
		{Kind: "mark", Label: "during ramp", TS: "2026-09-30T12:10:00Z"},
		sample(t, "2026-09-30T12:15:00Z", 300),
		sample(t, "2026-09-30T12:20:00Z", 400),
		sample(t, "2026-09-30T12:25:00Z", 500),
	}
	for _, from := range []string{"during ramp", "2026-09-30T12:10"} {
		out := captureStdout(t, func() {
			if err := cmdBaselineCompare(rows, baselineOpts{from: from, ramp: 20 * time.Minute, asJSON: true}); err != nil {
				t.Fatal(err)
			}
		})
		var got compareResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if got.Before.Rows != 1 || got.After.Rows != 2 || got.Before.Restarts != 1 || got.After.Restarts != 0 || got.Boundary != "from "+from {
			t.Fatalf("boundary %q: unexpected result %+v", from, got)
		}
	}
}

func TestBaselineSummaryIgnoresEventMeasurements(t *testing.T) {
	samples := fullSegment(t, "2026-09-30T11:00:00Z", 100, 100)[:3]
	want := summarizeSegment(samples)
	rows := append([]baselineRow(nil), samples...)
	for _, kind := range []string{"start", "mark", "unknown"} {
		var event baselineRow
		if err := json.Unmarshal([]byte(`{"kind":"`+kind+`","proxies":{"up":999},"clients":999,"resources":{"rss_bytes":99999999999},"host":{"mem_available_mib":1,"swap_used_mib":9999,"psi_full_avg60":999},"oom":{"kills":999}}`), &event); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, event)
	}
	want.Restarts = 1
	if got := summarizeSegment(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("events changed sample statistics: got %+v, want %+v", got, want)
	}
	if got := compareSegments(rows, rows); got.Insufficient == "" {
		t.Fatal("events incorrectly satisfied the minimum sample count")
	}
}

func TestBaselineSummaryUsesAvailableSampleOOMCounters(t *testing.T) {
	for _, counters := range []string{
		`[{"kind":"start","oom":{"kills":0}},{"kind":"sample"},{"kind":"sample","oom":{"kills":2}},{"kind":"sample"},{"kind":"sample","oom":{"kills":5}},{"kind":"sample"},{"kind":"mark","oom":{"kills":100}}]`,
		`[{"kind":"sample","oom":{"kills":2}},{"kind":"sample","oom":{"kills":5}}]`,
	} {
		var rows []baselineRow
		if err := json.Unmarshal([]byte(counters), &rows); err != nil {
			t.Fatal(err)
		}
		if got := summarizeSegment(rows).OOMKillDelta; got != 3 {
			t.Errorf("OOM delta = %d, want 3", got)
		}
	}
	for _, counters := range []string{
		`[{"kind":"sample"}]`,
		`[{"kind":"sample"},{"kind":"sample","oom":{"kills":5}}]`,
		`[{"kind":"sample","oom":{"kills":5}},{"kind":"sample","oom":{"kills":2}}]`,
	} {
		var rows []baselineRow
		if err := json.Unmarshal([]byte(counters), &rows); err != nil {
			t.Fatal(err)
		}
		if got := summarizeSegment(rows).OOMKillDelta; got != 0 {
			t.Errorf("OOM delta = %d, want 0 for missing, single or reset counters", got)
		}
	}
}

func TestBaselineOfflineTargets(t *testing.T) {
	home, stateDir := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	oldProcesses, oldStopped := discoverProcessesFn, discoverStoppedFn
	t.Cleanup(func() { discoverProcessesFn, discoverStoppedFn = oldProcesses, oldStopped })
	discoverProcessesFn = func() []Provider { return nil }
	discoverStoppedFn = func([]Provider) []Provider {
		return []Provider{{User: "provider-user", Unit: "urnetwork.service", StateDir: stateDir}}
	}
	path := filepath.Join(stateDir, "baseline.jsonl")
	if err := os.WriteFile(path, []byte("{\"kind\":\"sample\",\"ts\":\"2026-09-30T12:00:00Z\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{"--state-dir=" + stateDir, "--user=provider-user", "--unit=urnetwork.service"} {
		for _, sub := range []string{"show", "compare"} {
			captureStdout(t, func() {
				if err := cmdBaseline([]string{sub, "--json", selector}); err != nil {
					t.Errorf("%s %s without provider socket: %v", sub, selector, err)
				}
			})
		}
	}
	if _, err := baselineLocalPath(Target{User: "missing-user"}); err == nil {
		t.Error("unresolved target fell back to HOME")
	}
	discoverStoppedFn = func([]Provider) []Provider {
		return []Provider{{User: "provider-user"}}
	}
	if _, err := baselineLocalPath(Target{User: "provider-user"}); err == nil {
		t.Error("target without a state dir was accepted")
	}
	discoverProcessesFn = func() []Provider { t.Fatal("untargeted or explicit path read invoked discovery"); return nil }
	if got, err := baselineLocalPath(Target{}); err != nil || got != filepath.Join(home, ".urnetwork", "baseline.jsonl") {
		t.Fatalf("HOME fallback = %q, %v", got, err)
	}
	t.Setenv("HOME", "")
	if got, err := baselineLocalPath(Target{StateDir: stateDir}); err != nil || got != path {
		t.Fatalf("explicit path without HOME = %q, %v", got, err)
	}
	if _, err := baselineLocalPath(Target{}); err == nil {
		t.Error("untargeted read without HOME succeeded")
	}
}
