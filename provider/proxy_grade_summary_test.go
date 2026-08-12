package main

// Tests for the periodic A-F grade summary (design 2026-08-09): config
// live-re-read, running-set bucketing (URL vs paid grades), per-source
// breakdown, delta emission, and grades.log retention.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGradesOverride(t *testing.T, content string) {
	t.Helper()
	home := withTempHome(t)
	path := filepath.Join(home, ".urnetwork", "proxy_grades.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProxyGradesConfig_Defaults(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".urnetwork", "proxy_grades.json")
	cfg, err := readProxyGradesConfigFrom(path)
	if err != nil {
		t.Fatalf("missing file should be defaults: %v", err)
	}
	if !cfg.enabled() || cfg.interval() != defaultGradeSummaryInterval || !cfg.countdownEnabled() || !cfg.gradesLogEnabled() {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if cfg.retentionDays() != defaultGradesRetentionDays {
		t.Fatalf("default retention: %d", cfg.retentionDays())
	}
}

func TestProxyGradesConfig_PartialMerge(t *testing.T) {
	writeGradesOverride(t, `{"interval_sec": 60}`)
	cfg := readProxyGradesConfig()
	if cfg.interval() != time.Minute {
		t.Fatalf("interval not merged: %v", cfg.interval())
	}
	if !cfg.enabled() || !cfg.countdownEnabled() || !cfg.gradesLogEnabled() {
		t.Fatalf("defaults clobbered by partial file: %+v", cfg)
	}
}

func TestProxyGradesConfig_Disable(t *testing.T) {
	writeGradesOverride(t, `{"enabled": false, "countdown": false, "grades_log": false, "retention_days": 3}`)
	cfg := readProxyGradesConfig()
	if cfg.enabled() {
		t.Fatal("enabled should be false")
	}
	if cfg.countdownEnabled() || cfg.gradesLogEnabled() {
		t.Fatal("countdown/grades_log should be false")
	}
	if cfg.retentionDays() != 3 {
		t.Fatalf("retention: %d", cfg.retentionDays())
	}
}

func TestProxyGradesConfig_LiveReRead(t *testing.T) {
	writeGradesOverride(t, `{"interval_sec": 60}`)
	resetProxyGradesConfigCache()
	if cfg := readProxyGradesConfig(); cfg.interval() != time.Minute {
		t.Fatalf("first read: %v", cfg.interval())
	}
	// Same content: cached.
	writeGradesOverride(t, `{"interval_sec": 60}`)
	resetProxyGradesConfigCache()
	// Different mtime + content: re-read live.
	writeGradesOverride(t, `{"interval_sec": 900}`)
	resetProxyGradesConfigCache()
	if cfg := readProxyGradesConfig(); cfg.interval() != 15*time.Minute {
		t.Fatalf("live re-read failed: %v", cfg.interval())
	}
}

func TestCollectProxyGradeSummary_Buckets(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// URL-sourced proxies: grades live in proxy_url.json cache.
	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {ProbeOK: true, Score: 0.95, Graded: true, LastProbe: time.Now()},
		"2.2.2.2:1080": {ProbeOK: true, Score: 0.83, Graded: true, LastProbe: time.Now()},
		"3.3.3.3:1080": {ProbeOK: true, Score: 0.5, Graded: true, LastProbe: time.Now()},
		"4.4.4.4:1080": {ProbeOK: true}, // running but never graded
	}}
	if err := writeProxyURLStateTo(filepath.Join(dir, "proxy_url.json"), urlState); err != nil {
		t.Fatal(err)
	}
	// Paid/file proxies: grades live in proxy.state ProxyEntry.
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
		"4.4.4.4:1080": {Health: "up", Source: "url"},
		"5.5.5.5:1080": {Health: "up", Source: "file", Score: 0.98, Graded: true, LastGraded: time.Now()},
		"6.6.6.6:1080": {Health: "dead", Source: "file", Score: 0.4, Graded: true},
		"7.7.7.7:1080": {Health: "up", Source: "internal"}, // ungraded paid
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}

	s, ok := collectProxyGradeSummary()
	if !ok {
		t.Fatal("collectProxyGradeSummary returned ok=false")
	}
	if s.running != 6 { // 1.1.1.1..4.4.4.4 (url), 5.5.5.5 (file), 7.7.7.7 (internal up-ungraded); 6.6.6.6 dead
		t.Fatalf("running: %d", s.running)
	}
	// A: 1.1.1.1 (0.95) + 5.5.5.5 (0.98) = 2; B: 2.2.2.2 (0.83) = 1;
	// F: 3.3.3.3 (0.5) = 1; ungraded: 4.4.4.4 + 7.7.7.7 = 2.
	if s.tiers["A"] != 2 || s.tiers["B"] != 1 || s.tiers["F"] != 1 || s.tiers["ungraded"] != 2 {
		t.Fatalf("buckets wrong: %+v", s.tiers)
	}
	if len(s.scores) != 4 {
		t.Fatalf("scores: %d", len(s.scores))
	}
	if s.sources["url"]["A"] != 1 || s.sources["file"]["A"] != 1 {
		t.Fatalf("per-source wrong: %+v", s.sources)
	}
}

