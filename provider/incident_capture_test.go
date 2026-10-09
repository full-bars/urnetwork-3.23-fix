package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var incidentT0 = time.Unix(1_800_000_000, 0)

func feedIncident(tr *incidentTracker, start time.Time, minutes int, goroutines int, heapFrac float64) (reason string) {
	for i := 0; i < minutes*6; i++ { // one sample per 10s
		if r, ok := tr.observe(incidentSample{at: start.Add(time.Duration(i) * 10 * time.Second), goroutines: goroutines, heapFrac: heapFrac}); ok {
			return r
		}
	}
	return ""
}

// The LA7 buildup: 23k goroutines for a long time, then 39k, then 61k, with the
// heap at 94% of its limit. A capture must happen on the way up, while the
// process can still run, not after it has been starved.
func TestIncidentTrackerFiresOnGoroutineGrowthBeforeTheHeapRunsAway(t *testing.T) {
	tr := newIncidentTracker()
	if r := feedIncident(tr, incidentT0, 30, 23000, 0.55); r != "" {
		t.Fatalf("a steady 23k goroutines must not capture, got %q", r)
	}
	r := feedIncident(tr, incidentT0.Add(30*time.Minute), 10, 38700, 0.58)
	if r != "goroutine-growth" {
		t.Fatalf("23k -> 38.7k (1.7x the 30 minute floor) must capture as goroutine-growth, got %q", r)
	}
}

func TestIncidentTrackerStaysQuietOnSmallOrSlowChange(t *testing.T) {
	tr := newIncidentTracker()
	// small process: a 3x jump from 800 to 2400 goroutines is under the absolute floor
	if r := feedIncident(tr, incidentT0, 20, 800, 0.3); r != "" {
		t.Fatal(r)
	}
	if r := feedIncident(tr, incidentT0.Add(20*time.Minute), 20, 2400, 0.3); r != "" {
		t.Fatalf("below the absolute floor nothing is an incident, got %q", r)
	}
	// big process, but growing 20% is not
	tr2 := newIncidentTracker()
	feedIncident(tr2, incidentT0, 30, 30000, 0.5)
	if r := feedIncident(tr2, incidentT0.Add(30*time.Minute), 20, 36000, 0.5); r != "" {
		t.Fatalf("20%% growth is ordinary load, got %q", r)
	}
	// no history yet: a young process cannot call growth
	tr3 := newIncidentTracker()
	if r := feedIncident(tr3, incidentT0, 5, 60000, 0.5); r != "" {
		t.Fatalf("5 minutes of history is not a baseline, got %q", r)
	}
}

func TestIncidentTrackerHeapRulesAndCooldowns(t *testing.T) {
	tr := newIncidentTracker()
	if r := feedIncident(tr, incidentT0, 2, 5000, 0.93); r != "heap-near-limit" {
		t.Fatalf("93%% of the limit must capture, got %q", r)
	}
	// the same reason does not repeat inside its cooldown
	if r := feedIncident(tr, incidentT0.Add(2*time.Minute), 20, 5000, 0.95); r != "" {
		t.Fatalf("heap-near-limit must not repeat within 30 minutes, got %q", r)
	}
	// but the escalation is a different reason and gets its own capture
	if r := feedIncident(tr, incidentT0.Add(22*time.Minute), 2, 5000, 1.3); r != "heap-over-limit" {
		t.Fatalf("1.3x the limit must capture as heap-over-limit, got %q", r)
	}
	// and the day has a ceiling: capture every time a cooldown allows, for 23 hours
	tr2 := newIncidentTracker()
	captures := 0
	for i := 0; i < 40; i++ {
		start := incidentT0.Add(time.Duration(i) * 35 * time.Minute)
		frac := 0.95
		if i%2 == 1 {
			frac = 1.3
		}
		if r := feedIncident(tr2, start, 2, 5000, frac); r != "" {
			captures++
		}
	}
	if captures != incidentMaxPerDay {
		t.Fatalf("captured %d times within a day, want exactly the ceiling of %d (every allowed capture happens, none beyond)", captures, incidentMaxPerDay)
	}
}

func TestCaptureIncidentWritesTheProfilesAndPrunes(t *testing.T) {
	home := withTempHome(t)
	now := incidentT0
	for i := 0; i < incidentKeep+3; i++ {
		dir, err := captureIncident(now.Add(time.Duration(i)*time.Hour), "goroutine-growth", incidentSample{goroutines: 61000, heapFrac: 0.94})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"goroutines.txt", "heap.pb.gz", "meta.txt", "summary.txt"} {
			st, err := os.Stat(filepath.Join(dir, name))
			if err != nil || st.Size() == 0 {
				t.Fatalf("%s missing or empty in %s: %v", name, dir, err)
			}
		}
		summary, _ := os.ReadFile(filepath.Join(dir, "summary.txt"))
		if !strings.Contains(string(summary), " goroutines\n") {
			t.Fatalf("the summary of a real profile must list goroutine groups: %q", summary)
		}
		meta, _ := os.ReadFile(filepath.Join(dir, "meta.txt"))
		if !strings.Contains(string(meta), "reason=goroutine-growth") || !strings.Contains(string(meta), "goroutines=61000") {
			t.Fatalf("meta.txt does not carry the trigger: %s", meta)
		}
	}
	entries, err := os.ReadDir(filepath.Join(home, ".urnetwork", "incidents"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != incidentKeep {
		t.Fatalf("expected the %d newest incidents kept, found %d", incidentKeep, len(entries))
	}
}

func TestGoroutineSummaryRanksTheBiggestStacksFirst(t *testing.T) {
	profile := "goroutine profile: total 30\n" +
		"3 @ 0x1 0x2\n#\t0x1\tpkg.small+0x10\t/a.go:1\n\n" +
		"25 @ 0x3 0x4\n#\t0x3\tpkg.(*PeerConn).teardown+0x20\t/b.go:2\n#\t0x4\tpkg.caller+0x30\t/c.go:3\n\n" +
		"2 @ 0x5\n#\t0x5\tpkg.tiny+0x10\t/d.go:4\n"
	out := summarizeGoroutineProfile(profile, 2)
	if !strings.Contains(out, "25 goroutines") || !strings.Contains(out, "PeerConn).teardown") {
		t.Fatalf("the 25-goroutine stack must lead: %q", out)
	}
	if strings.Contains(out, "tiny") {
		t.Fatalf("only the top 2 stacks belong in the summary: %q", out)
	}
	if strings.Index(out, "25 goroutines") > strings.Index(out, "3 goroutines") {
		t.Fatalf("stacks must be ordered by size: %q", out)
	}
}
