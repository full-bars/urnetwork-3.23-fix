package connect

import (
	"testing"
	"time"
)

// waitForPoolBalance waits until the global message-pool counters for `size`
// settle at a stable taken==returned balance, then returns the totals.
//
// The pool counters are process-global, and other tests' transport goroutines
// can legitimately return buffers AFTER their owning test has finished (the
// buffer was taken before that test's reset, so its late Return lands in a
// zeroed counter and transiently makes returned > taken). An assertion read
// immediately after a test's own flow therefore sees stragglers and reports
// a phantom "leak" — this is what made the leak-contract tests flaky once
// debugTags (on by default) stamped real tags on every allocation.
//
// Polling until two consecutive snapshots are identical bounds the wait: in a
// static test binary all prior goroutines finish quickly, so after a brief
// quiet window the balance is exact. Only a REAL leak — a buffer this test's
// flow took and never returned — keeps appearing as returned < taken across
// repeated snapshots. The 2s cap turns a wedged transport goroutine into a
// test failure with a clear message instead of an infinite hang.
func waitForPoolBalance(t *testing.T, size int) (taken, returned uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var prevTaken, prevReturned uint64
	stable := 0
	for {
		curTaken, curReturned := MessagePoolTotals(size)
		if curTaken == prevTaken && curReturned == prevReturned {
			stable++
		} else {
			stable = 0
		}
		if stable >= 2 {
			return curTaken, curReturned
		}
		prevTaken, prevReturned = curTaken, curReturned
		if time.Now().After(deadline) {
			t.Fatalf("pool for size %d did not reach a stable balance within 2s (taken=%d returned=%d); a goroutine from another test is still returning buffers", size, curTaken, curReturned)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