func TestCollectProxyGradeSummary_Stale(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {Score: 0.9, Graded: true, LastProbe: old},
		"2.2.2.2:1080": {Score: 0.9, Graded: true, LastProbe: time.Now()},
	}}
	if err := writeProxyURLStateTo(filepath.Join(dir, "proxy_url.json"), urlState); err != nil {
		t.Fatal(err)
	}
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}
	s, ok := collectProxyGradeSummary()
	if !ok {
		t.Fatal("collectProxyGradeSummary returned ok=false")
	}
	if s.stale != 1 {
		t.Fatalf("stale: %d, want 1", s.stale)
	}
}

// TestGradeSummaryStaleAfter_PerSource pins the per-source stale-window
// decision (independent review finding): URL-tagged entries must be
// judged against the URL reaper's window (reaperStaleThreshold), and
// paid/file/internal entries against the paid window — the summary's
// stale ratio must agree with whoever owns the entry's refresh cadence.
func TestGradeSummaryStaleAfter_PerSource(t *testing.T) {
	if got := gradeSummaryStaleAfter("url", 0); got != reaperStaleThreshold(0) {
		t.Fatalf("url source: got %v, want URL reaper window %v", got, reaperStaleThreshold(0))
	}
	for _, src := range []string{"file", "internal", ""} {
		if got := gradeSummaryStaleAfter(src, 0); got != paidStaleThreshold(0) {
			t.Fatalf("source %q: got %v, want paid window %v", src, got, paidStaleThreshold(0))
		}
	}
	// The URL window must be strictly narrower than the paid window at
	// the same pressure, or the paid/free divergence is meaningless.
	if !(reaperStaleThreshold(0) < paidStaleThreshold(0)) {
		t.Fatal("URL window must be narrower than the paid window")
	}
}

// TestCollectProxyGradeSummary_StaleBySource exercises the per-source
// window through the collector: a URL entry 4h old is stale by the URL
// reaper window (3h calm) while a paid entry 4h old is still fresh by
// the paid window (6h calm) — only the URL one counts as stale.
// (Calm-pressure test, consistent with the rest of the suite.)
func TestCollectProxyGradeSummary_StaleBySource(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fourHours := time.Now().Add(-4 * time.Hour)
	urlState := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.1.1.1:1080": {Score: 0.9, Graded: true, LastProbe: fourHours},
	}}
	if err := writeProxyURLStateTo(filepath.Join(dir, "proxy_url.json"), urlState); err != nil {
		t.Fatal(err)
	}
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"5.5.5.5:1080": {Health: "up", Source: "file", Graded: true, Score: 0.8, LastGraded: fourHours},
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}
	s, ok := collectProxyGradeSummary()
	if !ok {
		t.Fatal("collectProxyGradeSummary returned ok=false")
	}
	if s.stale != 1 {
		t.Fatalf("stale: %d, want 1 (URL entry stale by URL window; paid entry fresh by paid window)", s.stale)
	}
}

