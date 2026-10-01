package urnettools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// baseline.go is the CLI half of the baseline recorder: `urnet-tools baseline`
// show, mark and compare.
//
// The single-writer rule is the important structural point: show and compare
// read ~/.urnetwork/baseline.jsonl directly, because they are read-only and
// must work on a box whose provider is stopped. mark does NOT: it goes through
// the control socket so the provider stays the only writer. A second writer
// would be a second process appending to the same JSONL during a hot swap,
// which is exactly the case the append's inter-process lock exists for.

// readBaselineFile parses the record. A line that does not parse is skipped
// rather than fatal: a box killed mid-write leaves a partial line, and one bad
// line must not hide every other sample.
func readBaselineFile(path string) ([]baselineRow, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no baseline record at %s; a provider writes one from its "+
			"first start, so this box may not have run since the recorder shipped", path)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rows []baselineRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r baselineRow
		if json.Unmarshal(line, &r) == nil && r.Kind != "" {
			rows = append(rows, r)
		}
	}
	return rows, sc.Err()
}

func cmdBaselineShow(path string, rows []baselineRow, opts baselineOpts) error {
	if opts.asJSON {
		b, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	if len(rows) == 0 {
		return fmt.Errorf("the baseline record at %s is empty", path)
	}
	limit := opts.limit
	if limit <= 0 {
		limit = baselineDefaultShow
	}
	if limit > baselineMaxShow {
		limit = baselineMaxShow
	}
	shown := rows
	if len(shown) > limit {
		shown = shown[len(shown)-limit:]
	}
	var b strings.Builder
	b.WriteString("  time                 kind    ver        up  want  rssMiB  availMiB  swapMiB  billableMiB\n")
	for _, r := range shown {
		up := "-"
		if r.Kind == "sample" {
			up = strconv.Itoa(r.Proxies.Up)
		}
		want := "-"
		if r.Desired != nil {
			want = strconv.Itoa(*r.Desired)
		}
		rss, avail, swap, bill := "-", "-", "-", "-"
		if v, ok := r.rssBytes(); ok {
			rss = fmt.Sprintf("%.0f", float64(v)/1024/1024)
		}
		if v, ok := r.availMiB(); ok {
			avail = strconv.FormatInt(v, 10)
		}
		if v, ok := r.swapMiB(); ok {
			swap = strconv.FormatInt(v, 10)
		}
		// A delta from the previous shown row, because an instantaneous reading
		// of a bursty counter is not a rate anyone should act on.
		bill = "-"
		_ = bill
		label := r.Label
		if label == "" {
			label = r.Kind
		}
		b.WriteString(fmt.Sprintf("  %-20s %-7s %-10s %3s  %4s  %6s  %8s  %7s  %10s\n",
			r.TS, label, shortVersion(r.Version), up, want, rss, avail, swap, bill))
	}
	b.WriteString(fmt.Sprintf("\n%s\n", baselineFileSummary(path, rows, shown)))

	fmt.Print(b.String())
	return nil
}

func shortVersion(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func baselineFileSummary(path string, all, shown []baselineRow) string {
	var b strings.Builder
	first, last := "", ""
	if len(all) > 0 {
		first = all[0].TS
		last = all[len(all)-1].TS
	}
	size := int64(0)
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	fmt.Fprintf(&b, "  first: %s\n  last:  %s\n  size:  %d bytes, %d samples on disk, showing %d\n",
		baselineOrDash(first), baselineOrDash(last), size, len(all), len(shown))
	return b.String()
}

func baselineOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// cmdBaselineCompare splits the record at the boundary the operator asked for
// and reports both sides.
func cmdBaselineCompare(rows []baselineRow, opts baselineOpts) error {
	if opts.ramp == 0 {
		opts.ramp = baselineDefaultRamp
	}
	starts := rampStarts(rows)
	before, after, boundary, err := splitAtBoundary(rows, opts.from, opts.to)
	if err != nil {
		return err
	}
	before = excludeRamp(before, starts, opts.ramp)
	after = excludeRamp(after, starts, opts.ramp)
	res := compareSegments(before, after)
	if opts.asJSON {
		// The boundary is part of the answer, so it goes in the JSON rather
		// than on a line above it, which would make the output unparseable.
		res.Boundary = boundary
		out, err := renderCompare(res, true)
		if err != nil {
			return err
		}
		fmt.Println(out)
		return nil
	}
	out, err := renderCompare(res, false)
	if err != nil {
		return err
	}
	fmt.Printf("boundary: %s\n\n", boundary)
	fmt.Print(out)
	return nil
}

// --- the commands ---

const (
	baselineDefaultShow = 20
	baselineMaxShow     = 200
	// baselineDefaultRamp is how long after a start the pool is treated as still
	// ramping. Twenty minutes is the default the brief names.
	baselineDefaultRamp = 20 * time.Minute
)

func newBaselineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "baseline [show|mark|compare] [flags]",
		Short: "Show this box's recorded baseline, and compare an upgrade against it",
		Long: "A provider records a small local time series of how it behaved, so any " +
			"upgrade can be judged against what the box did BEFORE it. Nothing external " +
			"is involved: no Prometheus, no Grafana, no sampler over ssh.\n\n" +
			"  show      the newest samples as a table\n" +
			"  mark      annotate the timeline, for example just before an upgrade\n" +
			"  compare   before and after the last upgrade, or between two marks\n\n" +
			"The record lives in ~/.urnetwork/baseline.jsonl and holds counts and totals " +
			"only: no proxy addresses, usernames or passwords.",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdBaseline(args)
		},
	}
}

