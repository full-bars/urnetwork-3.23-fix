package urnettools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// baseline.go is the CLI half of the baseline recorder: `urnet-tools baseline
// show`, `mark` and `compare`.
//
// The single-writer rule is the important structural point: show and compare
// read ~/.urnetwork/baseline.jsonl directly, because they are read-only and
// must work on a box whose provider is stopped. mark does NOT: it goes through
// the control socket so the provider stays the only writer. A second writer
// would be a second process appending to the same JSONL during a hot swap,
// which is exactly the case the append's inter-process lock exists for.

// baselineRow is one parsed line. It mirrors the provider's on-disk shape.
type baselineRow struct {
	V    int    `json:"v"`
	Kind string `json:"kind"`
	// Label is set on marks only.
	Label           string `json:"label,omitempty"`
	TS              string `json:"ts"`
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	UptimeSeconds   *int64 `json:"uptime_seconds,omitempty"`

	Proxies struct {
		Up         int `json:"up"`
		Degraded   int `json:"degraded"`
		Connecting int `json:"connecting"`
		Dead       int `json:"dead"`
	} `json:"proxies"`
	Desired *int `json:"desired,omitempty"`
	TrimCap *int `json:"trim_cap,omitempty"`

	Clients  int64 `json:"clients"`
	Sessions struct {
		PQE       int64 `json:"pqe"`
		Classical int64 `json:"classical"`
	} `json:"sessions"`
	Rate struct {
		Avg5mBps *int64 `json:"avg5m_bps,omitempty"`
	} `json:"rate"`
	Traffic *struct {
		BillableBytes         uint64  `json:"billable_bytes"`
		TotalBytes            uint64  `json:"total_bytes"`
		LifetimeBillableBytes *uint64 `json:"lifetime_billable_bytes,omitempty"`
	} `json:"traffic,omitempty"`
	Contracts *struct {
		Acquired int64 `json:"acquired"`
		Denied   int64 `json:"denied"`
	} `json:"contracts,omitempty"`
	Pressure *float64 `json:"pressure,omitempty"`

	Resources struct {
		RSSBytes       *uint64 `json:"rss_bytes,omitempty"`
		HeapInuseBytes *uint64 `json:"heap_inuse_bytes,omitempty"`
		MemLimitBytes  *uint64 `json:"mem_limit_bytes,omitempty"`
		Goroutines     *int    `json:"goroutines,omitempty"`
		OpenFDs        *uint64 `json:"open_fds,omitempty"`
		FDLimit        *uint64 `json:"fd_limit,omitempty"`
	} `json:"resources"`
	Host *struct {
		MemAvailableMiB *int64   `json:"mem_available_mib,omitempty"`
		AvailSource     string   `json:"avail_source,omitempty"`
		SwapUsedMiB     *int64   `json:"swap_used_mib,omitempty"`
		PSISomeAvg60    *float64 `json:"psi_some_avg60,omitempty"`
		PSIFullAvg60    *float64 `json:"psi_full_avg60,omitempty"`
		PSIFullAvg300   *float64 `json:"psi_full_avg300,omitempty"`
		Load1           *float64 `json:"load1,omitempty"`
	} `json:"host,omitempty"`
	OOM *struct {
		Kills int64  `json:"kills"`
		Scope string `json:"scope,omitempty"`
	} `json:"oom,omitempty"`
	Restart *struct {
		Reason        string `json:"reason,omitempty"`
		CleanShutdown bool   `json:"clean_shutdown"`
	} `json:"restart,omitempty"`
	Errors []struct {
		Category string `json:"category"`
		Count    uint64 `json:"count"`
	} `json:"errors,omitempty"`
}

// timeAt parses a row's timestamp. Rows whose timestamp will not parse are
// skipped rather than treated as the epoch: one bad line must not shift a
// whole comparison.
func (r baselineRow) timeAt() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, r.TS)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (r baselineRow) lifetimeBytes() (uint64, bool) {
	if r.Traffic == nil || r.Traffic.LifetimeBillableBytes == nil {
		return 0, false
	}
	return *r.Traffic.LifetimeBillableBytes, true
}

func (r baselineRow) rssBytes() (uint64, bool) {
	if r.Resources.RSSBytes == nil {
		return 0, false
	}
	return *r.Resources.RSSBytes, true
}

func (r baselineRow) availMiB() (int64, bool) {
	if r.Host == nil || r.Host.MemAvailableMiB == nil {
		return 0, false
	}
	return *r.Host.MemAvailableMiB, true
}

