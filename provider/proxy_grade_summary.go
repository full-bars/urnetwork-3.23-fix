package main

// Proxy grade summary logging (design 2026-08-09).
//
// A periodic snapshot of the RUNNING proxy set bucketed by A-F tier, a
// per-source breakdown, the changes vs the previous round, score stats,
// and the next-probe countdown. The snapshot rides its own ticker (default
// 5m) and is PURE-READ — it never probes, so it is safe on boxes with tens
// of thousands of proxies.
//
// Persistence model:
//   - Snapshot + delta lines go through importantLogf (ramlog + important
//     buffer + disk events.log) AND the dedicated grades.log (daily files,
//     retention_days of per-proxy history).
//   - The next-probe countdown goes to the regular ramlog ONLY (tlog), so
//     it can run as often as desired without crowding the important/disk
//     logs.
//
// Settings live in ~/.urnetwork/proxy_grades.json and are re-read live
// (mtime-invalidated cache, same pattern as proxy_probe.json) so operators
// can change them without a restart — urnet-tools writes the file, the
// provider picks it up on the next tick.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultGradeSummaryInterval = 5 * time.Minute
	defaultGradesRetentionDays  = 7
)

// proxyGradesConfig is the runtime override at ~/.urnetwork/proxy_grades.json.
type proxyGradesConfig struct {
	Enabled       *bool `json:"enabled,omitempty"` // nil = true
	IntervalSec   int   `json:"interval_sec,omitempty"`
	Countdown     *bool `json:"countdown,omitempty"`      // nil = true
	GradesLog     *bool `json:"grades_log,omitempty"`     // nil = true
	RetentionDays int   `json:"retention_days,omitempty"` // 0 = 7
}

func defaultProxyGradesConfig() proxyGradesConfig {
	return proxyGradesConfig{IntervalSec: 300}
}

func (c proxyGradesConfig) enabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c proxyGradesConfig) interval() time.Duration {
	if c.IntervalSec <= 0 {
		return defaultGradeSummaryInterval
	}
	return time.Duration(c.IntervalSec) * time.Second
}

func (c proxyGradesConfig) countdownEnabled() bool {
	return c.Countdown == nil || *c.Countdown
}

func (c proxyGradesConfig) gradesLogEnabled() bool {
	return c.GradesLog == nil || *c.GradesLog
}

func (c proxyGradesConfig) retentionDays() int {
	if c.RetentionDays <= 0 {
		return defaultGradesRetentionDays
	}
	return c.RetentionDays
}

func proxyGradesOverridePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_grades.json"), nil
}

// readProxyGradesConfigFrom parses the override file and merges it onto the
// defaults, so a partial file ({}) leaves the defaults intact.
func readProxyGradesConfigFrom(path string) (proxyGradesConfig, error) {
	cfg := defaultProxyGradesConfig()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	var overrides proxyGradesConfig
	if err := json.Unmarshal(b, &overrides); err != nil {
		return cfg, fmt.Errorf("parse proxy_grades.json: %w", err)
	}
	// Merge: only fields present in the file override the defaults.
	cfg.Enabled = overrides.Enabled
	if overrides.IntervalSec != 0 {
		cfg.IntervalSec = overrides.IntervalSec
	}
	cfg.Countdown = overrides.Countdown
	cfg.GradesLog = overrides.GradesLog
	if overrides.RetentionDays != 0 {
		cfg.RetentionDays = overrides.RetentionDays
	}
	return cfg, nil
}

// mtime-invalidated cache, mirroring the proxy_probe.json pattern.
var (
	proxyGradesConfigMu    sync.Mutex
	proxyGradesConfigCache struct {
		path  string
		mtime time.Time
		cfg   proxyGradesConfig
		err   error
	}
)

func resetProxyGradesConfigCache() {
	proxyGradesConfigMu.Lock()
	defer proxyGradesConfigMu.Unlock()
	proxyGradesConfigCache = struct {
		path  string
		mtime time.Time
		cfg   proxyGradesConfig
		err   error
	}{}
}