// parseBaselineArgs splits the subcommand and its flags. The target flags are
// parsed the same way as every other command, so `--user` and friends work.
func parseBaselineArgs(args []string) (sub string, opts baselineOpts, err error) {
	if len(args) == 0 {
		return "", opts, fmt.Errorf("usage: urnet-tools baseline [show|mark|compare] [flags]")
	}
	sub = args[0]
	switch sub {
	case "show", "mark", "compare":
	default:
		return "", opts, fmt.Errorf("unknown subcommand %q (usage: urnet-tools baseline "+
			"[show|mark|compare])", sub)
	}
	rest := args[1:]
	for len(rest) > 0 {
		// Normalize value-taking options so both spellings share validation.
		switch rest[0] {
		case "-n", "--from", "--to", "--skip-ramp":
			if len(rest) < 2 {
				return "", opts, fmt.Errorf("%s requires a value", rest[0])
			}
			rest = append([]string{rest[0] + "=" + rest[1]}, rest[2:]...)
		}
		switch {
		case rest[0] == "--json":
			opts.asJSON = true
			rest = rest[1:]
		case strings.HasPrefix(rest[0], "--skip-ramp="):
			d, perr := time.ParseDuration(strings.TrimPrefix(rest[0], "--skip-ramp="))
			if perr != nil || d < 0 {
				return "", opts, fmt.Errorf("--skip-ramp must be a duration (got %q)",
					strings.TrimPrefix(rest[0], "--skip-ramp="))
			}
			opts.ramp = d
			rest = rest[1:]
		case strings.HasPrefix(rest[0], "-n="):
			n, cerr := strconv.Atoi(strings.TrimPrefix(rest[0], "-n="))
			if cerr != nil || n <= 0 {
				return "", opts, fmt.Errorf("-n must be a positive integer (got %q)",
					strings.TrimPrefix(rest[0], "-n="))
			}
			opts.limit = n
			rest = rest[1:]
		case strings.HasPrefix(rest[0], "--from="):
			opts.from = strings.TrimPrefix(rest[0], "--from=")
			rest = rest[1:]
		case strings.HasPrefix(rest[0], "--to="):
			opts.to = strings.TrimPrefix(rest[0], "--to=")
			rest = rest[1:]
		default:
			t, remaining, terr := parseTargetFlags(rest)
			if terr != nil {
				return "", opts, terr
			}
			opts.target = t
			opts.positional = remaining
			rest = nil
		}
	}
	return sub, opts, nil
}

type baselineOpts struct {
	target     Target
	limit      int
	ramp       time.Duration
	asJSON     bool
	from, to   string
	positional []string
}

