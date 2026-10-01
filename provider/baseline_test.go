package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ptr helpers, so a test can say "this field was measured" without repeating
// the address-of dance on every literal.
func i64p(v int64) *int64     { return &v }
func f64p(v float64) *float64 { return &v }
func u64p(v uint64) *uint64   { return &v }

// The baseline recorder writes a small, bounded, local time series so every box
// has a free before/after comparison for any future upgrade. The one rule that
// matters more than the schema is: an unknown field is OMITTED, never written
// as zero. A zero reads as "measured, and the answer was nothing", which for
// host memory or PSI would be a catastrophic misreading of a healthy box.
//
// These tests are deterministic by construction: the sample builder takes its
// inputs as arguments, so nothing here reads a clock, sleeps, or races.

func TestBaselineSampleOmitsUnknownFieldsRatherThanZeroingThem(t *testing.T) {
	// A snapshot that knows nothing about the host, plus readers that all failed.
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	got := buildBaselineSample(baselineInputs{
		now:     now,
		snap:    &NodeSnapshot{V: 1, Version: "v1", Now: now, StartedAt: now},
		hostErr: errNoHostData,
		oom:     baselineOOM{},
	})

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line := string(b)

	// Fields that must NOT appear when nothing was measured.
	for _, absent := range []string{
		"mem_available_mib", "avail_source", "swap_used_mib",
		"psi_some_avg60", "psi_full_avg60", "psi_full_avg300", "load1",
		"lifetime_billable_bytes", "oom", "desired", "trim_cap",
	} {
		if strings.Contains(line, absent) {
			t.Errorf("sample wrote %q when the value is unknown; it must be omitted so a "+
				"later reader cannot mistake unknown for zero:\n%s", absent, line)
		}
	}
	// And the ones that ARE known must be present, or the sample is useless.
	for _, present := range []string{`"kind":"sample"`, `"version":"v1"`} {
		if !strings.Contains(line, present) {
			t.Errorf("sample is missing %s:\n%s", present, line)
		}
	}
}