func (r baselineRow) swapMiB() (int64, bool) {
	if r.Host == nil || r.Host.SwapUsedMiB == nil {
		return 0, false
	}
	return *r.Host.SwapUsedMiB, true
}

func (r baselineRow) psiFull60() (float64, bool) {
	if r.Host == nil || r.Host.PSIFullAvg60 == nil {
		return 0, false
	}
	return *r.Host.PSIFullAvg60, true
}

func (r baselineRow) availSource() string {
	if r.Host == nil {
		return ""
	}
	return r.Host.AvailSource
}

func (r baselineRow) oomKills() int64 {
	if r.OOM == nil {
		return 0
	}
	return r.OOM.Kills
}

// excludeRamp drops the rows within skip of the most recent start mark in the
// segment, on BOTH sides of a comparison. A pool that just restarted under-earns
// while it ramps back up, and those minutes would otherwise drag one side's
// figure and make the comparison a story about the ramp rather than about the
// upgrade.
//
// The start mark itself is kept: it is the boundary, and the reader counts
// restarts from it. A segment with no start mark is returned unchanged, because
// there is no ramp to exclude.
func excludeRamp(rows []baselineRow, skip time.Duration) []baselineRow {
	var startAt time.Time
	for _, r := range rows {
		if r.Kind != "start" {
			continue
		}
		if at, ok := r.timeAt(); ok {
			// The LAST start is the relevant one for a segment: an earlier one
			// means the segment spans two lives and the later boundary is the
			// one the ramp belongs to.
			startAt = at
		}
	}
	if startAt.IsZero() || skip <= 0 {
		return rows
	}
	cut := startAt.Add(skip)
	out := make([]baselineRow, 0, len(rows))
	for _, r := range rows {
		at, ok := r.timeAt()
		if !ok {
			// A row with no usable timestamp cannot be placed relative to the
			// ramp. Keep it rather than silently dropping data.
			out = append(out, r)
			continue
		}
		if r.Kind == "start" || !at.Before(cut) {
			out = append(out, r)
		}
	}
	return out
}

// segmentStats is what compare reports for one side.
type segmentStats struct {
	Rows int
	// BillableBps is derived from the lifetime counter's delta over the time
	// delta, never from the instantaneous rate. The rates are bursty: a box
	// showing 4.6 KiB/s now and 56 KiB/s a minute later is normal, and
	// averaging instantaneous readings would report a number that never happened.
	BillableBps float64
	// OOMKillDelta is the change in the OOM counter across the segment.
	OOMKillDelta int64
	Restarts     int
	// Capacity is the configured pool size and the effective trim cap, so a
	// trimmed box is not misread as a regression.
	Desired   int
	TrimCap   int
	HaveCap   bool
	AvailSrcs map[string]bool

	ProxiesUpMean float64
	ClientsMean   float64
	RSSMean       float64
	RSSMax        float64
	AvailMin      int64
	HaveAvail     bool
	SwapMax       int64
	HaveSwap      bool
	PSIFullMean   float64
	PSIFullMax    float64
	HavePSI       bool
}

func newSegmentStats() segmentStats {
	return segmentStats{AvailSrcs: map[string]bool{}}
}

// billableBpsFrom builds the rate from the FIRST and LAST row that carry a
// lifetime total. Rows without one are skipped: the lifetime store may not
// have been running for the whole segment, and treating a missing total as
// zero would produce an enormous, entirely fictional rate.
func billableBpsFrom(rows []baselineRow) float64 {
	var first, last baselineRow
	var firstT, lastT time.Time
	haveFirst := false
	for _, r := range rows {
		v, ok := r.lifetimeBytes()
		if !ok {
			continue
		}
		t, ok := r.timeAt()
		if !ok {
			continue
		}
		if !haveFirst {
			first, firstT, haveFirst = r, t, true
			_ = v
		}
		last, lastT = r, t
	}
	if !haveFirst {
		return 0
	}
	firstV, ok1 := first.lifetimeBytes()
	lastV, ok2 := last.lifetimeBytes()
	if !ok1 || !ok2 {
		return 0
	}
	dt := lastT.Sub(firstT).Seconds()
	if dt <= 0 {
		return 0
	}
	// A counter that went DOWN was reset (a fresh lifetime store, or a state
	// directory wipe). That interval is ignored rather than reported as a
	// negative rate or a huge positive one.
	if lastV < firstV {
		return 0
	}
	return float64(lastV-firstV) / 8 / dt
}