func cmdBaseline(args []string) error {
	sub, opts, err := parseBaselineArgs(args)
	if err != nil {
		return err
	}
	switch sub {
	case "mark":
		return cmdBaselineMark(opts)
	default:
		// show and compare read the file directly, so they work on a box whose
		// provider is stopped. That is the whole point of a local record: after a
		// bad upgrade that will not start, the evidence is still there.
		path, err := baselineLocalPath(opts.target)
		if err != nil {
			return err
		}
		rows, err := readBaselineFile(path)
		if err != nil {
			return err
		}
		switch sub {
		case "show":
			return cmdBaselineShow(path, rows, opts)
		case "compare":
			return cmdBaselineCompare(rows, opts)
		}
	}
	return nil
}

func cmdBaselineMark(opts baselineOpts) error {
	if len(opts.positional) == 0 {
		return fmt.Errorf("usage: urnet-tools baseline mark <label>")
	}
	label := strings.Join(opts.positional, " ")
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("a mark needs a label")
	}
	p, err := selectTarget(Discover(), opts.target)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	resp, err := sendSocketRequest(filepath.Join(p.StateDir, "provider.sock"),
		controlRequest{Cmd: "mark", Value: label})
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("mark failed: %s", resp.Error)
	}
	fmt.Printf("marked %s at %s\n", label, resp.Value)
	return nil
}

// baselineLocalPath resolves explicit targets through local discovery, including
// stopped providers. Only an untargeted read falls back to the caller's HOME.
func baselineLocalPath(t Target) (string, error) {
	if t.StateDir != "" {
		return filepath.Join(t.StateDir, "baseline.jsonl"), nil
	}
	if t != (Target{}) {
		p, err := selectTarget(Discover(), t)
		if err != nil {
			return "", err
		}
		if p.StateDir == "" {
			return "", fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
		}
		return filepath.Join(p.StateDir, "baseline.jsonl"), nil
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "", fmt.Errorf("cannot resolve the state directory: HOME is not set")
	}
	return filepath.Join(home, ".urnetwork", "baseline.jsonl"), nil
}

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

// rampStarts collects starts from the complete record, so a ramp crossing a
// comparison boundary is excluded on both sides.
func rampStarts(rows []baselineRow) []time.Time {
	var starts []time.Time
	for _, r := range rows {
		if r.Kind != "start" {
			continue
		}
		if at, ok := r.timeAt(); ok {
			starts = append(starts, at)
		}
	}
	return starts
}

// excludeRamp drops rows within skip after any recorded start. Start rows stay
// available for restart counts; rows with unusable timestamps are also kept.
func excludeRamp(rows []baselineRow, starts []time.Time, skip time.Duration) []baselineRow {
	if skip <= 0 || len(starts) == 0 {
		return rows
	}
	out := make([]baselineRow, 0, len(rows))
	for _, r := range rows {
		at, ok := r.timeAt()
		if !ok {
			// A row with no usable timestamp cannot be placed relative to the
			// ramp. Keep it rather than silently dropping data.
			out = append(out, r)
			continue
		}
		if r.Kind == "start" || !inRamp(at, starts, skip) {
			out = append(out, r)
		}
	}
	return out
}

