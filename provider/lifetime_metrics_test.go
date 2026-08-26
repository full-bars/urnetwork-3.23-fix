package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