// summarizeSegment computes the rows for one side of a comparison.
func summarizeSegment(rows []baselineRow) segmentStats {
	s := newSegmentStats()
	s.Rows = len(rows)
	if len(rows) == 0 {
		return s
	}
	var upSum, clientSum, rssSum, psiSum float64
	var upN, clientN, rssN, psiN int
	s.Restarts = 0
	for i, r := range rows {
		// A start mark inside a segment is a restart, which is what the reader
		// needs to see: a rate computed across a restart mixes two processes.
		if r.Kind == "start" {
			s.Restarts++
		}
		upSum += float64(r.Proxies.Up)
		upN++
		clientSum += float64(r.Clients)
		clientN++
		if v, ok := r.rssBytes(); ok {
			rssSum += float64(v)
			rssN++
			if float64(v) > s.RSSMax {
				s.RSSMax = float64(v)
			}
		}
		if v, ok := r.availMiB(); ok {
			if !s.HaveAvail || v < s.AvailMin {
				s.AvailMin = v
			}
			s.HaveAvail = true
			s.AvailSrcs[r.availSource()] = true
		}
		if v, ok := r.swapMiB(); ok {
			if v > s.SwapMax {
				s.SwapMax = v
			}
			s.HaveSwap = true
		}
		if v, ok := r.psiFull60(); ok {
			psiSum += v
			psiN++
			if v > s.PSIFullMax {
				s.PSIFullMax = v
			}
			s.HavePSI = true
		}
		if r.Desired != nil {
			s.Desired = *r.Desired
			s.HaveCap = true
		}
		if r.TrimCap != nil {
			s.TrimCap = *r.TrimCap
			s.HaveCap = true
		}
		_ = i
	}
	if upN > 0 {
		s.ProxiesUpMean = upSum / float64(upN)
	}
	if clientN > 0 {
		s.ClientsMean = clientSum / float64(clientN)
	}
	if rssN > 0 {
		s.RSSMean = rssSum / float64(rssN)
	}
	if psiN > 0 {
		s.PSIFullMean = psiSum / float64(psiN)
	}
	s.BillableBps = billableBpsFrom(rows)
	first, ok1 := rows[0].oomKills(), true
	last := first
	if len(rows) > 0 {
		last = rows[len(rows)-1].oomKills()
	}
	_ = ok1
	if last >= first {
		s.OOMKillDelta = last - first
	}
	return s
}

// baselineMinSegmentSamples is the floor below which a comparison is not
// reported as a percentage. Under four samples the segment may be entirely
// inside one burst, and a percentage computed from it would be a number the
// reader trusts too much.
const baselineMinSegmentSamples = 4

// compareResult is what `baseline compare` prints.
type compareResult struct {
	Before segmentStats
	After  segmentStats
	// Warnings are the one-line caveats: a capacity change or a change of
	// availability source makes the two sides not like for like, and a reader
	// who is not told will read a trimmed box as a regression.
	Warnings []string
	// Insufficient is set when either side is below the sample floor.
	Insufficient string
	Rows         []compareRow
}

type compareRow struct {
	Label   string
	Before  string
	After   string
	Change  string
	Numeric bool
}

