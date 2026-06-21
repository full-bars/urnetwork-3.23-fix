package main

import (
	"context"
	"testing"
	"time"
)

// TestProxyAdmissionGate_ReleaseWeightedRandomFavorsLowerFailureCount is a
// regression test for the core scheduling property: among waiters competing
// for the same slot, the lottery must strongly favor a lower failure count
// (closer to "untried") without ever fully excluding a higher one — a
// strict "untried always wins" rule would starve retries entirely once the
// candidate pool is large enough to keep refilling itself, which it always
// is here via the 15-minute URL-source requeue.
func TestProxyAdmissionGate_ReleaseWeightedRandomFavorsLowerFailureCount(t *testing.T) {
	g := &proxyAdmissionGate{wake: make(chan struct{}, 1)}

	const trials = 2000
	lowWins := 0

	for i := 0; i < trials; i++ {
		low := &admissionWaiter{weight: 1.0 / float64(0+1), ready: make(chan struct{})}  // untried
		high := &admissionWaiter{weight: 1.0 / float64(10+1), ready: make(chan struct{})} // 10 prior failures
		g.waiters = []*admissionWaiter{low, high}

		g.releaseWeightedRandom()

		select {
		case <-low.ready:
			lowWins++
		default:
		}
	}

	// Expected win rate for the untried waiter is 11/12 ≈ 91.7%. Assert a
	// strong majority without pinning the exact ratio (this is a random
	// process), and assert the higher-failure waiter still wins sometimes so
	// the test would catch a regression to strict (non-random) priority.
	if lowWins < trials*70/100 {
		t.Fatalf("expected the untried waiter to win a strong majority of trials, got %d/%d", lowWins, trials)
	}
	if lowWins >= trials {
		t.Fatalf("expected the higher-failure waiter to win at least sometimes (no starvation), got %d/%d for the untried waiter", lowWins, trials)
	}
}

// TestProxyAdmissionGate_AdmitGrantsAccess confirms the end-to-end path
// (push -> dispatch loop -> rate limiter -> weighted release) actually
// unblocks a caller under normal conditions.
func TestProxyAdmissionGate_AdmitGrantsAccess(t *testing.T) {
	limiter := newAuthRateLimiter(1000, 1000, 1000)
	g := newProxyAdmissionGate(limiter)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	release, err := g.Admit(ctx, 0)
	if err != nil {
		t.Fatalf("expected Admit to succeed, got %v", err)
	}
	release()
}

// TestProxyAdmissionGate_AdmitRespectsCancellation confirms a caller that
// gives up (e.g. its proxy's context is canceled) doesn't hang forever or
// leak into the waiter list.
func TestProxyAdmissionGate_AdmitRespectsCancellation(t *testing.T) {
	limiter := newAuthRateLimiter(0.001, 0.001, 1)
	g := newProxyAdmissionGate(limiter)

	// Consume the only burst token so the next Admit has to actually wait.
	release, err := g.Admit(context.Background(), 0)
	if err != nil {
		t.Fatalf("expected first Admit (within burst) to succeed, got %v", err)
	}
	release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := g.Admit(ctx, 0); err == nil {
		t.Fatal("expected the second Admit to be blocked by the near-zero rate and hit the context deadline")
	}

	g.mu.Lock()
	leaked := len(g.waiters)
	g.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("expected the canceled waiter to be removed from the queue, got %d still queued", leaked)
	}
}

// TestProxyAdmissionGate_BoundsInFlightConcurrency is a regression test for
// a bug found during fleet deployment analysis: the rate limiter only
// throttled how fast new attempts were released, not how many were
// simultaneously in flight. A slow caller that never calls its release()
// func (simulating a proxy whose auth attempt is hanging) must block
// admission of new callers past proxyAdmissionMaxConcurrency, even though
// the rate limiter itself has plenty of budget to keep admitting more.
func TestProxyAdmissionGate_BoundsInFlightConcurrency(t *testing.T) {
	limiter := newAuthRateLimiter(1000, 1000, 1000) // effectively unthrottled
	g := newProxyAdmissionGate(limiter)

	var releases []func()
	for i := 0; i < proxyAdmissionMaxConcurrency; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := g.Admit(ctx, 0)
		if err != nil {
			t.Fatalf("expected Admit %d (within concurrency cap) to succeed, got %v", i, err)
		}
		releases = append(releases, release)
	}

	// The cap is now fully held by callers that haven't released yet. One
	// more Admit must block despite the rate limiter having unlimited
	// budget.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := g.Admit(ctx, 0); err == nil {
		t.Fatal("expected Admit beyond the concurrency cap to block until a slot is released")
	}

	// Releasing one slot must unblock a subsequent Admit.
	releases[0]()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	release, err := g.Admit(ctx2, 0)
	if err != nil {
		t.Fatalf("expected Admit to succeed after a slot was released, got %v", err)
	}
	release()

	for _, release := range releases[1:] {
		release()
	}
}
