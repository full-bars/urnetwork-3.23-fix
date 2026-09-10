package main

import (
	"fmt"
	"path/filepath"
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

// persist writes the ring buffer to disk atomically.
func (r *AuditRing) persist() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	ordered := make([]CommandAudit, 0, r.size)
	if r.size < len(r.entries) {
		ordered = append(ordered, r.entries[:r.size]...)
	} else {
		ordered = append(ordered, r.entries[r.head:]...)
		ordered = append(ordered, r.entries[:r.head]...)
	}
	r.mu.Unlock()

	payload := map[string]interface{}{
		"entries": ordered,
	}
	return atomicWriteJSON(r.path, payload)
}

// recordAndPersist appends an entry and persists periodically (not every
// write — the 1000-entry ring means at most ~50KB, cheap to persist
// every 30 seconds).
var lastAuditPersist time.Time

func recordAndPersist(entry CommandAudit) {
	if globalAuditRing == nil {
		return
	}
	globalAuditRing.Append(entry)

	// Persist at most every 30 seconds (not every command)
	if time.Since(lastAuditPersist) > 30*time.Second {
		lastAuditPersist = time.Now()
		if err := globalAuditRing.persist(); err != nil {
			tlog("[audit] persist failed: %v\n", err)
		}
	}
}

// forceAuditPersist persists immediately (called on shutdown).
func forceAuditPersist() {
	if globalAuditRing == nil {
		return
	}
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