func TestBaselineSampleKeepsRealZerosThatWereActuallyMeasured(t *testing.T) {
	// The mirror image of the test above: a measured zero is real data and must
	// survive. A box with 0 available MiB and 0 PSI is a box in trouble, and
	// dropping those fields would hide it.
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	got := buildBaselineSample(baselineInputs{
		now: now,
		snap: &NodeSnapshot{
			V: 1, Version: "v1", Now: now, StartedAt: now,
			Proxies: SnapshotProxies{Up: 0},
		},
		host:      baselineHost{MemAvailableMiB: i64p(0), AvailSource: "host"},
		hostKnown: true,
		oom:       baselineOOM{},
	})
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"mem_available_mib":0`) {
		t.Errorf("a measured mem_available_mib of 0 was dropped; a real zero is data:\n%s", b)
	}
	if !strings.Contains(string(b), `"up":0`) {
		t.Errorf("a measured proxy count of 0 was dropped:\n%s", b)
	}
}

func TestBaselineSampleRoundsOutTheSchema(t *testing.T) {
	// A fully-populated sample must carry every field brief section 4 lists, so
	// a later field cannot be quietly forgotten.
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	lifetime := uint64(4096)
	got := buildBaselineSample(baselineInputs{
		now: now,
		snap: &NodeSnapshot{
			V: 1, Version: "v1.2.3", PreviousVersion: "v1.2.2",
			StartedAt: now.Add(-time.Hour), Now: now, UptimeSeconds: 3600,
			State: "degraded", StateReason: "dead proxies",
			Rate:      SnapshotRate{Avg5mBps: 1234},
			Clients:   7,
			Sessions:  SnapshotSessions{PQE: 3, Classical: 4},
			Proxies:   SnapshotProxies{Up: 10, Degraded: 1, Connecting: 2, Dead: 3},
			Pressure:  0.5,
			Restart:   SnapshotRestart{Reason: "manual", CleanShutdown: true},
			Resources: SnapshotResources{HeapInuseBytes: 5, MemLimitBytes: 6, RSSBytes: 7, Goroutines: 8, OpenFDs: 9, FDLimit: 10},
			Traffic:   &SnapshotTraffic{BillableBytes: 11, TotalBytes: 12, LifetimeBillableBytes: &lifetime},
		},
		host:      baselineHost{MemAvailableMiB: i64p(100), AvailSource: "cgroup", SwapUsedMiB: i64p(5), PSISomeAvg60: f64p(1.5), PSIFullAvg60: f64p(2.5), PSIFullAvg300: f64p(3.5), Load1: f64p(0.9)},
		hostKnown: true,
		desired:   500,
		trimCap:   400,
		trimSrc:   trimCapOperator,
		oom:       baselineOOM{Kills: 2, Scope: "cgroup:memory.events"},
		contracts: [2]int64{30, 4},
		errs:      map[string]uint64{"auth": 3, "contract": 1},
		errsKnown: true,
	})
	b, _ := json.Marshal(got)
	line := string(b)
	for _, want := range []string{
		`"v":1`, `"version":"v1.2.3"`, `"previous_version":"v1.2.2"`, `"uptime_seconds":3600`,
		`"state":"degraded"`, `"state_reason":"dead proxies"`,
		`"up":10`, `"degraded":1`, `"connecting":2`, `"dead":3`,
		`"desired":500`, `"trim_cap":400`, `"trim_cap_source":"operator"`,
		`"clients":7`, `"pqe":3`, `"classical":4`, `"avg5m_bps":1234`,
		`"billable_bytes":11`, `"total_bytes":12`, `"lifetime_billable_bytes":4096`,
		`"acquired":30`, `"denied":4`, `"pressure":0.5`,
		`"rss_bytes":7`, `"heap_inuse_bytes":5`, `"mem_limit_bytes":6`,
		`"goroutines":8`, `"open_fds":9`, `"fd_limit":10`,
		`"mem_available_mib":100`, `"avail_source":"cgroup"`, `"swap_used_mib":5`,
		`"psi_some_avg60":1.5`, `"psi_full_avg60":2.5`, `"psi_full_avg300":3.5`, `"load1":0.9`,
		`"kills":2`, `"scope":"cgroup:memory.events"`,
		`"reason":"manual"`, `"clean_shutdown":true`,
		`"category":"auth","count":3`,
		`"category":"contract","count":1`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("fully-populated sample is missing %s:\n%s", want, line)
		}
	}
}

func TestBaselineSampleKeepsOnlyTheTopErrors(t *testing.T) {
	// Brief section 4: top N by count, N=8, so the file cannot grow with the
	// error taxonomy.
	now := time.Now().UTC()
	errs := map[string]uint64{}
	for i := 0; i < 20; i++ {
		errs[string(rune('a'+i))] = uint64(i)
	}
	got := buildBaselineSample(baselineInputs{
		now: now, snap: &NodeSnapshot{V: 1, Version: "v", Now: now, StartedAt: now},
		errs: errs, errsKnown: true,
	})
	if len(got.Errors) != 8 {
		t.Fatalf("kept %d error categories, want 8", len(got.Errors))
	}
	// The highest counts must be the ones kept, in descending order.
	for i := 1; i < len(got.Errors); i++ {
		if got.Errors[i-1].Count < got.Errors[i].Count {
			t.Fatalf("errors not sorted by count: %+v", got.Errors)
		}
	}
	if got.Errors[0].Count != 19 {
		t.Errorf("highest-count category has %d, want 19", got.Errors[0].Count)
	}
}

func TestBaselineAppendKeepsTheNewestWholeLines(t *testing.T) {
	// The trim must never leave a half-written line, because every reader
	// parses line by line and a split line is a parse error forever.
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.jsonl")
	// A tiny cap so the trim runs on nearly every append.
	const maxBytes = 200
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		e := baselineSample{Kind: "sample", Version: "v1"}
		e.TS = base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		e.Proxies.Up = i
		if err := baselineAppend(path, e, maxBytes); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > maxBytes+64 {
		t.Errorf("file is %d bytes, well past the %d bound", len(b), maxBytes)
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		t.Error("file does not end on a line boundary; the last line was split")
	}
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var e baselineSample
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d does not parse, so the trim split it: %v\nline: %q", i, err, line)
		}
	}
	// The newest must survive, and it must be the last one written.
	rows, err := baselineTail(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("trim emptied the file")
	}
	if last := rows[len(rows)-1]; last.Proxies.Up != 19 {
		t.Errorf("newest sample lost: last is proxy %d, want 19", last.Proxies.Up)
	}
}

func TestBaselineAppendKeepsAnOversizedNewestLineRatherThanEmptying(t *testing.T) {
	// action_ledger.go hit this exact bug: with a single line bigger than the
	// bound, a naive start index of len(b) writes an EMPTY file, destroying the
	// whole record. Over the bound for one entry beats losing all of them.
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.jsonl")
	big := baselineSample{Kind: "sample", Version: strings.Repeat("x", 500)}
	big.TS = time.Now().UTC().Format(time.RFC3339)
	if err := baselineAppend(path, big, baselineMaxBytes); err != nil {
		t.Fatal(err)
	}
	// A second append, still oversized, must not empty what came before.
	if err := baselineAppend(path, big, baselineMaxBytes); err != nil {
		t.Fatal(err)
	}
	rows, err := baselineTail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: an oversized line must not empty the file", len(rows))
	}
}

func TestBaselineTailSkipsACorruptLine(t *testing.T) {
	// A box that was killed mid-write, or a disk that filled, leaves a partial
	// line. One bad line must not hide every other sample.
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.jsonl")
	good1 := `{"kind":"sample","ts":"2026-09-30T12:00:00Z","version":"v1","proxies":{"up":1}}`
	good2 := `{"kind":"sample","ts":"2026-09-30T12:15:00Z","version":"v1","proxies":{"up":2}}`
	bad := `{"kind":"sample","ts":"2026-`
	content := good1 + "\n" + bad + "\n" + good2 + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := baselineTail(path, 10)
	if err != nil {
		t.Fatalf("tail returned an error for a corrupt line: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (the corrupt line must be skipped, not fatal)", len(rows))
	}
	if rows[0].Proxies.Up != 1 || rows[1].Proxies.Up != 2 {
		t.Errorf("wrong rows survived: %+v", rows)
	}
}

func TestBaselineTailOnAMissingFileIsEmptyNotAnError(t *testing.T) {
	// `baseline show` on a box that has never recorded must print a helpful
	// message, not a stack trace.
	rows, err := baselineTail(filepath.Join(t.TempDir(), "nope.jsonl"), 10)
	if err != nil {
		t.Errorf("a missing file is not an error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows from a missing file", len(rows))
	}
}

func TestBaselineIntervalHasAOneMinuteFloor(t *testing.T) {
	// Brief section 3: the env override exists so tests and canaries can go
	// faster, with a 1 minute floor so nothing can turn it into a busy loop.
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", baselineInterval},
		{"90s", 90 * time.Second},
		{"1m", time.Minute},
		{"30s", time.Minute},           // below the floor, clamped up
		{"1ms", time.Minute},           // absurd, still clamped
		{"-5m", time.Minute},           // negative must never become a hot loop
		{"nonsense", baselineInterval}, // unparseable -> default, NOT the floor: a typo must not
		// silently turn a 15-minute recorder into a 1-minute one forever
	}
	for _, c := range cases {
		t.Setenv("URNETWORK_BASELINE_INTERVAL", c.env)
		if got := resolveBaselineInterval(); got != c.want {
			t.Errorf("URNETWORK_BASELINE_INTERVAL=%q -> %v, want %v", c.env, got, c.want)
		}
	}
}

