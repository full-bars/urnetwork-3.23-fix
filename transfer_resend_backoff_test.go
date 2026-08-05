package connect

import (
	"testing"
	"time"
)

func TestResendBackoffGrowsWithSendCount(t *testing.T) {
	const maxResend = 8 * time.Second
	var prev time.Duration
	for sendCount := 2; sendCount <= 8; sendCount++ {
		got := resendBackoff(100*time.Millisecond, sendCount, maxResend)
		if got <= prev {
			t.Fatalf("sendCount=%d: backoff %v must exceed the previous %v", sendCount, got, prev)
		}
		prev = got
	}
	if got := resendBackoff(100*time.Millisecond, 1, maxResend); got != 100*time.Millisecond {
		t.Fatalf("first send must be plain ScaledRtt, got %v", got)
	}
}

func TestResendBackoffCapsAtMaxResendInterval(t *testing.T) {
	// Regression for the missing cap: the old fork formula (ScaledRtt <<
	// min(sendCount, 6), uncapped) yields 64s at sendCount=6 with a 1s RTT.
	const maxResend = 8 * time.Second
	if got := resendBackoff(time.Second, 6, maxResend); got != maxResend {
		t.Fatalf("backoff must cap at MaxResendInterval, got %v", got)
	}
	if got := resendBackoff(time.Second, 100, maxResend); got != maxResend {
		t.Fatalf("large sendCount must not overflow past the cap, got %v", got)
	}
	if got := resendBackoff(10*time.Second, 2, maxResend); got != maxResend {
		t.Fatalf("a ScaledRtt already above the cap must clamp, got %v", got)
	}
}

func TestResendBackoffFirstSendIsNotBackedOff(t *testing.T) {
	const maxResend = 8 * time.Second
	if got := resendBackoff(250*time.Millisecond, 1, maxResend); got != 250*time.Millisecond {
		t.Fatalf("sendCount=1 must yield a plain ScaledRtt, got %v", got)
	}
}

func TestResendBackoffAppliesWithoutDegradation(t *testing.T) {
	// Behavior change pinned: the old fork path returned a flat ScaledRtt()
	// whenever isBackendDegraded() was false, so a delayed-ack (not lost-ack)
	// congestion case never backed off. The backoff now applies on every
	// resend regardless of degradation state. Verified against the old code:
	// this case returned 100ms there, and the cap case returned 64s (uncapped).
	const maxResend = 8 * time.Second
	if got := resendBackoff(100*time.Millisecond, 3, maxResend); got != 400*time.Millisecond {
		t.Fatalf("third send must back off 2^2 x ScaledRtt without any degraded gate, got %v", got)
	}
}
