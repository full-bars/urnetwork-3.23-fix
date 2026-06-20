package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestAuthRateLimiter_DecreasesOn429 is a regression test for the live
// deployment problem this limiter exists to solve: per-proxy backoff alone
// barely dents the aggregate request rate when hundreds of proxies retry
// independently. A 429 must cut the shared rate so every proxy slows down
// together, not just the one that happened to get rate-limited.
func TestAuthRateLimiter_DecreasesOn429(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 15)
	l.lastAdjustedAt = time.Now().Add(-authRateAdjustCooldown - time.Second)

	l.ReportResult(errors.New("429 Too Many Requests: <html error page, 162 bytes>"))

	if got := l.CurrentRate(); got != 5 {
		t.Fatalf("expected rate to halve from 10 to 5, got %v", got)
	}
}

// TestAuthRateLimiter_FloorsAtMin ensures repeated 429s don't drive the rate
// below the configured floor, which would stall startup entirely.
func TestAuthRateLimiter_FloorsAtMin(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 15)

	for i := 0; i < 10; i++ {
		l.lastAdjustedAt = time.Now().Add(-authRateAdjustCooldown - time.Second)
		l.ReportResult(errors.New("429 Too Many Requests"))
	}

	if got := l.CurrentRate(); got != 1 {
		t.Fatalf("expected rate to floor at 1, got %v", got)
	}
}

// TestAuthRateLimiter_DecreaseRespectsCooldown is a regression test for a
// thundering-herd scenario: a burst of 429s that were all already in flight
// before the first cut takes effect must not each trigger their own
// additional cut, or the rate collapses to the floor in one instant instead
// of reacting proportionally to sustained congestion.
func TestAuthRateLimiter_DecreaseRespectsCooldown(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 15)
	l.lastAdjustedAt = time.Now().Add(-authRateAdjustCooldown - time.Second)

	l.ReportResult(errors.New("429 Too Many Requests"))
	rateAfterFirst := l.CurrentRate()

	// These land inside the cooldown window and must be ignored.
	l.ReportResult(errors.New("429 Too Many Requests"))
	l.ReportResult(errors.New("429 Too Many Requests"))

	if got := l.CurrentRate(); got != rateAfterFirst {
		t.Fatalf("expected rate to stay at %v during cooldown, got %v", rateAfterFirst, got)
	}
}

// TestAuthRateLimiter_IncreasesAfterSustainedSuccess is a regression test for
// the "don't take 12 hours to start 1000 proxies" requirement: once the rate
// has been cut, a long enough run of clean (non-429) results must creep it
// back up rather than leaving it permanently throttled after one bad patch.
func TestAuthRateLimiter_IncreasesAfterSustainedSuccess(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 15)
	l.lastAdjustedAt = time.Now().Add(-authRateAdjustCooldown - time.Second)
	l.ReportResult(errors.New("429 Too Many Requests"))
	if got := l.CurrentRate(); got != 5 {
		t.Fatalf("expected rate to halve to 5, got %v", got)
	}

	l.lastAdjustedAt = time.Now().Add(-authRateAdjustCooldown - time.Second)
	for i := 0; i < authRateIncreaseThreshold; i++ {
		l.ReportResult(nil)
	}

	if got := l.CurrentRate(); got != 6 {
		t.Fatalf("expected rate to rise from 5 to 6 after %d clean results, got %v", authRateIncreaseThreshold, got)
	}
}

// TestAuthRateLimiter_IncreaseNeverExceedsMax ensures the additive increase
// never pushes the rate past the configured ceiling.
func TestAuthRateLimiter_IncreaseNeverExceedsMax(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 15)

	for round := 0; round < 5; round++ {
		l.lastAdjustedAt = time.Now().Add(-authRateAdjustCooldown - time.Second)
		for i := 0; i < authRateIncreaseThreshold; i++ {
			l.ReportResult(nil)
		}
	}

	if got := l.CurrentRate(); got != 10 {
		t.Fatalf("expected rate to cap at max 10, got %v", got)
	}
}

// TestAuthRateLimiter_NonRateLimitErrorDoesNotCount ensures an ordinary
// failure (timeout, etc.) counts toward the success streak the same as an
// actual success — only 429s are the congestion signal this limiter reacts
// to, not auth/network failures unrelated to request volume.
func TestAuthRateLimiter_NonRateLimitErrorDoesNotTriggerDecrease(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 15)
	before := l.CurrentRate()

	l.ReportResult(errors.New("Timeout."))

	if got := l.CurrentRate(); got != before {
		t.Fatalf("expected non-429 error not to change the rate, got %v want %v", got, before)
	}
}

// TestAuthRateLimiter_Wait_AllowsBurstThenThrottles confirms the limiter lets
// an initial batch up to the burst size through immediately (so starting a
// large proxy list doesn't serialize one at a time), then paces further
// requests at the configured rate.
func TestAuthRateLimiter_Wait_AllowsBurstThenThrottles(t *testing.T) {
	l := newAuthRateLimiter(1, 10, 3)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(ctx); err != nil {
			t.Fatalf("unexpected error on burst request %d: %v", i, err)
		}
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("expected the first %d requests (within burst) to proceed immediately, took %v", 3, elapsed)
	}

	// The 4th request exceeds burst and must wait for the rate to refill.
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("expected the request beyond burst to be throttled, took only %v", elapsed)
	}
}

func TestIsRateLimitedError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{errors.New("429 Too Many Requests: <html error page, 162 bytes>"), true},
		{errors.New("Too Many Requests"), true},
		{errors.New("Timeout."), false},
		{errors.New("connection refused"), false},
	}
	for _, c := range cases {
		if got := isRateLimitedError(c.err); got != c.want {
			t.Errorf("isRateLimitedError(%q) = %v, want %v", c.err, got, c.want)
		}
	}
}

var _ = rate.Limit(0) // keep golang.org/x/time/rate import path explicit for reviewers
