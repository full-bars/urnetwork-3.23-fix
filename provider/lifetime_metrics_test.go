package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLifetimeMetrics_AddAndSnapshot(t *testing.T) {
	lm := &lifetimeMetrics{}
	lm.Add(3, 1, 5, 2, 7, 9, 1024)
	pqe, clas, up, deny, recov, lost, bytes := lm.Snapshot()
	if pqe != 3 || clas != 1 || up != 5 || deny != 2 || recov != 7 || lost != 9 || bytes != 1024 {
		t.Fatalf("unexpected snapshot: %d %d %d %d %d %d %d", pqe, clas, up, deny, recov, lost, bytes)
	}
}

// Zero deltas must never move totals (the store itself takes uint64s; the
// reset-guard contract lives in the callers' uDelta, tested implicitly by
// the collector wiring — here we pin that harmless adds are harmless).
func TestLifetimeMetrics_NegativeDeltasIgnored(t *testing.T) {
	lm := &lifetimeMetrics{}
	lm.Add(10, 10, 10, 10, 10, 10, 1000)
	lm.Add(0, 0, 0, 0, 0, 0, 0)
	pqe, _, _, _, _, _, bytes := lm.Snapshot()
	if pqe != 10 || bytes != 1000 {
		t.Fatalf("zero-delta add mutated totals: pqe=%d bytes=%d", pqe, bytes)
	}
}

func TestLifetimeMetrics_PersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".urnetwork", "lifetime_metrics.json")

	lm := loadLifetimeMetrics(path)
	lm.Add(11, 3, 4, 1, 6, 2, 123456)
	lm.Flush()

	reloaded := loadLifetimeMetrics(path)
	pqe, clas, up, deny, recov, lost, bytes := reloaded.Snapshot()
	if pqe != 11 || clas != 3 || up != 4 || deny != 1 || recov != 6 || lost != 2 || bytes != 123456 {
		t.Fatalf("round-trip mismatch: %d %d %d %d %d %d %d", pqe, clas, up, deny, recov, lost, bytes)
	}

	// File hygiene: valid JSON with the expected keys, 0600 perms.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		t.Fatal("state file is not valid JSON")
	}
	for _, key := range []string{"pqe_opens", "billable_bytes"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing key %q in state file", key)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file perms = %v, want 0600", perm)
	}
}

func TestLifetimeMetrics_MissingFileStartsEmpty(t *testing.T) {
	lm := loadLifetimeMetrics(filepath.Join(t.TempDir(), ".urnetwork", "nope.json"))
	pqe, clas, up, deny, recov, lost, bytes := lm.Snapshot()
	if pqe|clas|up|deny|recov|lost|bytes != 0 {
		t.Fatal("missing file must yield an empty store")
	}
}

func TestLifetimeMetrics_CorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".urnetwork", "lifetime_metrics.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("{not json"), 0o600)

	lm := loadLifetimeMetrics(path)
	pqe, _, _, _, _, _, _ := lm.Snapshot()
	if pqe != 0 {
		t.Fatal("corrupt file must reset (not block) the store")
	}
	// And the store must remain usable + flushable afterwards.
	lm.Add(1, 0, 0, 0, 0, 0, 5)
	lm.Flush()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("flush after corrupt-load failed: %v", err)
	}
}

func TestLifetimeMetrics_ThrottledFlush(t *testing.T) {
	dir := t.TempDir()
	lm := loadLifetimeMetrics(filepath.Join(dir, "lm.json"))
	lm.flushEvery = time.Hour // effectively never within this test

	lm.Add(1, 0, 0, 0, 0, 0, 0)
	now := time.Now()
	if lm.MaybeFlush(now) {
		t.Fatal("first flush inside throttle window should not write... ")
	}
	if !lm.MaybeFlush(now.Add(2 * time.Hour)) {
		t.Fatal("flush after throttle window elapsed should write")
	}
	if lm.MaybeFlush(now.Add(2*time.Hour + time.Minute)) {
		t.Fatal("clean store must not rewrite")
	}
}

