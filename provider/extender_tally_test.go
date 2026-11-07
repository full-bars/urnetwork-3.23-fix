package main

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestTally(t *testing.T, now *time.Time) *extenderTally {
	t.Helper()
	tally := newExtenderTally(filepath.Join(t.TempDir(), extenderTallyFileName))
	tally.now = func() time.Time { return *now }
	return tally
}

// Requests served are counted by the hour and reported over the last 24 hours,
// the last 7 days and the lifetime, so a busy day and a quiet month both read
// honestly.
func TestExtenderTallyWindows(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tally := newTestTally(t, &now)

	tally.Observe(10) // first reading is the baseline: 10 requests already counted
	w := tally.Windows()
	if w.Last24h != 10 || w.Last7d != 10 || w.Lifetime != 10 {
		t.Fatalf("after first observe: %+v", w)
	}

	now = now.Add(30 * time.Hour)
	tally.Observe(15) // 5 more, 30h later
	w = tally.Windows()
	if w.Last24h != 5 {
		t.Errorf("last 24h = %d, want 5 (the 10 are 30h old)", w.Last24h)
	}
	if w.Last7d != 15 || w.Lifetime != 15 {
		t.Errorf("7d/lifetime = %d/%d, want 15/15", w.Last7d, w.Lifetime)
	}

	now = now.Add(8 * 24 * time.Hour)
	w = tally.Windows()
	if w.Last24h != 0 || w.Last7d != 0 {
		t.Errorf("after 8 idle days 24h/7d = %d/%d, want 0/0", w.Last24h, w.Last7d)
	}
	if w.Lifetime != 15 {
		t.Errorf("lifetime = %d, want 15: it never expires", w.Lifetime)
	}
}

// A role that restarts starts its counters from zero. That is a new baseline,
// not a negative delta and not a second count of what it already served.
func TestExtenderTallyRoleRestartRebases(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tally := newTestTally(t, &now)
	tally.Observe(100)
	tally.Rebase() // the role was replaced
	tally.Observe(3)
	if w := tally.Windows(); w.Lifetime != 103 {
		t.Fatalf("lifetime = %d, want 103 (100 + 3 from the new role)", w.Lifetime)
	}
	// a counter that goes down without a rebase is treated as a restart too
	tally.Observe(1)
	if w := tally.Windows(); w.Lifetime != 104 {
		t.Fatalf("lifetime = %d, want 104 after an unexpected counter reset", w.Lifetime)
	}
}

// A reboot must not zero the history.
func TestExtenderTallyPersists(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), extenderTallyFileName)
	first := newExtenderTally(path)
	first.now = func() time.Time { return now }
	first.Observe(7)
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}

	second := newExtenderTally(path)
	second.now = func() time.Time { return now.Add(time.Hour) }
	if err := second.Load(); err != nil {
		t.Fatal(err)
	}
	w := second.Windows()
	if w.Lifetime != 7 || w.Last24h != 7 {
		t.Fatalf("after reload: %+v, want 7 in both", w)
	}
	second.Observe(2) // the new process's counters start from zero: 2 is all new
	if w := second.Windows(); w.Lifetime != 9 {
		t.Fatalf("lifetime = %d, want 9", w.Lifetime)
	}
}

func TestExtenderTallyOldBucketsArePruned(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tally := newTestTally(t, &now)
	tally.Observe(1)
	now = now.Add(30 * 24 * time.Hour)
	tally.Rebase()
	tally.Observe(1)
	if n := tally.bucketCount(); n != 1 {
		t.Fatalf("bucket count = %d, want 1: month-old hours are folded into the lifetime and dropped", n)
	}
	if w := tally.Windows(); w.Lifetime != 2 {
		t.Fatalf("lifetime = %d, want 2", w.Lifetime)
	}
}

func TestExtenderRequestsServedCountsClientServices(t *testing.T) {
	// proxy is ONLY ordinary non-extender http (scanners and browsers hitting 443,
	// answered by the camouflage reverse proxy): never a client. probe is the
	// platform's own verification and gossip is mesh traffic.
	counts := map[string]uint64{"proxy": 400, "forward": 3, "feed": 2, "probe": 50, "gossip": 9, "other": 1}
	if got := extenderRequestsServed(counts, nil); got != 5 {
		t.Fatalf("requests served = %d, want 5 (forward+feed only)", got)
	}
	// refused requests were counted as a service before they were refused, so they
	// are taken back out; refusals of requests that never decoded (h2, bad header)
	// were never counted as a service and must not be subtracted
	refusals := map[string]uint64{"destination authorization": 1, "request": 7, "header decode": 3}
	if got := extenderRequestsServed(counts, refusals); got != 4 {
		t.Fatalf("requests served with a refusal = %d, want 4", got)
	}
	// never negative
	if got := extenderRequestsServed(map[string]uint64{"forward": 1}, map[string]uint64{"destination authorization": 9}); got != 0 {
		t.Fatalf("requests served = %d, want 0", got)
	}
}

// Hours are the bucket, so a window is accurate to the hour. The hour a window
// starts inside is counted in full, never dropped: a 24h window read at 12:30
// includes everything from yesterday's 12:00 hour on.
func TestExtenderTallyWindowIncludesTheBoundaryHour(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	tally := newTestTally(t, &now)
	// 3 requests land in the hour that starts 24h before the window's start hour
	now = time.Date(2026, 11, 1, 12, 40, 0, 0, time.UTC)
	tally.Observe(3)
	now = time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	if w := tally.Windows(); w.Last24h != 3 {
		t.Fatalf("last 24h = %d, want 3: the boundary hour must be counted, not dropped", w.Last24h)
	}
}
