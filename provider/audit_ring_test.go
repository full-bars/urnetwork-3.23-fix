package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAuditRing_AppendAndEntries(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	// Append 5 entries
	for i := 0; i < 5; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, 0, i, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			Value:     "nyc-1",
			OK:        true,
		})
	}

	// Get all 5, newest first
	entries, nextCursor := ring.Entries(10, "")
	if len(entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(entries))
	}
	// Newest first: 10:00:04, 10:00:03, 10:00:02, 10:00:01, 10:00:00
	if entries[0].Timestamp.Second() != 4 {
		t.Errorf("first entry should be newest (sec=4), got %d", entries[0].Timestamp.Second())
	}
	if entries[4].Timestamp.Second() != 0 {
		t.Errorf("last entry should be oldest (sec=0), got %d", entries[4].Timestamp.Second())
	}
	if nextCursor != "" {
		t.Errorf("nextCursor should be empty when all returned, got %q", nextCursor)
	}
}

func TestAuditRing_Pagination(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	for i := 0; i < 10; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, 0, i, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			OK:        true,
		})
	}

	// First page: 3 entries (newest first: sec=9,8,7)
	entries, nextCursor := ring.Entries(3, "")
	if len(entries) != 3 {
		t.Fatalf("page 1: expected 3, got %d", len(entries))
	}
	if nextCursor == "" {
		t.Fatal("page 1: nextCursor should not be empty")
	}
	if entries[0].Timestamp.Second() != 9 {
		t.Errorf("page 1 first entry sec=9, got %d", entries[0].Timestamp.Second())
	}

	// Second page: skip entries <= cursor (sec=6), remaining = [7,8,9]
	// Take last 2 (since only 2 remain): sec=9,8
	entries2, nextCursor2 := ring.Entries(3, nextCursor)
	if len(entries2) != 2 {
		t.Fatalf("page 2: expected 2, got %d", len(entries2))
	}
	if entries2[0].Timestamp.Second() != 9 {
		t.Errorf("page 2 first entry sec=9, got %d", entries2[0].Timestamp.Second())
	}
	// No more pages
	if nextCursor2 != "" {
		t.Errorf("page 2: nextCursor should be empty, got %q", nextCursor2)
	}
}

func TestAuditRing_WrapsAround(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	// Fill the ring past capacity (1000)
	for i := 0; i < 1050; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, i/60, i%60, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			OK:        true,
		})
	}

	// Should have exactly 1000 entries
	entries, _ := ring.Entries(2000, "")
	if len(entries) != 1000 {
		t.Fatalf("expected 1000 entries, got %d", len(entries))
	}

	// Newest should be entry 1049
	if entries[0].Timestamp.Minute() != 1049/60 || entries[0].Timestamp.Second() != 1049%60 {
		t.Errorf("newest entry unexpected: %v", entries[0].Timestamp)
	}

	// Oldest should be entry 50 (first 50 were overwritten)
	if entries[999].Timestamp.Minute() != 50/60 || entries[999].Timestamp.Second() != 50%60 {
		t.Errorf("oldest entry unexpected: %v", entries[999].Timestamp)
	}
}

func TestAuditRing_Empty(t *testing.T) {
	ring := &AuditRing{path: filepath.Join(t.TempDir(), "audit.json")}

	entries, nextCursor := ring.Entries(10, "")
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
	if nextCursor != "" {
		t.Errorf("nextCursor should be empty, got %q", nextCursor)
	}
}

func TestAuditRing_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.json")

	// Create and persist
	ring := &AuditRing{path: path}
	for i := 0; i < 3; i++ {
		ring.Append(CommandAudit{
			Timestamp: time.Date(2026, 9, 9, 10, 0, i, 0, time.UTC),
			Cmd:       "set",
			Key:       "node_name",
			Value:     "nyc-1",
			OK:        true,
		})
	}
	if err := ring.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Load into new ring via loadJSONWithRecovery
	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	ok, err := loadJSONWithRecovery(path, &saved)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(saved.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(saved.Entries))
	}

	// Replay into new ring
	ring2 := &AuditRing{path: path}
	for _, e := range saved.Entries {
		ring2.Append(e)
	}

	entries, _ := ring2.Entries(10, "")
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries from ring2, got %d", len(entries))
	}
	// Verify content survived round-trip
	if entries[0].Key != "node_name" || entries[0].Value != "nyc-1" {
		t.Errorf("entry content wrong: %+v", entries[0])
	}
}

func TestAuditRing_NilSafe(t *testing.T) {
	// nil ring should not panic
	var ring *AuditRing
	ring.Append(CommandAudit{}) // should not panic
	entries, cursor := ring.Entries(10, "")
	if entries != nil || cursor != "" {
		t.Error("nil ring should return empty")
	}
}

// The hotswap parent exits via os.Exit without reaching main()'s graceful
// shutdown persist, so forceAuditPersist must flush entries even inside
// the 30s persist window or the last commands before an update are lost.
func TestForceAuditPersistWritesWithinPersistWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	ring := &AuditRing{path: path}
	prevRing := globalAuditRing
	globalAuditRing = ring
	defer func() { globalAuditRing = prevRing }()
	prevPersist := lastAuditPersist
	defer func() { lastAuditPersist = prevPersist }()

	lastAuditPersist = time.Now() // fresh persist: recordAndPersist alone skips disk
	recordProcessStart(false)
	recordAndPersist(CommandAudit{
		Timestamp: time.Now(),
		Cmd:       "hotswap",
		Key:       "version",
		Value:     RequireVersion(),
		Source:    "trigger",
		OK:        true,
	})

	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	if ok, _ := loadJSONWithRecovery(path, &saved); ok {
		t.Fatal("recordAndPersist wrote inside the 30s window — test premise broken")
	}

	forceAuditPersist()
	if ok, err := loadJSONWithRecovery(path, &saved); err != nil {
		t.Fatalf("reload after forceAuditPersist: %v", err)
	} else if !ok {
		t.Fatal("audit.json missing after forceAuditPersist")
	}
	if len(saved.Entries) != 2 {
		t.Fatalf("expected both unsaved entries on disk, got %d", len(saved.Entries))
	}
	if saved.Entries[0].Cmd != "start" || saved.Entries[0].Source != "boot" {
		t.Errorf("first entry: %+v, want start/boot", saved.Entries[0])
	}
	if saved.Entries[1].Cmd != "hotswap" {
		t.Errorf("second entry: %+v, want hotswap", saved.Entries[1])
	}
}

