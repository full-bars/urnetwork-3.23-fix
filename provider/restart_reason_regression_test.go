package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An in-place update must still be reported as an update. The marker is written
// before the binary is fetched, verified and swapped, and the old 10-minute
// window expired on an ordinary upgrade, after which the reason fell through to
// the version-change branch and every upgraded box claimed it had restarted
// uncleanly. Observed on two boxes after the 32.1 -> 32.7 update, one reading
// "unclean (v3.23.0-fix.32.1 to v3.23.0-fix.32.7)".
func TestUpdateMarkerSurvivesASlowUpdate(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{0, 5 * time.Minute, 15 * time.Minute, 45 * time.Minute} {
		at := now.Add(-age)
		got := parseRestartMarker([]byte("update "+at.Format(time.RFC3339)), now)
		if got != "update" {
			t.Errorf("a marker %v old must still read as update, got %q", age, got)
		}
	}
	// The window still has to expire a marker from a restart that never ran.
	ancient := now.Add(-restartMarkerMaxAge - time.Minute)
	if got := parseRestartMarker([]byte("update "+ancient.Format(time.RFC3339)), now); got != "" {
		t.Errorf("a marker past the window must expire, got %q", got)
	}
}

// The .clean-shutdown marker used to be deleted the moment it was read, so the
// only evidence of a clean exit existed between the exit and the next start. A
// `systemctl restart` and an OOM kill both skip the clean-exit path, so both left
// the file absent and both reported unclean: indistinguishable. The marker now
// survives until the running process rewrites it on its own exit.
func TestCleanShutdownMarkerSurvivesUntilRewritten(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".clean-shutdown")
	markCleanShutdownIn(dir)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	// Drive the REAL startup path, which is where the marker used to be deleted.
	startupDiag.mu.Lock()
	startupDiag.loaded = false
	startupDiag.cleanShutdown = false
	startupDiag.mu.Unlock()
	detectStartup()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("detectStartup must not delete the clean-shutdown marker: %v", err)
	}
	startupDiag.mu.Lock()
	clean := startupDiag.cleanShutdown
	startupDiag.loaded = false
	startupDiag.mu.Unlock()
	if !clean {
		t.Fatal("detectStartup must see an existing marker as a clean shutdown")
	}

	// A clean exit rewrites it, so the next start still sees one.
	markCleanShutdownIn(dir)
	startupDiag.mu.Lock()
	startupDiag.loaded = false
	startupDiag.mu.Unlock()
	detectStartup()
	startupDiag.mu.Lock()
	clean2 := startupDiag.cleanShutdown
	startupDiag.loaded = false
	startupDiag.mu.Unlock()
	if !clean2 {
		t.Fatal("the marker must survive a restart, so a restarted box is not reported unclean")
	}
}
