package main

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func auditAt(base time.Time, sec int, key string) CommandAudit {
	return CommandAudit{Timestamp: base.Add(time.Duration(sec) * time.Second), Cmd: "set", Key: key, Source: "test", OK: true}
}

func fillRing(r *AuditRing, entries []CommandAudit) {
	for _, e := range entries {
		r.Append(e)
	}
}

func writeDisk(t *testing.T, path string, entries []CommandAudit) {
	t.Helper()
	if err := atomicWriteJSON(path, map[string]interface{}{"entries": entries}); err != nil {
		t.Fatal(err)
	}
}

func withGlobalRing(t *testing.T, r *AuditRing) {
	t.Helper()
	prev := globalAuditRing
	globalAuditRing = r
	t.Cleanup(func() { globalAuditRing = prev })
}

// A full live ring of NEWER entries must not be displaced by a parent
// snapshot of OLDER ones: appending the disk entries one at a time into a full
// ring evicted the newest live entries.
func TestMergeAuditRingKeepsNewestWhenFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var older, newer []CommandAudit
	for i := 0; i < 1000; i++ {
		older = append(older, auditAt(base, i, "old"))
		newer = append(newer, auditAt(base, 1000+i, "new"))
	}
	writeDisk(t, path, older)
	live := &AuditRing{path: path}
	fillRing(live, newer)
	withGlobalRing(t, live)

	mergeAuditRingFromDisk()

	got, _ := live.Entries(1000, "")
	if len(got) != 1000 {
		t.Fatalf("ring has %d entries, want 1000", len(got))
	}
	for _, e := range got {
		if e.Key != "new" {
			t.Fatalf("an older disk entry %v displaced a newer live one", e.Timestamp)
		}
	}
}

// The handoff cascade: the successor has recorded its start entry and a newer
// command, the parent's final flush adds one more entry, and both rings are
// near capacity. The result must be the newest 1000 of the union, each once,
// in timestamp order.
func TestMergeAuditRingFullRingCascadeIsAStableUnion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var shared []CommandAudit
	for i := 0; i < 999; i++ {
		shared = append(shared, auditAt(base, i, "shared"))
	}
	parentFinal := auditAt(base, 1000, "parent-final")
	start := auditAt(base, 1001, "successor-start")
	command := auditAt(base, 1002, "successor-command")

	disk := append(append([]CommandAudit{}, shared...), parentFinal)
	writeDisk(t, path, disk)

	live := &AuditRing{path: path}
	fillRing(live, shared[1:]) // the successor already evicted the oldest shared entry
	fillRing(live, []CommandAudit{start, command})
	withGlobalRing(t, live)

	mergeAuditRingFromDisk()

	got, _ := live.Entries(1000, "")
	if len(got) != 1000 {
		t.Fatalf("ring has %d entries, want 1000", len(got))
	}
	seen := map[string]int{}
	for _, e := range got {
		seen[e.Key]++
	}
	for _, key := range []string{"parent-final", "successor-start", "successor-command"} {
		if seen[key] != 1 {
			t.Errorf("%s appears %d times, want 1", key, seen[key])
		}
	}
	if seen["shared"] != 997 {
		t.Errorf("shared entries = %d, want 997 (the newest 1000 of 1002 unique entries)", seen["shared"])
	}
	for i := 1; i < len(got); i++ { // newest first
		if got[i].Timestamp.After(got[i-1].Timestamp) {
			t.Fatalf("entries are not in timestamp order at %d", i)
		}
	}
}

// The reconciliation persist only happens when the retained window actually
// gained entries from disk.
func TestMergeAuditRingPersistsOnlyWhenItImportedSomething(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	onDisk := []CommandAudit{auditAt(base, 1, "a"), auditAt(base, 2, "b")}
	writeDisk(t, path, onDisk)
	live := &AuditRing{path: path}
	fillRing(live, onDisk)
	withGlobalRing(t, live)

	var persists atomic.Int32
	prev := auditPersistHook
	auditPersistHook = func() { persists.Add(1) }
	t.Cleanup(func() { auditPersistHook = prev })

	mergeAuditRingFromDisk()
	if got := persists.Load(); got != 0 {
		t.Fatalf("persisted %d times although disk had nothing new", got)
	}

	writeDisk(t, path, append(onDisk, auditAt(base, 3, "c")))
	mergeAuditRingFromDisk()
	if got := persists.Load(); got != 1 {
		t.Fatalf("persisted %d times after importing one entry, want 1", got)
	}
}

// Two persists must never overlap: each snapshots the ring and then writes the
// file, and a snapshot taken before an import can otherwise be written after
// the reconciliation's own persist, removing the imported entry from disk.
func TestAuditPersistsAreSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.json")
	live := &AuditRing{path: path}
	withGlobalRing(t, live)
	prevPersist := lastAuditPersist
	t.Cleanup(func() { lastAuditPersist = prevPersist })

	var active, maxActive atomic.Int32
	prev := auditPersistHook
	auditPersistHook = func() {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		active.Add(-1)
	}
	t.Cleanup(func() { auditPersistHook = prev })

	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				live.Append(auditAt(base, g*100+i, "f"))
				forceAuditPersist()
			}
		}(g)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				auditPersistMu.Lock()
				lastAuditPersist = time.Time{} // open the 30 s gate
				auditPersistMu.Unlock()
				recordAndPersist(auditAt(base, 5000+g*100+i, "r"))
			}
		}(g)
	}
	wg.Wait()

	if got := maxActive.Load(); got != 1 {
		t.Fatalf("up to %d persists ran at once, want them serialized", got)
	}
}
