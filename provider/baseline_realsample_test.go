package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBaselineRealShortRun is the brief's "real short run": it drives the actual
// recorder (the real reader, the real append, the real file) rather than
// synthesising rows, so the file it produces is a genuine artifact.
func TestBaselineRealShortRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("URNETWORK_BASELINE_INTERVAL", "1m")
	// This test must not inherit the recorder being switched off by an earlier
	// test, nor a rate limiter that swallows the write warnings it is checking.
	prevEnabled := baselineEnabled.Load()
	baselineEnabled.Store(true)
	t.Cleanup(func() { baselineEnabled.Store(prevEnabled) })
	baselineWarnLast.Store(0)
	if got := resolveBaselineInterval(); got != time.Minute {
		t.Fatalf("the override did not take: %v", got)
	}
	// The start mark and two samples, exactly as baselineRun would, with the
	// first-sample delay bypassed for the sake of a test that finishes.
	baselineRecord(baselineStartSample("v3.23.0-fix.32.9", "v3.23.0-fix.32.8", time.Now()))
	baselineRecord(buildBaselineSample(baselineCollect(time.Now())))
	baselineRecord(buildBaselineSample(baselineCollect(time.Now())))
	if _, err := baselineMark("before the 32.9 rollout"); err != nil {
		t.Fatalf("mark: %v", err)
	}

	path := filepath.Join(dir, ".urnetwork", baselineFileName)
	rows, err := baselineTail(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("wrote %d rows, want 4 (start, 2 samples, mark)", len(rows))
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := func() os.FileMode { return st.Mode().Perm() }
	if mode()&0o077 != 0 {
		t.Errorf("file mode is %o; the record must not be readable by other users", mode())
	}
	// The samples must be REAL readings, not placeholders: on this machine
	// mem_available_mib and rss_bytes must be present.
	var sawAvail, sawRSS bool
	for _, r := range rows {
		if r.Kind != "sample" {
			continue
		}
		if r.Host != nil && r.Host.MemAvailableMiB != nil {
			sawAvail = true
		}
		if r.Resources.RSSBytes != nil && *r.Resources.RSSBytes > 0 {
			sawRSS = true
		}
	}
	if !sawAvail {
		t.Error("no sample carried mem_available_mib on a host that can read it")
	}
	if !sawRSS {
		t.Error("no sample carried a non-zero rss_bytes")
	}
	fmt.Printf("BASELINE-RUN path=%s bytes=%d rows=%d\n", path, st.Size(), len(rows))
	for _, r := range rows {
		fmt.Printf("  %s kind=%s v=%s up=%d availMiB=%s\n", r.TS, r.Kind, r.Version, r.Proxies.Up,
			availStr(r))
	}
}

func availStr(r baselineSample) string {
	if r.Host == nil || r.Host.MemAvailableMiB == nil {
		return "-"
	}
	return fmt.Sprintf("%d (%s)", *r.Host.MemAvailableMiB, r.Host.AvailSource)
}