func readProxyGradesConfig() proxyGradesConfig {
	path, err := proxyGradesOverridePath()
	if err != nil {
		return defaultProxyGradesConfig()
	}
	proxyGradesConfigMu.Lock()
	defer proxyGradesConfigMu.Unlock()
	if proxyGradesConfigCache.path == path {
		st, err := os.Stat(path)
		if err == nil && !st.ModTime().After(proxyGradesConfigCache.mtime) {
			return proxyGradesConfigCache.cfg
		}
	}
	cfg, err := readProxyGradesConfigFrom(path)
	proxyGradesConfigCache.path = path
	if st, serr := os.Stat(path); serr == nil {
		proxyGradesConfigCache.mtime = st.ModTime()
	}
	proxyGradesConfigCache.cfg = cfg
	proxyGradesConfigCache.err = err
	if err != nil {
		// A malformed proxy_grades.json must not fail silently: the parse
		// error is cached against the new mtime so nothing would ever
		// surface it again (MEDIUM-5). Log once per cache fill.
		importantLogf("[proxy][grade] warning: %v (using defaults)\n", err)
	}
	return cfg
}

// Next-probe timing, published by the fetch + reaper loops so the summary
// can report honest countdowns. urlFetcherState explains WHY no countdown
// exists when nextFetchProbeAt is zero: "" = state unknown/legacy,
// "none" = no URL sources configured (fetch loop never started),
// "pending" = sources exist but the first fetch has not happened yet.
var (
	nextFetchProbeAtMu   sync.Mutex
	nextFetchProbeAt     = time.Time{}
	urlFetcherState      string
	nextGradeRefreshAtMu sync.Mutex
	nextGradeRefreshAt   = time.Time{}
)

func setURLFetcherState(s string) {
	nextFetchProbeAtMu.Lock()
	defer nextFetchProbeAtMu.Unlock()
	urlFetcherState = s
}

func getURLFetcherState() string {
	nextFetchProbeAtMu.Lock()
	defer nextFetchProbeAtMu.Unlock()
	return urlFetcherState
}

func setNextFetchProbeAt(t time.Time) {
	nextFetchProbeAtMu.Lock()
	defer nextFetchProbeAtMu.Unlock()
	nextFetchProbeAt = t
}

func setNextGradeRefreshAt(t time.Time) {
	nextGradeRefreshAtMu.Lock()
	defer nextGradeRefreshAtMu.Unlock()
	nextGradeRefreshAt = t
}

// gradeSummary is one snapshot of the running proxy set.
type gradeSummary struct {
	running int // Health == "up"
	tracked int // total entries in proxy.state
	tiers   map[string]int
	sources map[string]map[string]int
	scores  []float64
	stale   int
}

func tierName(score float64) string {
	t := proxyGradeTier(score)
	if t == "" {
		return "F"
	}
	return t
}

// gradeSummaryStaleAfter returns the grade-freshness window for one proxy
// entry based on its source: URL-tagged entries ride the URL reaper's
// window (the URL reaper refreshes them), paid/file/internal entries ride
// the paid window (the paid grader refreshes them). The summary's stale
// ratio must agree with whoever owns the entry's refresh cadence — one
// shared number would mislabel URL entries as fresh long after their
// owner would re-probe them, or mislabel paid entries as stale early
// (independent review finding).
func gradeSummaryStaleAfter(src string, pressure float64) time.Duration {
	if src == "url" {
		return reaperStaleThreshold(pressure)
	}
	return paidStaleThreshold(pressure)
}