// compareSegments is the maths, separated from the I/O so it can be tested on
// synthetic series.
func compareSegments(before, after []baselineRow) compareResult {
	res := compareResult{Before: summarizeSegment(before), After: summarizeSegment(after)}

	if res.Before.Desired != res.After.Desired || res.Before.TrimCap != res.After.TrimCap {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"capacity changed %d -> %d (trim cap %d -> %d); the rate difference includes that",
			res.Before.Desired, res.After.Desired, res.Before.TrimCap, res.After.TrimCap))
	}
	if len(res.Before.AvailSrcs) > 0 && len(res.After.AvailSrcs) > 0 &&
		!sameAvailSource(res.Before.AvailSrcs, res.After.AvailSrcs) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"available memory came from %s before and %s after; the two figures are not like for like",
			availSourceList(res.Before.AvailSrcs), availSourceList(res.After.AvailSrcs)))
	}
	if res.Before.Rows < baselineMinSegmentSamples || res.After.Rows < baselineMinSegmentSamples {
		res.Insufficient = fmt.Sprintf("insufficient data: %d samples before, %d after (need %d each)",
			res.Before.Rows, res.After.Rows, baselineMinSegmentSamples)
	}

	kiB := func(bps float64) string { return fmt.Sprintf("%.1f", bps*8/1024) }
	res.Rows = []compareRow{
		{Label: "billable KiB/s", Before: kiB(res.Before.BillableBps), After: kiB(res.After.BillableBps),
			Numeric: true, Change: changePct(res.Before.BillableBps, res.After.BillableBps)},
		{Label: "proxies up (mean)", Before: fmt.Sprintf("%.1f", res.Before.ProxiesUpMean),
			After: fmt.Sprintf("%.1f", res.After.ProxiesUpMean), Numeric: true,
			Change: changePct(res.Before.ProxiesUpMean, res.After.ProxiesUpMean)},
		{Label: "clients (mean)", Before: fmt.Sprintf("%.1f", res.Before.ClientsMean),
			After: fmt.Sprintf("%.1f", res.After.ClientsMean), Numeric: true,
			Change: changePct(res.Before.ClientsMean, res.After.ClientsMean)},
		{Label: "RSS MiB (mean)", Before: mib(res.Before.RSSMean), After: mib(res.After.RSSMean),
			Numeric: true, Change: changePct(res.Before.RSSMean, res.After.RSSMean)},
		{Label: "RSS MiB (max)", Before: mib(res.Before.RSSMax), After: mib(res.After.RSSMax),
			Numeric: true, Change: changePct(res.Before.RSSMax, res.After.RSSMax)},
		{Label: "host available MiB (min)", Before: intOrDash(res.Before.AvailMin, res.Before.HaveAvail),
			After: intOrDash(res.After.AvailMin, res.After.HaveAvail)},
		{Label: "swap used MiB (max)", Before: intOrDash(res.Before.SwapMax, res.Before.HaveSwap),
			After: intOrDash(res.After.SwapMax, res.After.HaveSwap)},
		{Label: "PSI full avg60 (mean)", Before: f2(res.Before.PSIFullMean, res.Before.HavePSI),
			After: f2(res.After.PSIFullMean, res.After.HavePSI)},
		{Label: "PSI full avg60 (max)", Before: f2(res.Before.PSIFullMax, res.Before.HavePSI),
			After: f2(res.After.PSIFullMax, res.After.HavePSI)},
		{Label: "desired", Before: intOrDashI(res.Before.Desired, res.Before.HaveCap),
			After: intOrDashI(res.After.Desired, res.After.HaveCap)},
		{Label: "trim cap", Before: intOrDashI(res.Before.TrimCap, res.Before.HaveCap),
			After: intOrDashI(res.After.TrimCap, res.After.HaveCap)},
		{Label: "restarts", Before: strconv.Itoa(res.Before.Restarts), After: strconv.Itoa(res.After.Restarts)},
		{Label: "OOM kills", Before: strconv.FormatInt(res.Before.OOMKillDelta, 10),
			After: strconv.FormatInt(res.After.OOMKillDelta, 10)},
	}
	return res
}

func mib(b float64) string { return fmt.Sprintf("%.1f", b/1024/1024) }
func f2(v float64, ok bool) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.2f", v)
}
func intOrDash(v int64, ok bool) string {
	if !ok {
		return "-"
	}
	return strconv.FormatInt(v, 10)
}

// intOrDashI is intOrDash for the count fields, which are plain ints.
func intOrDashI(v int, ok bool) string {
	if !ok {
		return "-"
	}
	return strconv.Itoa(v)
}

// pct renders a change, or a dash when there is no before value to compare
// against. A percentage of zero is not zero percent, it is unknown.
func changePct(before, after float64) string {
	if before == 0 {
		return "-"
	}
	d := (after - before) / before * 100
	sign := "+"
	if d < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%.1f%%", sign, d)
}

func sameAvailSource(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func availSourceList(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "unknown"
	}
	return strings.Join(out, "/")
}

// renderCompare formats the comparison as the table an operator reads.
func renderCompare(r compareResult, asJSON bool) (string, error) {
	if asJSON {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	var b strings.Builder
	b.WriteString("                      before        after        change\n")
	for _, row := range r.Rows {
		b.WriteString(fmt.Sprintf("  %-26s %10s %13s", row.Label, row.Before, row.After))
		if row.Change != "" {
			b.WriteString("  " + row.Change)
		}
		b.WriteString("\n")
	}
	if r.Insufficient != "" {
		b.WriteString("\n" + r.Insufficient + "; percentages are omitted because a segment " +
			"that short may sit entirely inside one burst\n")
	}
	for _, w := range r.Warnings {
		b.WriteString("warning: " + w + "\n")
	}
	return b.String(), nil
}