func TestBaselineFirstSampleIsFiveMinutesAfterStart(t *testing.T) {
	// Not immediately: a sample at t=0 measures a pool that has not launched
	// yet, and the first 5 minutes are the ramp the compare logic excludes.
	if got := baselineFirstSampleDelay; got != 5*time.Minute {
		t.Errorf("first sample delay is %v, want 5m", got)
	}
}

func TestBaselineStartAndMarkRecords(t *testing.T) {
	// The start record is what compare finds an upgrade by, and a mark is the
	// operator's own annotation, so both must carry the fields the reader needs.
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.jsonl")
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	start := baselineStartSample("v1.2.3", "v1.2.2", now)
	if start.Kind != "start" {
		t.Errorf("kind = %q, want start", start.Kind)
	}
	if start.Version != "v1.2.3" || start.PreviousVersion != "v1.2.2" {
		t.Errorf("start record lost the version transition: %+v", start)
	}
	if err := baselineAppend(path, start, baselineMaxBytes); err != nil {
		t.Fatal(err)
	}

	mark := baselineMarkSample("pre-upgrade check", now.Add(time.Minute))
	if mark.Kind != "mark" || mark.Label != "pre-upgrade check" {
		t.Errorf("mark record wrong: %+v", mark)
	}
	if err := baselineAppend(path, mark, baselineMaxBytes); err != nil {
		t.Fatal(err)
	}

	rows, err := baselineTail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Kind != "start" || rows[1].Kind != "mark" {
		t.Fatalf("records did not round-trip: %+v", rows)
	}
	// A mark must never carry proxy data: it is an annotation, and numbers on
	// it would be read as measurements.
	if rows[1].Proxies.Up != 0 || rows[1].Desired != nil {
		t.Errorf("a mark carries measurements: %+v", rows[1])
	}
}

