package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CommandAudit records one control socket operation.
type CommandAudit struct {
	Timestamp time.Time `json:"timestamp"`
	Cmd       string    `json:"cmd"`
	Key       string    `json:"key"`
	Value     string    `json:"value,omitempty"`
	Source    string    `json:"source"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
}

// AuditRing is a fixed-size circular buffer of command audit entries.
// Max 1000 entries — no backing-array growth, no memory leak.
type AuditRing struct {
	mu      sync.Mutex
	entries [1000]CommandAudit
	head    int
	size    int
	path    string
}

var globalAuditRing *AuditRing

// initAuditRing loads or creates the audit ring at startup.
func initAuditRing() {
	stateDir := mustStateDir()
	if stateDir == "" {
		return
	}
	path := filepath.Join(stateDir, "audit.json")
	ring := &AuditRing{path: path}

	// Load existing audit entries with checksum recovery
	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	if ok, err := loadJSONWithRecovery(path, &saved); ok && len(saved.Entries) > 0 {
		// Replay saved entries into the ring
		for _, e := range saved.Entries {
			ring.entries[ring.head] = e
			ring.head = (ring.head + 1) % len(ring.entries)
			if ring.size < len(ring.entries) {
				ring.size++
			}
		}
	} else if err != nil {
		tlog("[audit] error loading audit log: %v\n", err)
	}

	globalAuditRing = ring
}

// recordProcessStart appends this process's startup to the audit ring so
// `urnet-tools history` shows lifecycle events (an update landing, a
// crash-restart), not just control-socket config changes. Called right
// after initAuditRing. candidate marks a HotSwap successor (spawned
// candidate, or Docker execve successor via URNETWORK_HOTSWAP_EXEC) for
// the source label; deferPersist is true only for a SPAWNED candidate,
// whose ring was loaded before the parent's final flush and must not
// clobber it until the takeover merge.
func recordProcessStart(candidate, deferPersist bool) {
	src := "boot"
	if candidate {
		src = "hotswap"
	}
	if deferPersist {
		// A spawned candidate's ring predates the parent's final flush,
		// so writing it would clobber the authoritative on-disk state.
		// Arm the 30s gate now — the start entry stays in memory until
		// the takeover merge pulls the parent's entries in. Normal boots
		// and Docker execve successors (ring loaded after the parent's
		// flush) persist immediately.
		auditPersistMu.Lock()
		lastAuditPersist = time.Now()
		auditPersistMu.Unlock()
	}
	recordAndPersist(CommandAudit{
		Timestamp: time.Now(),
		Cmd:       "start",
		Key:       "version",
		Value:     RequireVersion(),
		Source:    src,
		OK:        true,
	})
}

// mergeAuditRingFromDisk reloads audit.json and appends entries the live
// ring does not already have. A HotSwap candidate calls this after the
// takeover handshake completes: its own ring was loaded from disk at
// spawn time, which precedes the parent's final flush, so entries the
// parent recorded in its last moments (the "hotswap" event, the final
// control-socket commands) exist only on disk until this merge pulls them
// into the successor's live ring. The parent flushes before the takeover
// message, so by the time this runs those entries are on disk.
func mergeAuditRingFromDisk() {
	if globalAuditRing == nil || globalAuditRing.path == "" {
		return
	}
	var saved struct {
		Entries []CommandAudit `json:"entries"`
	}
	ok, err := loadJSONWithRecovery(globalAuditRing.path, &saved)
	if err != nil || !ok {
		return
	}
	globalAuditRing.mu.Lock()
	merged, imported := unionAuditEntries(globalAuditRing.orderedLocked(), saved.Entries, len(globalAuditRing.entries))
	if imported > 0 {
		globalAuditRing.replaceLocked(merged)
	}
	globalAuditRing.mu.Unlock() // persist() takes r.mu itself; never call it under the lock
	if imported == 0 {
		return
	}
	// Serialize the merge persist with recordAndPersist/forceAuditPersist:
	// persist() snapshots the ring and then writes the file, and two of them
	// running at once can write out of order (or collide on the temp file) and
	// leave a stale snapshot on disk.
	auditPersistMu.Lock()
	err = globalAuditRing.persist()
	auditPersistMu.Unlock()
	if err != nil {
		tlog("[audit] merge persist failed: %v\n", err)
	}
}

// auditEntryKey identifies an entry for deduplication. The instant is compared
// by UnixNano, not by time.Time: entries loaded from disk carry freshly-parsed
// fixed-offset locations, and == compares the location pointer, so two entries
// for the same instant would never match after a JSON round-trip. Same-instant
// identical commands count as one (idempotent config sets ARE one state change).
type auditEntryKey struct {
	cmd, key, value, source, errText string
	ok                               bool
	at                               int64
}

func auditKeyOf(e CommandAudit) auditEntryKey {
	return auditEntryKey{e.Cmd, e.Key, e.Value, e.Source, e.Error, e.OK, e.Timestamp.UnixNano()}
}

// unionAuditEntries merges the live ring (oldest to newest) with entries read
// from disk into one deduplicated, timestamp-ordered slice, keeping only the
// newest capacity entries. imported is how many of the kept entries came from
// disk. Appending disk entries one at a time into a full ring instead let each
// older parent entry evict a newer successor entry, and the evicted shared
// entries then looked missing and were re-added.
func unionAuditEntries(live, disk []CommandAudit, capacity int) (merged []CommandAudit, imported int) {
	type tagged struct {
		e    CommandAudit
		disk bool
	}
	all := make([]tagged, 0, len(live)+len(disk))
	seen := make(map[auditEntryKey]struct{}, len(live)+len(disk))
	for _, e := range live {
		seen[auditKeyOf(e)] = struct{}{}
		all = append(all, tagged{e, false})
	}
	for _, e := range disk {
		k := auditKeyOf(e)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		all = append(all, tagged{e, true})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].e.Timestamp.Before(all[j].e.Timestamp) })
	if capacity > 0 && len(all) > capacity {
		all = all[len(all)-capacity:]
	}
	merged = make([]CommandAudit, len(all))
	for i, t := range all {
		merged[i] = t.e
		if t.disk {
			imported++
		}
	}
	return merged, imported
}

// orderedLocked returns the ring's entries oldest to newest. Caller holds r.mu.
func (r *AuditRing) orderedLocked() []CommandAudit {
	ordered := make([]CommandAudit, 0, r.size)
	if r.size < len(r.entries) {
		return append(ordered, r.entries[:r.size]...)
	}
	ordered = append(ordered, r.entries[r.head:]...)
	return append(ordered, r.entries[:r.head]...)
}

// replaceLocked replaces the ring's contents with entries (oldest to newest,
// at most the ring capacity). Caller holds r.mu.
func (r *AuditRing) replaceLocked(entries []CommandAudit) {
	r.entries = [len(r.entries)]CommandAudit{}
	n := copy(r.entries[:], entries)
	r.size = n
	r.head = n % len(r.entries)
}

// hasLocked reports whether an equivalent entry is already in the ring.
// Caller must hold r.mu. Timestamps are compared with time.Time.Equal, NOT
// ==: entries loaded from disk carry freshly-parsed fixed-offset locations,
// and == compares the location pointer, so two entries for the same instant
// would never dedupe after a JSON round-trip.
func (r *AuditRing) hasLocked(want CommandAudit) bool {
	for i := 0; i < r.size; i++ {
		var e CommandAudit
		if r.size < len(r.entries) {
			e = r.entries[i] // buffer not full: entries start at index 0
		} else {
			e = r.entries[(r.head+i)%len(r.entries)]
		}
		// hasLocked treats same-instant identical commands as one (idempotent
		// config sets ARE one state change), trading a false dedupe for the
		// timezone-correct dedupe that == cannot provide.
		if e.Cmd == want.Cmd && e.Key == want.Key && e.Value == want.Value &&
			e.Source == want.Source && e.OK == want.OK && e.Error == want.Error &&
			e.Timestamp.Equal(want.Timestamp) {
			return true
		}
	}
	return false
}

// Append adds an entry to the ring buffer (non-blocking, fast).
func (r *AuditRing) Append(entry CommandAudit) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries[r.head] = entry
	r.head = (r.head + 1) % len(r.entries)
	if r.size < len(r.entries) {
		r.size++
	}
	r.mu.Unlock()
}

// Entries returns up to `limit` entries, starting after `cursor` (a timestamp
// string "2006-01-02T15:04:05Z07:00"). Empty cursor returns from the most
// recent entry. Results are newest-first.
func (r *AuditRing) Entries(limit int, cursor string) ([]CommandAudit, string) {
	if r == nil {
		return nil, ""
	}
	if limit <= 0 {
		limit = 50
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.size == 0 {
		return nil, ""
	}

	// Build ordered slice (oldest to newest)
	ordered := make([]CommandAudit, r.size)
	if r.size < len(r.entries) {
		// Buffer not yet full — entries start at index 0
		copy(ordered, r.entries[:r.size])
	} else {
		// Buffer full — entries start at head (oldest)
		n := copy(ordered, r.entries[r.head:])
		copy(ordered[n:], r.entries[:r.head])
	}

	// Filter by cursor: skip entries with timestamp <= cursor.
	// Cursor is the oldest entry from the previous page.
	if cursor != "" {
		cursorTime, err := time.Parse(time.RFC3339, cursor)
		if err == nil {
			i := 0
			for i < len(ordered) && !ordered[i].Timestamp.After(cursorTime) {
				i++
			}
			ordered = ordered[i:]
		}
	}

	// Return newest-first
	total := len(ordered)
	if limit > total {
		limit = total
	}

	// Take the last `limit` entries (newest)
	result := make([]CommandAudit, limit)
	for i := 0; i < limit; i++ {
		result[i] = ordered[total-1-i]
	}

	// nextCursor = timestamp of the oldest returned entry.
	// Next page skips entries <= cursor via !After filter.
	nextCursor := ""
	if limit < total {
		nextCursor = result[limit-1].Timestamp.Format(time.RFC3339)
	}

	return result, nextCursor
}

// auditPersistHook runs inside persist after the snapshot and before the write.
// A seam for tests that need to observe whether persists overlap.
var auditPersistHook func()

// persist writes the ring buffer to disk atomically.
func (r *AuditRing) persist() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	ordered := r.orderedLocked()
	r.mu.Unlock()

	if auditPersistHook != nil {
		auditPersistHook()
	}
	payload := map[string]interface{}{
		"entries": ordered,
	}
	return atomicWriteJSON(r.path, payload)
}

// recordAndPersist appends an entry and persists periodically (not every
// write — the 1000-entry ring means at most ~50KB, cheap to persist
// every 30 seconds). The persist gate is mutex-guarded: recordAndPersist
// runs from concurrent control-socket goroutines while forceAuditPersist
// can fire from the hotswap drain.
var (
	auditPersistMu   sync.Mutex
	lastAuditPersist time.Time
)

func recordAndPersist(entry CommandAudit) {
	if globalAuditRing == nil {
		return
	}
	globalAuditRing.Append(entry)

	// Persist at most every 30 seconds (not every command). The lock is held
	// through the snapshot and the write: persist() snapshots the ring and then
	// writes the file, and an older snapshot written after a newer one would
	// drop entries from disk.
	auditPersistMu.Lock()
	defer auditPersistMu.Unlock()
	if time.Since(lastAuditPersist) <= 30*time.Second {
		return
	}
	lastAuditPersist = time.Now()
	if err := globalAuditRing.persist(); err != nil {
		tlog("[audit] persist failed: %v\n", err)
	}
}

// forceAuditPersist persists immediately (called on shutdown and on the
// hotswap parent exit paths).
func forceAuditPersist() {
	if globalAuditRing == nil {
		return
	}
	auditPersistMu.Lock()
	defer auditPersistMu.Unlock()
	lastAuditPersist = time.Now()
	if err := globalAuditRing.persist(); err != nil {
		tlog("[audit] shutdown persist failed: %v\n", err)
	}
}

// historyResponse is the response format for the "history" command.
type historyResponse struct {
	OK         bool           `json:"ok"`
	Entries    []CommandAudit `json:"entries,omitempty"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// handleHistory processes the "history" socket command.
func handleHistory(limit int, cursor string) historyResponse {
	if globalAuditRing == nil {
		return historyResponse{OK: false, Error: "audit ring not initialized"}
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	entries, nextCursor := globalAuditRing.Entries(limit, cursor)
	return historyResponse{
		OK:         true,
		Entries:    entries,
		NextCursor: nextCursor,
	}
}

// formatAuditSummary returns a one-line summary for logging.
func formatAuditSummary(e CommandAudit) string {
	if e.Value != "" {
		return fmt.Sprintf("%s %s=%s ok=%v src=%s", e.Cmd, e.Key, e.Value, e.OK, e.Source)
	}
	return fmt.Sprintf("%s %s ok=%v src=%s", e.Cmd, e.Key, e.OK, e.Source)
}

// auditReconcileGrace is how long past the parent's drain timeout the
// successor waits before reconciling, so the parent's drain-end persist has
// landed first.
const auditReconcileGrace = 10 * time.Second

// reconcileAuditRingAfterHandoff re-merges audit.json into the live ring and
// persists it once the parent's drain is over.
//
// The takeover merge runs once, right after the parent yields its control
// socket, and persists the combined ring. The parent then persists AGAIN at the
// end of its drain (up to HotSwapDrainTimeout later) from its own ring, which
// holds none of the successor's entries. They are two processes, so
// auditPersistMu cannot order the writes, and the parent's can land last. The
// disk then holds a parent-only ring until the next control command more than
// 30s after the last persist, or a clean shutdown, so a crash in between loses
// the successor's start entry. Waiting past the drain, merging again (which also
// carries over anything the parent recorded during the drain) and persisting
// unconditionally puts the authoritative ring back on disk.
//
// The persist is unconditional because mergeAuditRingFromDisk persists only when
// it added something, and after the parent's overwrite it often adds nothing.
// A cancelled ctx returns without touching the disk: the provider is shutting
// down and main()'s own final persist owns the file.
func reconcileAuditRingAfterHandoff(ctx context.Context, wait time.Duration) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	mergeAuditRingFromDisk()
	forceAuditPersist()
}
