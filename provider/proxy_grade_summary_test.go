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

	s := collectProxyGradeSummary()
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
	s := collectProxyGradeSummary()
	if s.stale != 1 {
		t.Fatalf("stale: %d, want 1", s.stale)
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
	// Not previously graded: no delta.
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

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