// "Since" is what makes "all-time" datable: it must survive restarts
// unchanged, so the label keeps describing the same period after a restart.
func TestLifetimeMetrics_SinceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lm.json")

	lm := loadLifetimeMetrics(path)
	started, approx := lm.Started()
	if started.IsZero() || approx {
		t.Fatalf("fresh store: since=%v approx=%v, want a real instant with no caveat", started, approx)
	}
	lm.Add(1, 0, 0, 0, 0, 0, 0)
	lm.Flush()

	reloaded := loadLifetimeMetrics(path)
	again, approxAgain := reloaded.Started()
	if approxAgain {
		t.Fatal("restarted store lost its exact start and became approximate")
	}
	if !again.Equal(started.UTC().Truncate(time.Second)) {
		t.Fatalf("since moved across restart: %v -> %v", started, again)
	}
}

// A store written before the field existed keeps its totals but cannot know
// when they began. It must say so rather than claim the date it was upgraded.
func TestLifetimeMetrics_LegacyStoreGetsApproximateStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.json")
	legacy := `{"pqe_opens":42,"billable_bytes":1024,"saved_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	lm := loadLifetimeMetrics(path)
	pqe, _, _, _, _, _, bytes := lm.Snapshot()
	if pqe != 42 || bytes != 1024 {
		t.Fatalf("legacy totals dropped: pqe=%d bytes=%d", pqe, bytes)
	}
	started, approx := lm.Started()
	if !approx {
		t.Fatal("legacy store must report an approximate start, not a date it cannot know")
	}
	if started.IsZero() || time.Since(started) > time.Minute {
		t.Fatalf("approximate start should be pinned near load time, got %v", started)
	}
}

func TestSinceLabel(t *testing.T) {
	instant := time.Date(2026, 10, 5, 8, 12, 0, 0, time.Local)
	if got, want := sinceLabel(instant, false), "since 2026-10-05 08:12 "+instant.Format("MST"); got != want {
		t.Fatalf("exact label = %q, want %q", got, want)
	}
	approxLabel := sinceLabel(instant, true)
	if !strings.Contains(approxLabel, "start approximate") {
		t.Fatalf("approximate label must say the start is approximate, got %q", approxLabel)
	}
	if !strings.Contains(approxLabel, "totals include earlier history") {
		t.Fatalf("approximate label must say the totals still cover earlier history, got %q", approxLabel)
	}
	if got := sinceLabel(time.Time{}, false); got != "since unknown" {
		t.Fatalf("zero start label = %q", got)
	}
	if got, want := clockLabel(instant), "2026-10-05 08:12 "+instant.Format("MST"); got != want {
		t.Fatalf("clockLabel = %q, want %q", got, want)
	}
	if got := clockLabel(time.Time{}); got != "unknown" {
		t.Fatalf("zero clockLabel = %q, want unknown", got)
	}
}

// The pinned start must reach disk even when the node never moves a counter,
// otherwise every idle restart re-pins a later date while the totals stay put.
func TestLifetimeMetrics_LegacyStartPersistsWithoutTraffic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.json")
	legacy := `{"pqe_opens":7,"saved_at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	lm := loadLifetimeMetrics(path)
	pinned, _ := lm.Started()
	lm.Flush() // no Add at all: an idle node between restarts

	reloaded := loadLifetimeMetrics(path)
	again, approx := reloaded.Started()
	if !approx {
		t.Fatal("reloaded legacy store lost its approximate flag")
	}
	if !again.Equal(pinned.UTC().Truncate(time.Second)) {
		t.Fatalf("pinned start rolled forward across restart: %v -> %v", pinned, again)
	}
}

// A zero time marshals as year 1. If one ever reaches the file, loading it
// must not turn it into an exact start date.
func TestLifetimeMetrics_YearOneSinceStaysApproximate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.json")
	zeroed := `{"pqe_opens":1,"since":"0001-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(zeroed), 0o600); err != nil {
		t.Fatal(err)
	}

	lm := loadLifetimeMetrics(path)
	started, approx := lm.Started()
	if !approx {
		t.Fatal("a year-one timestamp must be treated as an unknown start")
	}
	if started.Year() == 1 {
		t.Fatal("year-one start leaked through as if it were a real date")
	}
}