// A HotSwap candidate loads audit.json at spawn time, before the parent's
// final flush. mergeAuditRingFromDisk must pull the parent's post-spawn
// entries into the successor's live ring exactly once (dedupe), while
// leaving entries the candidate already has untouched.
func TestMergeAuditRingFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	prevRing := globalAuditRing
	defer func() { globalAuditRing = prevRing }()
	prevPersist := lastAuditPersist
	defer func() { lastAuditPersist = prevPersist }()

	parent := &AuditRing{path: path}
	globalAuditRing = parent
	lastAuditPersist = time.Time{}
	recordAndPersist(CommandAudit{Timestamp: t0(), Cmd: "set", Key: "metrics", Value: "on", OK: true})
	recordAndPersist(CommandAudit{Timestamp: t1(), Cmd: "hotswap", Key: "version", Value: "v3.23.0", Source: "trigger", OK: true})
	forceAuditPersist()

	// Candidate: spawn-time snapshot (replays what it loaded) plus its own
	// start entry; the 30s gate keeps it from persisting over the parent.
	cand := &AuditRing{path: path}
	globalAuditRing = cand
	cand.Append(CommandAudit{Timestamp: t0(), Cmd: "set", Key: "metrics", Value: "on", OK: true})
	lastAuditPersist = time.Now()
	recordProcessStart(false)

	// Parent records one more entry during the drain window and flushes.
	globalAuditRing = parent
	recordAndPersist(CommandAudit{Timestamp: t2(), Cmd: "set", Key: "node_name", Value: "nyc-1", OK: true})
	forceAuditPersist()

	// Takeover: the successor merges what the parent flushed.
	globalAuditRing = cand
	mergeAuditRingFromDisk()

	entries, _ := cand.Entries(10, "")
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Cmd+"|"+e.Key]++
	}
	want := map[string]int{
		"set|metrics":     1,
		"hotswap|version": 1,
		"set|node_name":   1,
		"start|version":   1,
	}
	for k, n := range want {
		if seen[k] != n {
			t.Errorf("entry %q count = %d, want %d (entries %+v)", k, seen[k], n, entries)
		}
	}
	if len(entries) != len(want) {
		t.Errorf("total entries = %d, want %d", len(entries), len(want))
	}
}

// Merge on a missing/corrupt file must be a silent no-op, not a crash.
func TestMergeAuditRingFromDiskNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	prevRing := globalAuditRing
	globalAuditRing = &AuditRing{path: path}
	defer func() { globalAuditRing = prevRing }()

	mergeAuditRingFromDisk() // must not panic
	if got := globalAuditRing.size; got != 0 {
		t.Errorf("ring size = %d, want 0", got)
	}
}

// recordAndPersist runs from concurrent control-socket goroutines while a
// hotswap drain can call forceAuditPersist; the persist gate must be
// race-free (caught by -race) and every entry must land. The gate is armed
// OPEN (zero time) so the goroutines genuinely race to WRITE the gate, not
// just read it.
func TestRecordAndPersistConcurrentRaceGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	ring := &AuditRing{path: path}
	prevRing := globalAuditRing
	globalAuditRing = ring
	defer func() { globalAuditRing = prevRing }()
	prevPersist := lastAuditPersist
	defer func() { lastAuditPersist = prevPersist }()
	lastAuditPersist = time.Time{} // gate open: first due write persists

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				recordAndPersist(CommandAudit{Timestamp: time.Now(), Cmd: "set", Key: "k", OK: true})
			}
		}()
	}
	wg.Wait()

	if got := ring.size; got != 8*50 {
		t.Fatalf("ring size = %d, want %d", got, 8*50)
	}
}

// The handoff record must reach disk immediately (not wait on the 30s
// gate): the successor's takeover merge reads audit.json, so a gated
// write would lose the event. Removing the forceAuditPersist inside
// recordHotSwapAndFlush makes this test fail.
func TestRecordHotSwapAndFlushWritesImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	ring := &AuditRing{path: path}
	prevRing := globalAuditRing
	globalAuditRing = ring
	defer func() { globalAuditRing = prevRing }()
	prevPersist := lastAuditPersist
	defer func() { lastAuditPersist = prevPersist }()
	lastAuditPersist = time.Now() // gate closed: only forceAuditPersist can write

	recordHotSwapAndFlush()

	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	ok, err := loadJSONWithRecovery(path, &saved)
	if err != nil || !ok {
		t.Fatalf("hotswap event not flushed to disk (ok=%v err=%v)", ok, err)
	}
	if len(saved.Entries) != 1 || saved.Entries[0].Cmd != "hotswap" {
		t.Fatalf("expected exactly the hotswap entry on disk, got %+v", saved.Entries)
	}
}

var auditT0 = time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

func t0() time.Time { return auditT0 }
func t1() time.Time { return auditT0.Add(1 * time.Second) }
func t2() time.Time { return auditT0.Add(2 * time.Second) }
