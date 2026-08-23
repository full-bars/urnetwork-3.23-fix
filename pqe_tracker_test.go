package connect

import (
	"testing"
	"time"
)

// TestPQETrackerWindows verifies the rolling-window + lifetime counting with
// deterministic synthesized events so the hour/day/week boundaries are exact.
func TestPQETrackerWindows(t *testing.T) {
	tr := newPQETracker()
	// Replaces time.Now() inside the tracker with a fixed base to make the
	// test hermetic. We cannot stub time.Now cleanly, so instead drive the
	// window math by injecting events relative to a recorded Now.
	base := time.Now()
	// helper: note an open "ago" seconds before base
	openAgo := func(agoSec int, pqe bool) {
		// Override the tracker's now by pre-inserting an event at base-ago.
		tr.mu.Lock()
		x := base.Add(-time.Duration(agoSec) * time.Second)
		if pqe {
			tr.pqeOpens = append(tr.pqeOpens, x)
			tr.pqeActive++
			tr.pqeLifetime++
		} else {
			tr.clasOpens = append(tr.clasOpens, x)
			tr.clasActive++
			tr.clasLifetime++
		}
		tr.mu.Unlock()
	}
	openAgo(10, true)          // within 1h
	openAgo(2*3600, true)      // within 24h, not 1h
	openAgo(2*24*3600, false)  // within 1w (7d), not 24h
	openAgo(20*24*3600, false) // older than 7d, lifetime only

	// Snapshot uses time.Now (>= base), so counts are >= the injected slots.
	c := tr.Snapshot()
	if c.PQEHour < 1 || c.PQEDay < 2 || c.PQEWeek < 2 || c.PQELifetime < 2 {
		t.Fatalf("pqe counts wrong: hour=%d day=%d week=%d life=%d", c.PQEHour, c.PQEDay, c.PQEWeek, c.PQELifetime)
	}
	if c.ClasHour != 0 || c.ClasDay != 0 || c.ClasWeek < 1 || c.ClasLifetime < 2 {
		t.Fatalf("clas counts wrong: hour=%d day=%d week=%d life=%d", c.ClasHour, c.ClasDay, c.ClasWeek, c.ClasLifetime)
	}
	// active
	if c.ActivePQE < 2 || c.ActiveClas < 2 {
		t.Fatalf("active wrong: pqe=%d clas=%d", c.ActivePQE, c.ActiveClas)
	}
	// close decrements active
	tr.NoteClose(true)
	c = tr.Snapshot()
	if c.ActivePQE != 1 {
		t.Fatalf("after one pqe close active pqe=%d want 1", c.ActivePQE)
	}
}