// collectProxyGradeSummary reads proxy.state + proxy_url.json under the
// proxy lock and buckets every RUNNING proxy by its grade. URL-sourced
// proxies take their grade from the URL cache; file/internal proxies from
// their ProxyEntry (paid/file grading sweep). Purely read-only.
//
// Returns (summary, false) when the snapshot could not be built (lock
// contention or unreadable state) — the caller must SKIP the round rather
// than log an all-zero "fleet collapsed" snapshot that would also poison
// the delta baseline (HIGH-2).
func collectProxyGradeSummary() (gradeSummary, bool) {
	s := gradeSummary{
		tiers:   map[string]int{},
		sources: map[string]map[string]int{},
	}
	// WithRetry (not the fail-fast acquireProxyLock) so a routine reload,
	// fetch, or reaper apply holding the lock does not make the summary
	// refuse a concurrent reload via mutual exclusion (MEDIUM-4).
	release, err := acquireProxyLockWithRetry()
	if err != nil {
		return s, false
	}
	defer release()

	state, err := readProxyState()
	if err != nil {
		return s, false
	}
	urlState, err := readProxyURLState()
	if err != nil {
		// URL cache unreadable is not fatal: file/internal proxies can
		// still be bucketed from proxy.state.
		urlState = &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	}
	// Freshness window PER SOURCE: URL-owned entries are refreshed by the
	// URL reaper on the URL stale window; paid/file-owned entries by the
	// paid grader on the wider paid window. The summary's stale ratio must
	// agree with whoever owns each entry's refresh cadence — one shared
	// number would mislabel URL entries as fresh long after their owner
	// would re-probe them, or mislabel paid entries as stale hours before
	// the paid grader would touch them (independent review finding).
	pressure := currentPressure()
	now := time.Now()

	// Effective ownership follows the SAME desired-set rule the paid
	// grader uses ("the desired set IS the ownership definition"): an
	// address in the paid file/internal set is served as a file proxy
	// (file wins in mergeProxyURLCache) even when its first-seen
	// provenance tag says "url", and it is graded by the paid grader
	// into its ProxyEntry. The summary must therefore bucket such an
	// address by the PAID owner — reading the URL cache grade and URL
	// window for it would report a grade the paid grader never produced
	// for that ownership (independent review finding). On a desired-set
	// read error the summary falls back to the state tags (read-only;
	// the worst case is a stale bucket, not a wrong write).
	// Ownership resolution MUST agree with the paid grader's UNION (file when
	// set + internal), not an either/or: a mixed file+internal deployment would
	// otherwise report internal-config addresses as ungraded with the wrong
	// staleness window (Opus review MEDIUM-2). Uses the same helper as the
	// grader so the reader cannot drift from the writer.
	desired := map[string]struct{}{}
	paidOwned, _ := paidDesiredSet(state)
	for addr := range paidOwned {
		desired[addr] = struct{}{}
	}

	for addr, entry := range state.Proxies {
		s.tracked++
		if entry.Health != "up" {
			continue
		}
		s.running++

		var score float64
		var graded bool
		var lastProbe time.Time
		src := entry.Source
		if _, ok := desired[addr]; ok {
			// Paid/file ownership overrides a stale URL provenance tag.
			src = "file"
		}
		if src == "url" {
			if ue, ok := urlState.Cache[addr]; ok {
				score, graded = ue.Score, ue.Graded
				lastProbe = ue.LastProbe
			}
		} else {
			score, graded = entry.Score, entry.Graded
			lastProbe = entry.LastGraded
		}

		// PENDING wins over a stale tier: a proxy that was previously graded
		// (Graded=true + old Score) but whose last pass was reachable-but-
		// undecidable must surface as "could not evaluate right now", not
		// hide behind its old letter tier. Otherwise a DNS-gutted re-probe of
		// a formerly-B proxy silently keeps it in the B bucket and the
		// operator never sees it went undecidable (review HIGH).
		if entry.Pending {
			s.tiers["pending"]++
			if s.sources[src] == nil {
				s.sources[src] = map[string]int{}
			}
			s.sources[src]["pending"]++
		} else if graded {
			t := tierName(score)
			s.tiers[t]++
			if s.sources[src] == nil {
				s.sources[src] = map[string]int{}
			}
			s.sources[src][t]++
			s.scores = append(s.scores, score)
			// Pick the window by the entry's source: URL entries ride the
			// URL reaper window, everything else the paid window.
			if !lastProbe.IsZero() && now.Sub(lastProbe) > gradeSummaryStaleAfter(src, pressure) {
				s.stale++
			}
		} else {
			s.tiers["ungraded"]++
			if s.sources[src] == nil {
				s.sources[src] = map[string]int{}
			}
			s.sources[src]["ungraded"]++
		}
	}
	sort.Float64s(s.scores)
	return s, true
}

