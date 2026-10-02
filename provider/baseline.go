package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// baseline.go records a small, bounded, local time series of how this box
// actually behaved, so every future upgrade has a before-and-after to compare
// against without anyone remembering to capture one by hand.
//
// The rules that matter, in order:
//
//   - An unknown field is OMITTED, never written as zero. A zero reads as
//     "measured, and the answer was nothing", which for host memory or PSI
//     would be a catastrophic misreading of a perfectly healthy box. This is
//     strict for the host readers, which can fail. The NodeSnapshot-derived
//     fields (rate, pressure, contracts, resources) carry no unknown marker, so
//     there a zero is omitted and an absent field means "zero or unknown".
//   - Recording never blocks or fails the thing it observes. Write errors are
//     ignored, except for one rate-limited line so a broken disk is visible
//     rather than silent.
//   - No proxy addresses, usernames or passwords ever reach the file. It holds
//     counts and totals, nothing that identifies a proxy or a customer.
//
// The file is the same shape as the autopilot ledger: append-only JSONL, bounded
// to the newest whole lines, with the same inter-process lock, because during a
// HotSwap the parent and the candidate both record and an unlocked rotation
// would clobber the shared temp file.

const (
	baselineFileName = "baseline.jsonl"

	// baselineMaxBytes is the cap on the file. At roughly 600 bytes a sample,
	// 1 MiB is about 1,700 samples, which at one per 15 minutes is about
	// 17 days: long enough to cover a slow upgrade cycle with room to spare.
	baselineMaxBytes = 1 << 20

	// baselineInterval is the steady-state cadence. baselineFirstSampleDelay is
	// longer than nothing because a sample at t=0 measures a pool that has not
	// launched yet, and those first minutes are exactly the ramp the compare
	// logic excludes from both sides.
	baselineInterval         = 15 * time.Minute
	baselineFirstSampleDelay = 5 * time.Minute

	// baselineMinInterval is the floor on the env override, so a typo or a
	// canary run cannot turn the recorder into a busy loop.
	baselineMinInterval = time.Minute

	// baselineErrorTopN caps the error taxonomy in one sample. The categories
	// are unbounded in principle; the file is not.
	baselineErrorTopN = 8
)

// errNoHostData marks a host reader that could not produce a value. Callers
// omit the field rather than writing a zero.
var (
	errNoHostData = errors.New("baseline: no host data")
	errNoStateDir = errors.New("no state directory")
)

