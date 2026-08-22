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

func TestResendBackoffExactDoublingTable(t *testing.T) {
	const scaledRtt = 100 * time.Millisecond
	const maxResend = 8 * time.Second
	cases := []struct {
		sendCount int
		want      time.Duration
	}{
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, 1600 * time.Millisecond},
		{6, 3200 * time.Millisecond},
		{7, 6400 * time.Millisecond},
		// shift would give 12.8s here, so it must clamp to the 8s cap.
		{8, maxResend},
	}
	for _, c := range cases {
		if got := resendBackoff(scaledRtt, c.sendCount, maxResend); got != c.want {
			t.Fatalf("sendCount=%d: got %v, want %v", c.sendCount, got, c.want)
		}
	}
}

func TestResendBackoffFirstSendIgnoresMaxResendInterval(t *testing.T) {
	// sendCount == 1 takes the early-return path and is never compared
	// against maxResendInterval, even when it exceeds the cap. Only once
	// backoff shifting begins (sendCount >= 2) does the cap apply.
	const maxResend = 1 * time.Second
	if got := resendBackoff(10*time.Second, 1, maxResend); got != 10*time.Second {
		t.Fatalf("sendCount=1 must bypass the MaxResendInterval cap entirely, got %v", got)
	}
	// As soon as a single resend has occurred, the same RTT is clamped.
	if got := resendBackoff(10*time.Second, 2, maxResend); got != maxResend {
		t.Fatalf("sendCount=2 must clamp to MaxResendInterval, got %v", got)
	}
}

func TestResendBackoffZeroScaledRtt(t *testing.T) {
	const maxResend = 8 * time.Second
	for _, sendCount := range []int{1, 2, 5, 20} {
		if got := resendBackoff(0, sendCount, maxResend); got != 0 {
			t.Fatalf("sendCount=%d: a zero ScaledRtt must stay zero regardless of shift, got %v", sendCount, got)
		}
	}
}

func TestResendBackoffZeroMaxResendInterval(t *testing.T) {
	// A zero cap still lets the unshifted first send through untouched,
	// but clamps every backed-off resend down to zero.
	if got := resendBackoff(100*time.Millisecond, 1, 0); got != 100*time.Millisecond {
		t.Fatalf("sendCount=1 with a zero cap must still return the plain ScaledRtt, got %v", got)
	}
	if got := resendBackoff(100*time.Millisecond, 2, 0); got != 0 {
		t.Fatalf("sendCount=2 with a zero cap must clamp to zero, got %v", got)
	}
}

func TestResendBackoffShiftSaturatesAtSixteen(t *testing.T) {
	// The internal shift is capped at 16 independently of maxResendInterval.
	// Use a cap large enough that it never binds, so any growth past
	// sendCount=17 would indicate the internal shift cap was not applied.
	const scaledRtt = 1 * time.Millisecond
	const maxResend = 365 * 24 * time.Hour
	want := scaledRtt << 16
	for _, sendCount := range []int{17, 18, 100, 1000} {
		if got := resendBackoff(scaledRtt, sendCount, maxResend); got != want {
			t.Fatalf("sendCount=%d: shift must saturate at 16 (want %v), got %v", sendCount, want, got)
		}
	}
}

func TestResendBackoffNonPositiveSendCount(t *testing.T) {
	// sendCount <= 0 is not a value production code ever passes (sendCount
	// starts at 1 and is only ever incremented), but resendBackoff must not
	// panic on it. sendCount-1 goes negative, and converting that negative
	// int to the shift's uint wraps to a huge value, which Go's shift
	// operator saturates to zero rather than overflowing or panicking.
	const maxResend = 8 * time.Second
	for _, sendCount := range []int{0, -1, -5} {
		if got := resendBackoff(time.Second, sendCount, maxResend); got != 0 {
			t.Fatalf("sendCount=%d: expected the wrapped shift to saturate to 0, got %v", sendCount, got)
		}
	}
}