func (s gradeSummary) tierLine() string {
	return fmt.Sprintf("running: A=%d B=%d C=%d D=%d F=%d pending=%d ungraded=%d (%d running, %d tracked)",
		s.tiers["A"], s.tiers["B"], s.tiers["C"], s.tiers["D"], s.tiers["F"],
		s.tiers["pending"], s.tiers["ungraded"], s.running, s.tracked)
}

func (s gradeSummary) sourcesLine() string {
	// Deterministic order: sort source names, then tiers A..F, ungraded.
	names := make([]string, 0, len(s.sources))
	for name := range s.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	tierOrder := []string{"A", "B", "C", "D", "F", "pending", "ungraded"}
	for _, name := range names {
		var b strings.Builder
		b.WriteString(name)
		b.WriteString(" A=")
		b.WriteString(fmt.Sprint(s.sources[name]["A"]))
		for _, t := range tierOrder[1:] {
			b.WriteString(" " + t + "=")
			b.WriteString(fmt.Sprint(s.sources[name][t]))
		}
		parts = append(parts, b.String())
	}
	if len(parts) == 0 {
		return "sources: (none)"
	}
	return "sources: " + strings.Join(parts, " | ")
}

func (s gradeSummary) scoresLine() string {
	if len(s.scores) == 0 {
		return "scores: (no graded proxies)"
	}
	median := s.scores[len(s.scores)/2]
	if len(s.scores)%2 == 0 {
		median = (s.scores[len(s.scores)/2-1] + s.scores[len(s.scores)/2]) / 2
	}
	// Nearest-rank p95. The unguarded index `int(float64(n)*0.95)` is
	// exactly `n` when 0.95n is an integer (n%20==0), i.e. the n=20 case
	// coderabbit flagged — clamp to the last element instead of indexing
	// out of range or selecting the max by accident.
	p95i := int(float64(len(s.scores)) * 0.95)
	if p95i >= len(s.scores) {
		p95i = len(s.scores) - 1
	}
	p95 := s.scores[p95i]
	// LOW-8: stale ratio is over graded proxies only (s.stale is only ever
	// incremented for graded entries), so the denominator must be
	// len(s.scores), not s.running (which includes ungraded proxies).
	return fmt.Sprintf("scores: median %.2f, p95 %.2f, min %.2f | stale grades: %d/%d",
		median, p95, s.scores[0], s.stale, len(s.scores))
}

// changesLine diffs the current snapshot against the previous one.
var (
	gradeSummaryPrevMu  sync.Mutex
	gradeSummaryPrev    gradeSummary
	gradeSummaryHasPrev bool
)

func (s gradeSummary) changesLine() string {
	gradeSummaryPrevMu.Lock()
	defer gradeSummaryPrevMu.Unlock()
	if !gradeSummaryHasPrev {
		gradeSummaryPrev = s
		gradeSummaryHasPrev = true
		return "changes vs last round: (first snapshot)"
	}
	var parts []string
	all := []string{"A", "B", "C", "D", "F", "pending", "ungraded"}
	for _, t := range all {
		d := s.tiers[t] - gradeSummaryPrev.tiers[t]
		if d > 0 {
			parts = append(parts, fmt.Sprintf("+%s %d", t, d))
		} else if d < 0 {
			parts = append(parts, fmt.Sprintf("-%s %d", t, -d))
		}
	}
	if n := s.running - gradeSummaryPrev.running; n != 0 {
		parts = append(parts, fmt.Sprintf("%drunning", n))
	}
	gradeSummaryPrev = s
	if len(parts) == 0 {
		return "changes vs last round: none"
	}
	return "changes vs last round: " + strings.Join(parts, ", ")
}