// baselineSample is one line of the file. Every optional field is a pointer or
// carries omitempty, so "unknown" and "zero" stay distinguishable on disk.
type baselineSample struct {
	V               int    `json:"v"`
	Kind            string `json:"kind"` // start | sample | mark
	Label           string `json:"label,omitempty"`
	TS              string `json:"ts"`
	Version         string `json:"version,omitempty"`
	PreviousVersion string `json:"previous_version,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	UptimeSeconds   *int64 `json:"uptime_seconds,omitempty"`

	State       string `json:"state,omitempty"`
	StateReason string `json:"state_reason,omitempty"`

	Proxies baselineProxies `json:"proxies"`
	// Desired is the configured pool size, and TrimCap the effective cap with
	// its source, so a later comparison can see that capacity changed rather
	// than reading a trimmed box as a regression.
	Desired       *int   `json:"desired,omitempty"`
	TrimCap       *int   `json:"trim_cap,omitempty"`
	TrimCapSource string `json:"trim_cap_source,omitempty"`

	Clients  int64            `json:"clients"`
	Sessions baselineSessions `json:"sessions"`
	Rate     baselineRate     `json:"rate"`

	Traffic   *baselineTraffic   `json:"traffic,omitempty"`
	Contracts *baselineContracts `json:"contracts,omitempty"`
	Pressure  *float64           `json:"pressure,omitempty"`

	Resources baselineResources `json:"resources"`
	Host      *baselineHost     `json:"host,omitempty"`
	// GC and Net are observation-only signals (see resource_signals.go). Pointers
	// so a box that could read neither writes no block rather than an empty one.
	GC  *baselineGC  `json:"gc,omitempty"`
	Net *baselineNet `json:"net,omitempty"`

	// Pointers because encoding/json's omitempty does nothing for a struct
	// value: a plain struct would serialise as {} even when nothing was known,
	// which is indistinguishable from a real reading of zero.
	OOM     *baselineOOM     `json:"oom,omitempty"`
	Restart *baselineRestart `json:"restart,omitempty"`
	Errors  []baselineError  `json:"errors,omitempty"`
}

type baselineProxies struct {
	Up         int `json:"up"`
	Degraded   int `json:"degraded"`
	Connecting int `json:"connecting"`
	Dead       int `json:"dead"`
}

type baselineSessions struct {
	PQE       int64 `json:"pqe"`
	Classical int64 `json:"classical"`
}

type baselineRate struct {
	Avg5mBps *int64 `json:"avg5m_bps,omitempty"`
}

type baselineTraffic struct {
	BillableBytes         uint64  `json:"billable_bytes"`
	TotalBytes            uint64  `json:"total_bytes"`
	LifetimeBillableBytes *uint64 `json:"lifetime_billable_bytes,omitempty"`
}

type baselineContracts struct {
	Acquired int64 `json:"acquired"`
	Denied   int64 `json:"denied"`
}

type baselineResources struct {
	RSSBytes       *uint64 `json:"rss_bytes,omitempty"`
	HeapInuseBytes *uint64 `json:"heap_inuse_bytes,omitempty"`
	MemLimitBytes  *uint64 `json:"mem_limit_bytes,omitempty"`
	Goroutines     *int    `json:"goroutines,omitempty"`
	OpenFDs        *uint64 `json:"open_fds,omitempty"`
	FDLimit        *uint64 `json:"fd_limit,omitempty"`
}

// baselineHost is the host-level view. MemAvailableMiB is the TIGHTER of host
// and cgroup availability (brief #2074): in Docker /proc/meminfo shows the
// host's free RAM, so a container near its limit would otherwise read healthy
// and a memory regression would be invisible on exactly those boxes.
// AvailSource says which reader supplied it, so a comparison across a systemd
// box and a Docker box is not read as like-for-like.
type baselineHost struct {
	MemAvailableMiB *int64   `json:"mem_available_mib,omitempty"`
	AvailSource     string   `json:"avail_source,omitempty"`
	SwapUsedMiB     *int64   `json:"swap_used_mib,omitempty"`
	PSISomeAvg60    *float64 `json:"psi_some_avg60,omitempty"`
	PSIFullAvg60    *float64 `json:"psi_full_avg60,omitempty"`
	PSIFullAvg300   *float64 `json:"psi_full_avg300,omitempty"`
	Load1           *float64 `json:"load1,omitempty"`
}

// baselineOOM is the OOM-kill counter for the scope it was read from. Scope
// matters: the parent cgroup's subtree counter also covers other services and,
// under the systemd Docker driver, other containers.
type baselineOOM struct {
	Kills int64  `json:"kills"`
	Scope string `json:"scope,omitempty"`
}

type baselineRestart struct {
	Reason        string `json:"reason,omitempty"`
	CleanShutdown bool   `json:"clean_shutdown"`
}

type baselineError struct {
	Category string `json:"category"`
	Count    uint64 `json:"count"`
}

// baselineInputs is everything the sample builder needs, passed in rather than
// read from the process, so the builder is a pure function and its tests need
// no clock, no goroutines and no fixtures on disk.
type baselineInputs struct {
	now  time.Time
	snap *NodeSnapshot

	host      baselineHost
	hostKnown bool
	hostErr   error

	desired int
	trimCap int
	trimSrc string

	oom baselineOOM

	gc  *baselineGC
	net *baselineNet

	contracts [2]int64 // acquired, denied
	errs      map[string]uint64
	errsKnown bool
}

// buildBaselineSample turns its inputs into one line. It reads no clock and
// touches no file, which is what makes the omission rules testable.
func buildBaselineSample(in baselineInputs) baselineSample {
	s := baselineSample{V: 1, Kind: "sample", TS: in.now.UTC().Format(time.RFC3339)}
	if in.snap == nil {
		return s
	}
	snap := in.snap
	s.Version = snap.Version
	s.PreviousVersion = snap.PreviousVersion
	if !snap.StartedAt.IsZero() {
		s.StartedAt = snap.StartedAt.UTC().Format(time.RFC3339)
	}
	if snap.UptimeSeconds != 0 {
		u := int64(snap.UptimeSeconds)
		s.UptimeSeconds = &u
	}
	s.State = snap.State
	s.StateReason = snap.StateReason
	s.Proxies = baselineProxies{
		Up:         snap.Proxies.Up,
		Degraded:   snap.Proxies.Degraded,
		Connecting: snap.Proxies.Connecting,
		Dead:       snap.Proxies.Dead,
	}
	s.Clients = snap.Clients
	s.Sessions = baselineSessions{PQE: snap.Sessions.PQE, Classical: snap.Sessions.Classical}
	if snap.Rate.Avg5mBps != 0 {
		r := snap.Rate.Avg5mBps
		s.Rate.Avg5mBps = &r
	}
	if snap.Traffic != nil {
		t := &baselineTraffic{
			BillableBytes: snap.Traffic.BillableBytes,
			TotalBytes:    snap.Traffic.TotalBytes,
		}
		if snap.Traffic.LifetimeBillableBytes != nil {
			l := *snap.Traffic.LifetimeBillableBytes
			t.LifetimeBillableBytes = &l
		}
		s.Traffic = t
	}
	if in.contracts != [2]int64{} {
		s.Contracts = &baselineContracts{Acquired: in.contracts[0], Denied: in.contracts[1]}
	}
	if snap.Pressure != 0 {
		p := snap.Pressure
		s.Pressure = &p
	}
	// Resources: a zero that was actually measured is kept, an unknown is not
	// set at all. NodeSnapshot already uses omitempty for most of these, so a
	// zero there means zero rather than unknown, and copying it is correct.
	s.Resources = baselineResources{
		Goroutines:     intPtrOrNil(snap.Resources.Goroutines, snap.Resources.Goroutines != 0),
		RSSBytes:       uint64PtrOrNil(snap.Resources.RSSBytes, snap.Resources.RSSBytes != 0),
		HeapInuseBytes: uint64PtrOrNil(snap.Resources.HeapInuseBytes, snap.Resources.HeapInuseBytes != 0),
		MemLimitBytes:  uint64PtrOrNil(snap.Resources.MemLimitBytes, snap.Resources.MemLimitBytes != 0),
		OpenFDs:        uint64PtrOrNil(snap.Resources.OpenFDs, snap.Resources.OpenFDs != 0),
		FDLimit:        uint64PtrOrNil(snap.Resources.FDLimit, snap.Resources.FDLimit != 0),
	}
	if in.hostKnown {
		h := in.host
		s.Host = &h
	}
	if in.oom.Kills != 0 || in.oom.Scope != "" {
		o := in.oom
		s.OOM = &o
	}
	s.GC = in.gc
	s.Net = in.net
	// Restart always describes this start, so it is known even when the reason
	// is empty: clean_shutdown is the load-bearing field.
	s.Restart = &baselineRestart{Reason: snap.Restart.Reason, CleanShutdown: snap.Restart.CleanShutdown}
	if in.errsKnown {
		s.Errors = topBaselineErrors(in.errs, baselineErrorTopN)
	}
	if in.desired != 0 {
		d := in.desired
		s.Desired = &d
	}
	if in.trimCap != 0 {
		c := in.trimCap
		s.TrimCap = &c
		s.TrimCapSource = in.trimSrc
	}
	return s
}

func intPtrOrNil(v int, known bool) *int {
	if !known {
		return nil
	}
	return &v
}

func uint64PtrOrNil(v uint64, known bool) *uint64 {
	if !known {
		return nil
	}
	return &v
}

// topBaselineErrors keeps the N largest counts, descending, so the file cannot
// grow with the error taxonomy and the most useful rows survive the trim.
func topBaselineErrors(m map[string]uint64, n int) []baselineError {
	if len(m) == 0 {
		return nil
	}
	out := make([]baselineError, 0, len(m))
	for k, v := range m {
		out = append(out, baselineError{Category: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		// A stable tie-break keeps the output deterministic across runs, which
		// matters for a diffable file.
		return out[i].Category < out[j].Category
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// baselineStartSample is written at launch and is what compare finds an
// upgrade by.
func baselineStartSample(version, previous string, now time.Time) baselineSample {
	s := baselineSample{
		V:               1,
		Kind:            "start",
		TS:              now.UTC().Format(time.RFC3339),
		Version:         version,
		PreviousVersion: previous,
	}
	return s
}

// baselineMarkSample is the operator's own annotation. It deliberately carries
// no measurements: numbers on a mark would be read as observations.
func baselineMarkSample(label string, now time.Time) baselineSample {
	return baselineSample{
		V:     1,
		Kind:  "mark",
		Label: label,
		TS:    now.UTC().Format(time.RFC3339),
	}
}

// resolveBaselineInterval reads the env override with a floor, so a canary can
// sample faster but nothing can turn the recorder into a busy loop.
func resolveBaselineInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("URNETWORK_BASELINE_INTERVAL"))
	if v == "" {
		return baselineInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		// Unparseable falls back to the default rather than the floor: a typo
		// must not silently turn a 15-minute recorder into a 1-minute one.
		return baselineInterval
	}
	if d < baselineMinInterval {
		return baselineMinInterval
	}
	return d
}

// baselineAppend adds one sample, trimming to the newest whole lines when the
// file passes the bound. The inter-process lock is not optional: during a
// HotSwap the parent and the candidate both record, and an unlocked rotation
// can clobber the shared temp file or drop an entry appended after its
// snapshot. maxBytes is a parameter rather than the constant so a test can prove the
// trim with a 200-byte file instead of writing a megabyte.
func baselineAppend(path string, s baselineSample, maxBytes int64) error {
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	release, err := acquireJWTStoreLock(path)
	if err != nil {
		return err
	}
	defer release()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxBytes {
		return baselineTrim(path, maxBytes/2)
	}
	return nil
}

// baselineTrim keeps the newest whole lines totalling at most keepBytes.
func baselineTrim(path string, keepBytes int64) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	start := len(b)
	var size int64
	for i := len(b) - 1; i >= 0; {
		j := i - 1
		for j >= 0 && b[j] != '\n' {
			j--
		}
		lineLen := int64(i - j)
		if size+lineLen > keepBytes {
			// The newest line alone is over the bound. Without this guard
			// start stays len(b) and we would write an EMPTY file, silently
			// destroying the whole record. Over the bound for one entry beats
			// losing all of them. j is the index of the newline BEFORE this
			// line, so j+1 is where it starts.
			if size == 0 {
				start = j + 1
			}
			break
		}
		size += lineLen
		start = j + 1
		i = j
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b[start:], 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// baselineTail returns the newest n samples in chronological order. A missing
// file is empty and a line that does not parse is skipped: a box that was
// killed mid-write must not hide every other sample.
func baselineTail(path string, n int) ([]baselineSample, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []baselineSample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s baselineSample
		if json.Unmarshal(line, &s) == nil && s.Kind != "" {
			all = append(all, s)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

// baselinePath returns the recorder's file, or "" when the state directory
// cannot be resolved.
func baselinePath() string { return baselinePathFn() }

// baselinePathFn is a variable so a test can simulate an unresolvable state
// directory; production never assigns it.
var baselinePathFn = func() string {
	dir, err := oomCapDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, baselineFileName)
}

// baselineEnabled is the live control state. Default on: the whole point is a
// free baseline without anyone remembering to turn it on.
var baselineEnabled atomic.Bool

func init() { baselineEnabled.Store(true) }

func baselineIsEnabled() bool { return baselineEnabled.Load() }

// baselineWriteErrorHook, when set by a test, runs on a failed write so a test
// can assert the rate-limited warning without provoking a real disk failure.
var baselineWriteErrorHook func()

// baselineWarnOnce is the rate limiter for the "cannot write" line. Recording
// must never fail the thing it records, so the error is swallowed, but a
// silently broken recorder is worse than a loud one: an operator comparing an
// upgrade would find an empty file and blame the upgrade.
var baselineWarnLast atomic.Int64

func baselineRecord(e baselineSample) {
	if !baselineIsEnabled() {
		return
	}
	if baselineWriteErrorHook != nil {
		baselineWriteErrorHook()
	}
	path := baselinePath()
	if path == "" {
		baselineWarnWrite(errNoStateDir)
		return
	}
	if err := baselineAppend(path, e, baselineMaxBytes); err != nil {
		baselineWarnWrite(err)
	}
}

// baselineNowUnix is the clock the write-warning rate limiter reads. It is a
// variable so a test can drive the limiter deterministically; nothing else
// depends on it and production never assigns it.
var baselineNowUnix = func() int64 { return time.Now().Unix() }

// baselineWarnHook, when set by a test, runs once per warning actually
// EMITTED (after the rate limiter has decided to allow it). The
// baselineWriteErrorHook above fires on every attempt, which is a different
// question: one is "a write was tried", this is "an operator was told".
var baselineWarnHook func()

// baselineWarnWrite logs at most one line per hour. A full disk would otherwise
// produce one warning per sample forever.
func baselineWarnWrite(err error) {
	now := baselineNowUnix()
	last := baselineWarnLast.Load()
	if now-last < 3600 {
		return
	}
	if !baselineWarnLast.CompareAndSwap(last, now) {
		return
	}
	if baselineWarnHook != nil {
		baselineWarnHook()
	}
	if err != nil {
		importantLogf("[baseline] cannot write %s: %v; recording is off until this clears\n",
			baselineFileName, err)
		return
	}
	importantLogf("[baseline] cannot write %s: no state directory; recording is off until this clears\n",
		baselineFileName)
}

// baselineStart writes the start mark and launches the sampler. It is called
// from the provider launcher, and the wiring test asserts that. The sampler is
// launched even when the key is off: it checks the flag on every tick, so
// `set baseline on` takes effect live. Returns a channel closed when it exits.
func baselineStart(ctx context.Context, version, previous string) <-chan struct{} {
	// baselineRecord is a no-op while the key is off, so no start mark is written.
	baselineRecord(baselineStartSample(version, previous, time.Now()))
	done := make(chan struct{})
	go baselineRun(ctx, done)
	return done
}

// baselineRun samples until ctx is cancelled. The first sample is delayed, so a
// sample at t=0 does not measure a pool that has not launched yet.
func baselineRun(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	interval := resolveBaselineInterval()
	first := time.NewTimer(baselineFirstSampleDelay)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	baselineRecord(buildBaselineSample(baselineCollect(time.Now())))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			baselineRecord(buildBaselineSample(baselineCollect(t)))
		}
	}
}

// baselineMark writes an operator mark through the same path, and is the only
// way anything outside the provider adds a line: the provider is the single
// writer.
func baselineMark(label string) (time.Time, error) {
	now := time.Now()
	path := baselinePath()
	if path == "" {
		// An empty path would open a file relative to the working directory.
		return time.Time{}, errNoStateDir
	}
	if err := baselineAppend(path, baselineMarkSample(label, now), baselineMaxBytes); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

// baselineCollect gathers one sample's worth of inputs. Every host reader is
// best-effort: a reader that fails omits its field rather than zeroing it.
func baselineCollect(now time.Time) baselineInputs {
	in := baselineInputs{now: now, snap: nodeSnapshots.Get()}

	host, err := readBaselineHost()
	in.host, in.hostKnown, in.hostErr = host, err == nil, err

	if cap, src, cerr := effectiveTrimCapSource(); cerr == nil {
		in.trimCap = cap
		in.trimSrc = src
	}
	in.desired = configuredProxyCount()

	scope, kills := readOOMKillEpoch()
	in.oom = baselineOOM{Kills: kills, Scope: scope}

	in.gc, in.net = readBaselineGC(now), readBaselineNet()

	acquired, denied := globalContractMetrics.totals()
	in.contracts = [2]int64{acquired, denied}

	in.errs, in.errsKnown = snapshotErrors(), true
	return in
}

// baselineHostRaw is the raw result of every host reader, before any
// omission decision. Each field is nil when its reader failed. Keeping the
// readings separate from the decision is what makes the omission rule testable
// without a real /proc, and it is what stops one failed reader from discarding
// the others.
type baselineHostRaw struct {
	HostMiB    int64 // -1 when unreadable
	CgroupMiB  int64 // -1 when there is no cgroup limit
	SwapUsed   *int64
	PSISome60  *float64
	PSIFull60  *float64
	PSIFull300 *float64
	Load1      *float64
}

// readBaselineHost gathers the host-level readings. Every reader is
// best-effort and independent: a box with no cgroup limit still records host
// memory, and a box with an unreadable /proc/meminfo still records PSI and
// load. Only the memory fields are dropped when BOTH memory readers fail.
func readBaselineHost() (baselineHost, error) {
	raw := baselineHostRaw{
		HostMiB:   readMemAvailableMiB(),
		CgroupMiB: readCgroupAvailableMiB(),
	}
	if used, ok := readSwapUsedMiB(); ok {
		raw.SwapUsed = &used
	}
	if some, err := readPSI("some"); err == nil {
		raw.PSISome60 = &some
	}
	if full60, full300, err := readPSIFull(); err == nil {
		raw.PSIFull60, raw.PSIFull300 = &full60, &full300
	}
	if l1, _, err := getSystemLoad(); err == nil {
		raw.Load1 = &l1
	}
	return buildBaselineHost(raw)
}

// buildBaselineHost applies the omission rules to the raw readings. Per brief
// #2074 mem_available_mib is the TIGHTER of host and cgroup, with avail_source
// naming which supplied it and host winning a tie. When neither reader worked,
// the memory fields are omitted but everything else still stands: losing PSI
// because /proc/meminfo was unreadable would be a silent gap in exactly the
// situation an operator is trying to diagnose.
//
// The comparisons are >= 0, following readMemAvailFrac: a real reading of 0
// means memory is exhausted, and treating that as no-data would hide the case
// that matters most. Both readers return -1 on error, so 0 is real data.
func buildBaselineHost(raw baselineHostRaw) (baselineHost, error) {
	h := baselineHost{
		SwapUsedMiB:   raw.SwapUsed,
		PSISomeAvg60:  raw.PSISome60,
		PSIFullAvg60:  raw.PSIFull60,
		PSIFullAvg300: raw.PSIFull300,
		Load1:         raw.Load1,
	}
	host, cgroup := raw.HostMiB, raw.CgroupMiB
	switch {
	case host >= 0 && cgroup >= 0:
		v, src := host, "host"
		if cgroup < host {
			v, src = cgroup, "cgroup"
		}
		h.MemAvailableMiB, h.AvailSource = &v, src
	case host >= 0:
		h.MemAvailableMiB, h.AvailSource = &host, "host"
	case cgroup >= 0:
		h.MemAvailableMiB, h.AvailSource = &cgroup, "cgroup"
	default:
		// No memory reading at all: omit those two fields, keep the rest.
		return h, errNoHostData
	}
	return h, nil
}

// readSwapUsedMiB reads /proc/meminfo. There was no existing reader, so this is
// new: SwapTotal minus SwapFree, and omitted when either is unreadable.
func readSwapUsedMiB() (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var total, free int64
	var haveTotal, haveFree bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		var dst *int64
		var have *bool
		switch {
		case strings.HasPrefix(line, "SwapTotal:"):
			dst, have = &total, &haveTotal
		case strings.HasPrefix(line, "SwapFree:"):
			dst, have = &free, &haveFree
		default:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// /proc/meminfo reports kB.
		if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			*dst = v / 1024
			*have = true
		}
	}
	if !haveTotal || !haveFree {
		return 0, false
	}
	return total - free, true
}

// readPSIFull returns the memory "full" line's avg60 and avg300. The "full" line
// lives in the resource files (/proc/pressure/memory); there is no
// /proc/pressure/full. readPSI only parses "some", and the baseline needs both
// the full-pressure stall time and its longer window, so this is a separate
// reader rather than a widened one.
func readPSIFull() (avg60, avg300 float64, err error) {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0, 0, err
	}
	return parsePSIFull(string(b))
}

// parsePSIFull parses the "full" line. Mirrors parsePSISome, which covers only
// "some" and only avg60.
func parsePSIFull(content string) (avg60, avg300 float64, err error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch k {
			case "avg60":
				avg60, err = strconv.ParseFloat(v, 64)
			case "avg300":
				avg300, err = strconv.ParseFloat(v, 64)
			}
			if err != nil {
				return 0, 0, err
			}
		}
		return avg60, avg300, nil
	}
	return 0, 0, fmt.Errorf("no 'full' line in PSI content")
}

// configuredProxyCount reads back what setConfiguredProxyCount stored.
func configuredProxyCount() int {
	return int(proxiesConfigured.Load())
}

// goroutineCount is here so the resources block can report a real number on
// every platform; runtime is imported for it.
var goroutineCount = runtime.NumGoroutine

// currentVersionForBaseline is the running build, with the same "dev"
// fallback the control socket's version command uses so a build without
// -ldflags still records something readable.
func currentVersionForBaseline() string {
	if v := RequireVersion(); v != "" {
		return v
	}
	return "dev"
}

// previousVersionForBaseline is the version this provider replaced, read
// through the same locked accessor the node snapshot uses. Empty on a first
// start, which is correct: there was no previous version.
func previousVersionForBaseline() string {
	startupDiag.mu.Lock()
	defer startupDiag.mu.Unlock()
	return startupDiag.previousVersion
}