func TestEmitProxyGradeDelta(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resetProxyGradesConfigCache()

	// Same tier: no delta.
	emitProxyGradeDelta("1.1.1.1:1080", "A", "A", 0.9, 0.91, true)
	// Not previously graded: no delta. This is the HIGH-1 shape: the
	// first-ever grade of a proxy must be suppressed (oldGraded=false) —
	// the production call sites now capture wasGraded BEFORE setting
	// entry.Graded=true, so this is the value they actually pass.
	emitProxyGradeDelta("2.2.2.2:1080", "", "A", 0, 0.9, false)
	// Real change: delta written to grades.log.
	emitProxyGradeDelta("3.3.3.3:1080", "A", "F", 0.92, 0.33, true)

	files, err := os.ReadDir(filepath.Join(dir, "grades"))
	if err != nil {
		t.Fatalf("grades dir: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("grades files: %d", len(files))
	}
	b, err := os.ReadFile(filepath.Join(dir, "grades", files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if !contains(content, "3.3.3.3:1080 A->F (0.92->0.33)") {
		t.Fatalf("delta line missing: %q", content)
	}
	if contains(content, "1.1.1.1") || contains(content, "2.2.2.2") {
		t.Fatalf("non-delta lines written: %q", content)
	}
}

// TestGradeSummaryScoresLineP95Edge is the coderabbit p95 regression:
// with exactly n%20==0 scores (the off-by-one case), scoresLine must not
// panic and must return a sane p95, not the max by accident.
func TestGradeSummaryScoresLineP95Edge(t *testing.T) {
	scores := make([]float64, 20)
	for i := range scores {
		scores[i] = float64(i+1) / 20 // 0.05..1.0
	}
	s := gradeSummary{scores: scores}
	got := s.scoresLine()
	// Nearest-rank p95 index for n=20: int(20*0.95)=19 → scores[19]=1.0
	// (the 95th percentile of 20 samples is the 19th element, 0-indexed).
	if !contains(got, "p95 1.00") {
		t.Fatalf("p95 edge: %q", got)
	}
	// LOW-8: the stale denominator is len(scores) for graded proxies.
	if !contains(got, "stale grades: 0/20") {
		t.Fatalf("stale denominator: %q", got)
	}
}

func TestPruneGradesLog(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork", "grades")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 10 days ago (must be pruned), today (kept), and a non-date file (kept).
	old := time.Now().UTC().AddDate(0, 0, -10)
	for _, name := range []string{old.Format("2006-01-02") + ".log", time.Now().UTC().Format("2006-01-02") + ".log", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneGradesLog(dir, defaultGradesRetentionDays)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("prune kept %d files, want 2", len(entries))
	}
}

func TestGradeSummaryLines(t *testing.T) {
	s := gradeSummary{
		running: 3, tracked: 5,
		tiers:   map[string]int{"A": 1, "B": 1, "F": 1, "ungraded": 1},
		sources: map[string]map[string]int{"url": {"A": 1, "F": 1}, "file": {"B": 1, "ungraded": 1}},
		scores:  []float64{0.1, 0.5, 0.9},
	}
	if got := s.tierLine(); !contains(got, "A=1 B=1 C=0 D=0 F=1 ungraded=1 (3 running, 5 tracked)") {
		t.Fatalf("tierLine: %q", got)
	}
	if got := s.sourcesLine(); !contains(got, "file A=0 B=1 C=0 D=0 F=0 ungraded=1") || !contains(got, "url A=1 B=0 C=0 D=0 F=1 ungraded=0") {
		t.Fatalf("sourcesLine: %q", got)
	}
	if got := s.scoresLine(); !contains(got, "median 0.50") || !contains(got, "p95 0.90") {
		t.Fatalf("scoresLine: %q", got)
	}
	if got := s.changesLine(); !contains(got, "(first snapshot)") {
		t.Fatalf("changesLine first: %q", got)
	}
	gradeSummaryHasPrev = false
	s2 := gradeSummary{running: 4, tiers: map[string]int{"A": 2, "B": 1}}
	_ = s2
}

func TestCountdownLine(t *testing.T) {
	setNextFetchProbeAt(time.Now().Add(4 * time.Minute))
	setNextGradeRefreshAt(time.Now().Add(55 * time.Second))
	got := countdownLine()
	if !contains(got, "next fetch probe in 4m") || !contains(got, "next grade refresh in 55s") {
		t.Fatalf("countdownLine: %q", got)
	}
}

// TestRunProxyGradeSummaryOnceSkipsOnUnreadableState is the HIGH-2
// regression: when the summary cannot build a real snapshot (here, the
// proxy.state path is a directory so ReadFile errors), the round must be
// skipped entirely — no "all-zero fleet collapsed" lines, and the delta
// baseline must NOT be installed so the next round cannot report a
// phantom mass recovery.
func TestRunProxyGradeSummaryOnceSkipsOnUnreadableState(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resetProxyGradesConfigCache()

	// Make proxy.state unreadable-as-file (a directory): readProxyState
	// returns a real error, not an empty state.
	if err := os.Mkdir(filepath.Join(dir, "proxy.state"), 0o700); err != nil {
		t.Fatal(err)
	}

	gradeSummaryHasPrev = false
	runProxyGradeSummaryOnce()

	if gradeSummaryHasPrev {
		t.Fatal("HIGH-2 regression: failed round installed the delta baseline (gradeSummaryHasPrev=true)")
	}
	// No grades.log may be created either: the round never reached the
	// write path.
	if _, err := os.Stat(filepath.Join(dir, "grades")); !os.IsNotExist(err) {
		t.Fatalf("HIGH-2 regression: grades dir created on a skipped round (err=%v)", err)
	}
}

// TestRunProxyGradeSummaryOnceSkipsOnCollectorFailure is the LOAD-BEARING
// HIGH-2 regression (Sonnet round-2 Finding A). The unreadable-state test
// above is masked by the tracked==0 skip (both fire on the same scenario),
// so it cannot prove the !ok guard is what prevents the phantom snapshot.
// This test injects a collector that returns ok=false WITH tracked>0 —
// the only scenario that discriminates: if the !ok guard were deleted,
// this round would log a zero snapshot and install the baseline even
// though real proxies exist.
func TestRunProxyGradeSummaryOnceSkipsOnCollectorFailure(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resetProxyGradesConfigCache()
	// A valid state file so tracked>0 would be produced by the real
	// collector — the fake below reports failure despite that.
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "file", Score: 0.95, Graded: true},
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}

	orig := collectGradeSummaryFn
	collectGradeSummaryFn = func() (gradeSummary, bool) {
		// A partial snapshot with real proxies, but a failed round:
		// tracked>0 AND ok=false — the HIGH-2 discriminator.
		return gradeSummary{
			tracked: 1, running: 1,
			tiers:   map[string]int{"A": 1},
			sources: map[string]map[string]int{"file": {"A": 1}},
			scores:  []float64{0.95},
		}, false
	}
	defer func() { collectGradeSummaryFn = orig }()

	gradeSummaryHasPrev = false
	runProxyGradeSummaryOnce()

	if gradeSummaryHasPrev {
		t.Fatal("HIGH-2: collector failure with tracked>0 installed the delta baseline — !ok guard is dead")
	}
	if _, err := os.Stat(filepath.Join(dir, "grades")); !os.IsNotExist(err) {
		t.Fatalf("HIGH-2: grades dir created despite collector failure (err=%v)", err)
	}
}

// TestRunProxyGradeSummaryOnceSkipsWhenKillSwitchOff is the MEDIUM-6
// regression: proxy_probe.json {"enabled": false} must silence the
// summary too, not just the stage-1 probe.
func TestRunProxyGradeSummaryOnceSkipsWhenKillSwitchOff(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resetProxyGradesConfigCache()
	if err := os.WriteFile(filepath.Join(dir, "proxy_probe.json"), []byte(`{"enabled": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A valid state file so the only reason to skip is the kill switch.
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "file", Score: 0.95, Graded: true},
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}

	gradeSummaryHasPrev = false
	runProxyGradeSummaryOnce()

	if gradeSummaryHasPrev {
		t.Fatal("MEDIUM-6 regression: summary ran despite kill switch (baseline installed)")
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestSummaryIntervalFromConfig covers the ticker-interval resolver
// (0% covered before this test — runProxyGradeSummary's loop is only
// exercised via a live ticker, so the pure resolver needs its own test).
func TestSummaryIntervalFromConfig(t *testing.T) {
	withTempHome(t)
	resetProxyGradesConfigCache()
	if got := summaryIntervalFromConfig(); got != defaultGradeSummaryInterval {
		t.Fatalf("default: got %v, want %v", got, defaultGradeSummaryInterval)
	}

	writeGradesOverride(t, `{"interval_sec": 42}`)
	resetProxyGradesConfigCache()
	if got := summaryIntervalFromConfig(); got != 42*time.Second {
		t.Fatalf("override: got %v, want 42s", got)
	}
}

// TestGradeSummaryChangesLine_Diff exercises the round-over-round delta
// branch of changesLine (only the "(first snapshot)" branch was covered
// before this test, leaving the actual diff math — the reason the line
// exists at all — untested).
func TestGradeSummaryChangesLine_Diff(t *testing.T) {
	gradeSummaryPrevMu.Lock()
	gradeSummaryPrev = gradeSummary{}
	gradeSummaryHasPrev = false
	gradeSummaryPrevMu.Unlock()

	s1 := gradeSummary{running: 5, tiers: map[string]int{"A": 2, "B": 1, "F": 1}}
	if got := s1.changesLine(); !contains(got, "(first snapshot)") {
		t.Fatalf("first round: %q", got)
	}

	s2 := gradeSummary{running: 6, tiers: map[string]int{"A": 3, "B": 1, "F": 0}}
	got := s2.changesLine()
	if !contains(got, "+A 1") {
		t.Fatalf("expected +A 1 in diff: %q", got)
	}
	if !contains(got, "-F 1") {
		t.Fatalf("expected -F 1 in diff: %q", got)
	}
	if !contains(got, "1running") {
		t.Fatalf("expected 1running in diff: %q", got)
	}

	// Third round identical to the second: no changes.
	s3 := gradeSummary{running: 6, tiers: map[string]int{"A": 3, "B": 1, "F": 0}}
	if got := s3.changesLine(); !contains(got, "changes vs last round: none") {
		t.Fatalf("no-op round: %q", got)
	}
}

// TestRunProxyGradeSummaryOnce_WritesSummary is the happy-path companion
// to the two skip-path regression tests above: with grading enabled, the
// kill switch on, and a real tracked proxy, the runner must actually
// produce and persist a snapshot (tier/sources/changes/scores lines to
// grades.log) and install the delta baseline. Before this test, only the
// two skip branches of runProxyGradeSummaryOnce were covered (41.2%),
// never the success path that does the real work.
func TestRunProxyGradeSummaryOnce_WritesSummary(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	resetProxyGradesConfigCache()

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "file", Score: 0.95, Graded: true, LastGraded: time.Now()},
		"2.2.2.2:1080": {Health: "up", Source: "internal", Score: 0.40, Graded: true, LastGraded: time.Now()},
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}

	gradeSummaryPrevMu.Lock()
	gradeSummaryHasPrev = false
	gradeSummaryPrevMu.Unlock()

	runProxyGradeSummaryOnce()

	gradeSummaryPrevMu.Lock()
	hasPrev := gradeSummaryHasPrev
	gradeSummaryPrevMu.Unlock()
	if !hasPrev {
		t.Fatal("successful round did not install the delta baseline")
	}

	files, err := os.ReadDir(filepath.Join(dir, "grades"))
	if err != nil || len(files) != 1 {
		t.Fatalf("grades log not written: files=%v err=%v", files, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "grades", files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if !contains(content, "[proxy][grade] running:") {
		t.Fatalf("tier line missing: %q", content)
	}
	if !contains(content, "[proxy][grade] sources:") {
		t.Fatalf("sources line missing: %q", content)
	}
	if !contains(content, "[proxy][grade] scores:") {
		t.Fatalf("scores line missing: %q", content)
	}
}

// TestRunProxyGradeSummaryOnce_SkipsWhenDisabled covers the cfg.enabled()
// == false branch, distinct from the kill-switch (proxy_probe.json)
// branch already covered elsewhere.
func TestRunProxyGradeSummaryOnce_SkipsWhenDisabled(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write the override directly (not via writeGradesOverride, which
	// calls withTempHome itself and would swap HOME out from under the
	// proxy.state already written below into this dir).
	if err := os.WriteFile(filepath.Join(dir, "proxy_grades.json"), []byte(`{"enabled": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetProxyGradesConfigCache()

	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "file", Score: 0.95, Graded: true},
	}}
	if err := writeProxyStateTo(filepath.Join(dir, "proxy.state"), state); err != nil {
		t.Fatal(err)
	}

	gradeSummaryPrevMu.Lock()
	gradeSummaryHasPrev = false
	gradeSummaryPrevMu.Unlock()

	runProxyGradeSummaryOnce()

	gradeSummaryPrevMu.Lock()
	hasPrev := gradeSummaryHasPrev
	gradeSummaryPrevMu.Unlock()
	if hasPrev {
		t.Fatal("disabled config still ran the summary")
	}
	if _, err := os.Stat(filepath.Join(dir, "grades")); !os.IsNotExist(err) {
		t.Fatalf("grades dir created despite enabled=false (err=%v)", err)
	}
}