func countdownLine() string {
	var b strings.Builder
	b.WriteString("next fetch probe")
	nextFetchProbeAtMu.Lock()
	fetchAt := nextFetchProbeAt
	nextFetchProbeAtMu.Unlock()
	if !fetchAt.IsZero() {
		d := time.Until(fetchAt)
		if d < 0 {
			d = 0
		}
		b.WriteString(" in " + d.Round(time.Second).String())
	} else {
		// Zero countdown: say WHY honestly instead of the old opaque
		// "unknown (fetcher idle)" (LA1 defect 5 — confirmed expected
		// state, wording only).
		switch getURLFetcherState() {
		case "none":
			b.WriteString(" n/a (no URL sources configured)")
		case "pending":
			b.WriteString(" pending (waiting for first fetch: warmup + startup cooldown)")
		case "inactive":
			b.WriteString(" n/a (fetcher cancelled)")
		default:
			b.WriteString(" unknown (fetcher has not reported a schedule)")
		}
	}
	nextGradeRefreshAtMu.Lock()
	refreshAt := nextGradeRefreshAt
	nextGradeRefreshAtMu.Unlock()
	b.WriteString(", next grade refresh")
	if !refreshAt.IsZero() {
		d := time.Until(refreshAt)
		if d < 0 {
			d = 0
		}
		b.WriteString(" in " + d.Round(time.Second).String())
	} else {
		b.WriteString(" unknown")
	}
	return b.String()
}

// --- grades.log (per-proxy history, daily files, retention) -------------

// gradesLogMu serializes the open/write/prune sequence. gradesLogWrite is
// reached from three goroutines (summary runner, paid grader, URL reaper);
// O_APPEND makes the offset update atomic, but a concurrent prune's
// os.Remove could race an append to the same day file and silently lose
// the line to an unlinked inode (LOW-12).
var gradesLogMu sync.Mutex

// lastGradesLogPruneDay tracks the UTC day of the last prune so the
// O(ReadDir) retention scan runs once per day-rollover, not once per
// written line (MEDIUM-3 — a per-line scan is a full directory read under
// the caller's lock on busy boxes).
var lastGradesLogPruneDay string

func gradesLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "grades"), nil
}

func gradesLogWrite(line string) {
	gradesLogMu.Lock()
	defer gradesLogMu.Unlock()

	dir, err := gradesLogDir()
	if err != nil {
		return
	}
	// Private directory + files: they contain per-proxy endpoints and
	// grades — operational intelligence not for other local users
	// (coderabbit security major).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		tlog("[proxy][grade] warning: grades dir %s: %v\n", dir, err)
		return
	}
	path := filepath.Join(dir, time.Now().UTC().Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		tlog("[proxy][grade] warning: grades log %s: %v\n", path, err)
		return
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		tlog("[proxy][grade] warning: grades log write: %v\n", err)
		f.Close()
		return
	}
	// Sync before close so the audit trail survives a crash (M3).
	if err := f.Sync(); err != nil {
		tlog("[proxy][grade] warning: grades log sync: %v\n", err)
	}
	f.Close()
	// Prune at most once per UTC day: the retention scan is O(entries) and
	// must not run per line (MEDIUM-3).
	day := time.Now().UTC().Format("2006-01-02")
	if day != lastGradesLogPruneDay {
		lastGradesLogPruneDay = day
		pruneGradesLog(dir, readProxyGradesConfig().retentionDays())
	}
}