// inRamp reports whether at falls within skip after any of the start marks.
func inRamp(at time.Time, starts []time.Time, skip time.Duration) bool {
	for _, s := range starts {
		if !at.Before(s) && at.Before(s.Add(skip)) {
			return true
		}
	}
	return false
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
	if len(rows) == 0 {
		return s
	}
	var upSum, clientSum, rssSum, psiSum float64
	var upN, clientN, rssN, psiN int
	var firstOOM, lastOOM int64
	haveOOM := false
	for _, r := range rows {
		// A start mark inside a segment is a restart, which is what the reader
		// needs to see: a rate computed across a restart mixes two processes.
		if r.Kind == "start" {
			s.Restarts++
		}
		if r.Kind != "sample" {
			continue
		}
		s.Rows++
		if r.OOM != nil {
			if !haveOOM {
				firstOOM, haveOOM = r.OOM.Kills, true
			}
			lastOOM = r.OOM.Kills
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
	if haveOOM && lastOOM >= firstOOM {
		s.OOMKillDelta = lastOOM - firstOOM
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
	// Boundary names the split that produced the two sides, so a reader of the
	// JSON knows what was compared without re-deriving it.
	Boundary string       `json:"boundary"`
	Before   segmentStats `json:"before"`
	After    segmentStats `json:"after"`
	// Warnings are the one-line caveats: a capacity change or a change of
	// availability source makes the two sides not like for like, and a reader
	// who is not told will read a trimmed box as a regression.
	Warnings []string `json:"warnings,omitempty"`
	// Insufficient is set when either side is below the sample floor.
	Insufficient string       `json:"insufficient_data,omitempty"`
	Rows         []compareRow `json:"rows"`
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

// splitAtBoundary divides the record into the two sides a comparison reports.
//
// A boundary is a mark label, a timestamp, or the special value "last-update",
// which means the most recent start whose version differs from the previous one:
// the segment BEFORE it is "before" and the segment AFTER it is "after". That
// default is what makes `baseline compare` useful with no arguments at all,
// which is the case an operator actually runs after an upgrade.
func splitAtBoundary(rows []baselineRow, from, to string) (before, after []baselineRow, boundary string, err error) {
	var fromIdx, toIdx int
	if len(rows) == 0 {
		return nil, nil, "", fmt.Errorf("the baseline record is empty")
	}
	fromIdx, toIdx, boundary, err = resolveBoundary(rows, from, to)
	if err != nil {
		return nil, nil, "", err
	}
	// The split is at the boundary: everything before it is "before", and
	// everything from it onward is "after". rows[toIdx:] would be empty
	// whenever toIdx is the end of the record, which is the default, so the
	// after side has to start at fromIdx and toIdx only caps it when --to
	// was given.
	after = rows[fromIdx:]
	if toIdx > fromIdx && toIdx < len(rows) {
		after = rows[fromIdx:toIdx]
	}
	return rows[:fromIdx], after, boundary, nil
}

// resolveBoundary turns the --from and --to arguments into row indexes.
func resolveBoundary(rows []baselineRow, from, to string) (fromIdx, toIdx int, label string, err error) {
	// Default: the last upgrade. A start whose version differs from the one
	// before it is the boundary; a restart of the SAME version is not an
	// upgrade and must not split the comparison.
	boundaryIdx := -1
	for i, r := range rows {
		if r.Kind != "start" || r.PreviousVersion == "" {
			continue
		}
		if r.Version != r.PreviousVersion {
			boundaryIdx = i
		}
	}
	if boundaryIdx < 0 {
		// No upgrade recorded: fall back to the midpoint so the command still
		// says something useful rather than erroring out.
		mid := len(rows) / 2
		if mid == 0 {
			return 0, len(rows), "no upgrade recorded; showing the whole record", nil
		}
		return mid, len(rows), "no upgrade recorded; comparing the two halves of the record", nil
	}
	fromIdx = boundaryIdx
	toIdx = len(rows)
	label = fmt.Sprintf("last update at %s (%s -> %s)", rows[boundaryIdx].TS,
		rows[boundaryIdx].PreviousVersion, rows[boundaryIdx].Version)

	// An explicit --from moves the boundary to a mark label or a timestamp.
	if from != "" {
		idx, ok := findBoundaryRow(rows, from)
		if !ok {
			return 0, 0, "", fmt.Errorf("no mark or timestamp matching %q; "+
				"marks are recorded by `baseline mark <label>`", from)
		}
		fromIdx, label = idx, "from "+from
	}
	// An explicit --to ends the "after" side early.
	if to != "" {
		idx, ok := findBoundaryRow(rows, to)
		if !ok {
			return 0, 0, "", fmt.Errorf("no mark or timestamp matching %q", to)
		}
		toIdx = idx
	}
	return fromIdx, toIdx, label, nil
}

// findBoundaryRow matches a mark label exactly, or a timestamp that prefixes the
// row's own, so "2026-09-30T12" finds the 12:00 rows without the operator having
// to type the seconds.
func findBoundaryRow(rows []baselineRow, want string) (int, bool) {
	for i, r := range rows {
		if r.Kind == "mark" && r.Label == want {
			return i, true
		}
	}
	for i, r := range rows {
		if r.TS != "" && strings.HasPrefix(r.TS, want) {
			return i, true
		}
	}
	return 0, false
}