// The cap has to hold under a real workload, not just a few appends: 2,000
// samples at roughly 600 bytes each is about 1.2 MiB, so the trim has to run
// several times and keep the file bounded.
func TestBaselineCapHoldsAfterTwoThousandSamples(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, baselineFileName)
	base := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ {
		e := baselineSample{V: 1, Kind: "sample", Version: "v3.23.0-fix.32.9"}
		e.TS = base.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339)
		e.Proxies.Up = i % 1200
		e.Proxies.Dead = i % 7
		e.Desired = &[]int{1200}[0]
		if err := baselineAppend(path, e, baselineMaxBytes); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := baselineTail(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 2000 samples: file is %d bytes (cap %d), %d rows survive", st.Size(), baselineMaxBytes, len(rows))
	if st.Size() > baselineMaxBytes {
		t.Errorf("file is %d bytes, over the %d cap", st.Size(), baselineMaxBytes)
	}
	if len(rows) == 0 {
		t.Fatal("the trim emptied the file")
	}
	// The newest sample must be the last one written, not an old one.
	if last := rows[len(rows)-1]; last.Proxies.Up != (1999 % 1200) {
		t.Errorf("newest row has proxy %d, want %d: the trim kept the wrong end", last.Proxies.Up, 1999%1200)
	}
	// And every surviving line must parse, i.e. no line was split.
	raw, _ := os.ReadFile(path)
	if raw[len(raw)-1] != '\n' {
		t.Error("the file does not end on a line boundary")
	}

	// 2,000 of THESE samples must exceed the cap, so the trim has to run and the
	// bound has to hold afterwards. Without this the test above could pass with
	// a trim that never executes.
	wide := filepath.Join(dir, "wide.jsonl")
	for i := 0; i < 2000; i++ {
		e := baselineSample{V: 1, Kind: "sample", Version: "v3.23.0-fix.32.9"}
		e.TS = base.Add(time.Duration(i) * 15 * time.Minute).Format(time.RFC3339)
		e.Proxies.Up = i % 1200
		e.StateReason = strings.Repeat("x", 300)
		e.State = "degraded"
		if err := baselineAppend(wide, e, 64<<10); err != nil {
			t.Fatalf("wide append %d: %v", i, err)
		}
	}
	wst, _ := os.Stat(wide)
	wrows, _ := baselineTail(wide, 100000)
	t.Logf("wide case: file is %d bytes (cap %d), %d rows survive", wst.Size(), 64<<10, len(wrows))
	if wst.Size() > 64<<10 {
		t.Errorf("wide file is %d bytes, over the %d cap", wst.Size(), 64<<10)
	}
	if len(wrows) == 0 {
		t.Fatal("the trim emptied the wide file")
	}
	if len(wrows) >= 2000 {
		t.Errorf("all %d rows survived, so the trim never ran", len(wrows))
	}
	raww, _ := os.ReadFile(wide)
	if wst.Size() > 64<<10 && raww[len(raww)-1] != '\n' {
		t.Error("the trimmed file does not end on a line boundary")
	}
}