// pruneGradesLog removes daily grade files older than retentionDays.
func pruneGradesLog(dir string, retentionDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Filenames are YYYY-MM-DD.log
		stamp := strings.TrimSuffix(e.Name(), ".log")
		t, err := time.Parse("2006-01-02", stamp)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// emitProxyGradeDelta logs a per-address tier change (important + disk +
// grades.log), mirroring the paid-grader convention for URL proxies.
func emitProxyGradeDelta(addr, oldTier, newTier string, oldScore, newScore float64, oldGraded bool) {
	if !oldGraded || oldTier == newTier {
		return
	}
	line := fmt.Sprintf("[proxy][grade] delta %s %s->%s (%.2f->%.2f)", addr, oldTier, newTier, oldScore, newScore)
	importantLogf("%s\n", line)
	if cfg := readProxyGradesConfig(); cfg.gradesLogEnabled() {
		gradesLogWrite(time.Now().UTC().Format(time.RFC3339) + " " + line)
	}
}

// --- the summary runner --------------------------------------------------

func runProxyGradeSummary(ctx context.Context) {
	// Read the interval from config so proxy_grades.json interval_sec is
	// honored; re-read + Reset each tick so a live edit takes effect on
	// the next round (coderabbit major: the ticker was hardcoded to the
	// default).
	cur := summaryIntervalFromConfig()
	ticker := time.NewTicker(cur)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if iv := summaryIntervalFromConfig(); iv != cur {
				cur = iv
				ticker.Reset(iv)
			}
		}
		runProxyGradeSummaryOnce()
	}
}

// summaryIntervalFromConfig returns the configured grade-summary interval,
// falling back to the default when unset or non-positive.
func summaryIntervalFromConfig() time.Duration {
	iv := readProxyGradesConfig().interval()
	if iv <= 0 {
		iv = defaultGradeSummaryInterval
	}
	return iv
}

// collectGradeSummaryFn is the snapshot collector used by the summary
// runner. It is a package-level var (not a direct call) so tests can
// inject a fake that returns ok=false with tracked>0 — the scenario that
// proves the HIGH-2 guard is load-bearing rather than masked by the
// tracked==0 skip (Sonnet round-2 Finding A).
var collectGradeSummaryFn = collectProxyGradeSummary

// runProxyGradeSummaryOnce computes and logs one snapshot. Split out for
// direct testing.
func runProxyGradeSummaryOnce() {
	cfg := readProxyGradesConfig()
	if !cfg.enabled() {
		return
	}
	// Kill switch (proxy_probe.json enabled=false) also silences the
	// summary: with stage-1 grading off, a running summary would report a
	// fleet that is now entirely "ungraded" (MEDIUM-6).
	if !resolveProxyTableProbeConfig().Enabled {
		return
	}
	s, ok := collectGradeSummaryFn()
	if !ok {
		// Lock contention or unreadable state: skip the round entirely.
		// Logging an all-zero snapshot would read as a fleet collapse and
		// install it as the delta baseline (HIGH-2).
		return
	}
	if s.tracked == 0 {
		// No proxies configured — skip rather than write 4 lines every 5
		// minutes of "(0 running, 0 tracked)" into important/disk/grades
		// logs (LOW-11).
		return
	}
	lines := []string{
		"[proxy][grade] " + s.tierLine(),
		"[proxy][grade] " + s.sourcesLine(),
		"[proxy][grade] " + s.changesLine(),
		"[proxy][grade] " + s.scoresLine(),
	}
	for _, l := range lines {
		importantLogf("%s\n", l)
		if cfg.gradesLogEnabled() {
			gradesLogWrite(time.Now().UTC().Format(time.RFC3339) + " " + l)
		}
	}
	if cfg.countdownEnabled() {
		// Regular ramlog only: high-frequency, ephemeral — must not crowd
		// the important/disk logs.
		tlog("[proxy][grade] %s\n", countdownLine())
	}
}
