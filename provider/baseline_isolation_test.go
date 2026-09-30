package main

import "testing"

// withTempHome is the package's isolation boundary: it redirects HOME and
// resets every piece of process-wide state a test can dirty. The baseline
// recorder adds three of them, and a test that leaves the recorder switched off
// or rate-limited would make the NEXT test in a shuffled run fail for a reason
// that has nothing to do with what it is testing.
//
// These two are order-sensitive by construction: the leak test only fails if it
// runs after a test that dirties the state, so under -shuffle they are worth
// running repeatedly.

func TestWithTempHomeResetsTheBaselineRecorderState(t *testing.T) {
	// Dirty every piece of state the recorder owns, the way a test that
	// exercises the control key or a failed write would.
	baselineEnabled.Store(false)
	baselineWarnLast.Store(1234567890)
	hookRan := false
	baselineWriteErrorHook = func() { hookRan = true }

	// The isolation boundary must undo all of it.
	dir := withTempHome(t)
	if !baselineIsEnabled() {
		t.Error("withTempHome left the recorder disabled; every later test in this " +
			"package would write no baseline, and one that checks the file would fail " +
			"for reasons unrelated to what it tests")
	}
	if got := baselineWarnLast.Load(); got != 0 {
		t.Errorf("withTempHome left the write-warning rate limiter at %d; the next "+
			"test that provokes a write error would be silenced for an hour", got)
	}
	if baselineWriteErrorHook != nil {
		t.Error("withTempHome left a test hook installed; the next test's writes " +
			"would fire it")
	}
	// And HOME really is redirected, so the file lands in the temp dir.
	if baselinePath() == "" {
		t.Error("baselinePath returned empty after withTempHome; the state directory " +
			"could not be resolved")
	} else if !containsDir(baselinePath(), dir) {
		t.Errorf("baseline file %s is outside the temp home %s; a test would write "+
			"into the developer's real state", baselinePath(), dir)
	}
	// The hook must not have fired as a side effect of the reset itself.
	if hookRan {
		t.Error("withTempHome fired the write-error hook it was supposed to clear")
	}
}

func TestBaselineWarnIsRateLimitedToOnceAnHour(t *testing.T) {
	// The warning has to be rate limited: a full disk would otherwise produce
	// one line per sample, forever, which is the noise the limiter exists to
	// prevent. Count the hook rather than parsing log output, so the test does
	// not depend on how the line is worded.
	var writes int
	baselineWarnHook = func() { writes++ }
	t.Cleanup(func() { baselineWarnHook = nil })

	// Drive the limiter's clock so the test does not depend on wall time: with
	// the real clock, the "an hour later" case would need an hour.
	//
	// The gaps are chosen so a wrong UNIT cannot pass. A 1-second gap still has
	// to be suppressed (a limit written in milliseconds would let it through),
	// and only a gap beyond the hour may warn.
	const hour = 3600
	nowUnix := int64(1_800_000_000)
	prevNow := baselineNowUnix
	baselineNowUnix = func() int64 { return nowUnix }
	t.Cleanup(func() { baselineNowUnix = prevNow })

	// Suppressed within the hour, including one second later: a limiter written
	// with the wrong unit (milliseconds, or seconds where hours were meant) lets
	// a one-second gap through, and this is what catches that.
	for _, gap := range []int64{0, 1, 60, hour - 1} {
		writes = 0
		baselineWarnLast.Store(nowUnix - gap)
		baselineWarnWrite(nil)
		if writes != 0 {
			t.Errorf("a warning %d second(s) after the last one was emitted again; "+
				"the limit is supposed to be one per hour", gap)
		}
	}
	// Beyond the hour it must warn, and only once per attempt.
	writes = 0
	baselineWarnLast.Store(nowUnix - hour - 1)
	baselineWarnWrite(nil)
	if writes != 1 {
		t.Errorf("a warning %d second(s) after the last one produced %d warnings, want 1",
			hour+1, writes)
	}
	// A failed write is the case that matters: it must still reach the operator
	// rather than being silently swallowed by the limiter.
	writes = 0
	baselineWarnLast.Store(nowUnix - hour - 1)
	baselineWarnWrite(errNoStateDir)
	if writes != 1 {
		t.Errorf("a failed write produced %d warning(s), want 1; a silently broken "+
			"recorder is worse than a loud one", writes)
	}
}

func containsDir(path, dir string) bool {
	return len(path) > len(dir) && path[:len(dir)] == dir
}
