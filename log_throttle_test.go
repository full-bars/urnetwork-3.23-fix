package connect

import (
	"testing"
	"time"
)

// logThrottle unifies the four copy-pasted shouldLogX rate-limiters
// (auth/select/write/oob err). It emits at most one line per interval and
// counts how many were suppressed in between, for a "(N suppressed)" tail.

func TestLogThrottle_FirstCallAllowedZeroSuppressed(t *testing.T) {
	th := newLogThrottle(time.Minute)
	ok, suppressed := th.Allow(time.Unix(100, 0))
	if !ok {
		t.Fatal("first call should be allowed")
	}
	if suppressed != 0 {
		t.Fatalf("first call should report 0 suppressed, got %d", suppressed)
	}
}

func TestLogThrottle_SuppressesWithinIntervalThenReportsCount(t *testing.T) {
	th := newLogThrottle(time.Minute)
	base := time.Unix(1000, 0)

	if ok, _ := th.Allow(base); !ok {
		t.Fatal("first call should be allowed")
	}
	for i := 1; i <= 3; i++ {
		if ok, _ := th.Allow(base.Add(time.Duration(i) * time.Second)); ok {
			t.Fatalf("call %ds in (within interval) should be suppressed", i)
		}
	}

	ok, suppressed := th.Allow(base.Add(2 * time.Minute))
	if !ok {
		t.Fatal("call after the interval elapsed should be allowed")
	}
	if suppressed != 3 {
		t.Fatalf("expected 3 suppressed since the last allowed line, got %d", suppressed)
	}
}

func TestLogThrottle_SuppressedCountResetsAfterEmit(t *testing.T) {
	th := newLogThrottle(time.Minute)
	base := time.Unix(1000, 0)

	th.Allow(base)                      // allowed
	th.Allow(base.Add(1 * time.Second)) // suppressed -> count 1
	th.Allow(base.Add(2 * time.Minute)) // allowed, reports & resets count

	// A subsequent allowed line with no suppression in between reports 0.
	ok, suppressed := th.Allow(base.Add(4 * time.Minute))
	if !ok || suppressed != 0 {
		t.Fatalf("expected allowed with 0 suppressed, got ok=%v suppressed=%d", ok, suppressed)
	}
}
