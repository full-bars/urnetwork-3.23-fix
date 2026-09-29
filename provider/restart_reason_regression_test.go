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

// A crash must never be reported as a clean exit. The .clean-shutdown marker is
// consumed on read: it means "the process that just exited did so cleanly" and
// is only true until the next start reads it. An earlier revision left it on
// disk to distinguish a restart from a crash, but a crash never runs the
// clean-exit path, so the stale marker survived and EVERY later crash was
// reported as "clean". Caught in review and reproduced.
//
// The sequence that matters is three boots, not two:
//
//	boot 1 exits cleanly and leaves the marker
//	boot 2 starts, consumes the marker, and IS a clean exit (correct)
//	boot 3 is the OOM kill: boot 2 never got to write a clean-exit marker, so
//	       there is nothing to consume and boot 3 must read unclean
//
// With the marker left on disk, boot 3 still finds boot 1's file and lies.
func TestCrashAfterACleanShutdownIsNotReportedAsClean(t *testing.T) {
	home := withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".clean-shutdown")

	boot := func() string {
		saved := startupDiag
		startupDiag = &startupDiagnostics{}
		detectStartup()
		got := startupDiag.restartReason
		startupDiag = saved
		return got
	}

	// Boot 1: exits cleanly, leaving the marker behind.
	markCleanShutdownIn(dir)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("a clean exit must leave the marker for the next start")
	}

	// Boot 2: consumes it. This start followed a clean exit, so "clean" is right.
	if got := boot(); got != restartReasonClean {
		t.Fatalf("boot 2 followed a clean exit and must read %q, got %q", restartReasonClean, got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the marker must be consumed on read, stat err = %v", err)
	}

	// Boot 3: boot 2 was OOM-killed before it could write a clean-exit marker,
	// so there is nothing on disk and this must NOT read clean.
	if got := boot(); got == restartReasonClean {
		t.Fatal("a crash after a clean shutdown was reported as clean: a stale marker was trusted")
	}
	_ = home
}

// The control case for the test above: with the marker genuinely present, a
// clean exit is still reported as clean, so the test is not passing merely
// because the marker is ignored.
func TestCleanShutdownIsStillReportedWhenTheMarkerIsPresent(t *testing.T) {
	withTempHome(t)
	dir, err := oomCapDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	markCleanShutdownIn(dir)

	saved := startupDiag
	startupDiag = &startupDiagnostics{}
	detectStartup()
	got := startupDiag.restartReason
	startupDiag = saved

	if got != restartReasonClean {
		t.Fatalf("a clean exit must still be reported as clean, got %q", got)
	}
}
