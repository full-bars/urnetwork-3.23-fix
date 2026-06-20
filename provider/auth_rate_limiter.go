package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// authRateLimiter gates every outbound auth attempt (first tries and
// retries alike) behind a single shared rate, instead of relying on each
// proxy to back off independently. With hundreds of proxies starting
// concurrently, per-proxy backoff barely dents the aggregate request rate
// hitting the API — the proxies in backoff slow down, but all the others
// keep hammering it at full speed.
//
// The rate adapts itself (AIMD, the same idea TCP congestion control uses):
// every 429 halves it, and a sustained run of non-429 results creeps it back
// up. That finds the API's actual safe throughput instead of relying on a
// hardcoded guess, and starts aggressive (at the believed ceiling) rather
// than slow-ramping from zero, since a cold start of hundreds of proxies
// shouldn't take hours just because the steady-state rate is conservative.
type authRateLimiter struct {
	limiter *rate.Limiter

	mu             sync.Mutex
	min            rate.Limit
	max            rate.Limit
	successStreak  int
	lastAdjustedAt time.Time
}

const (
	// authRateIncreaseThreshold is the number of consecutive non-429 results
	// required before creeping the rate up.
	authRateIncreaseThreshold = 20
	// authRateIncreaseStep is how much the rate grows per increase.
	authRateIncreaseStep = rate.Limit(1.0)
	// authRateDecreaseFactor is the multiplicative cut applied on a 429.
	authRateDecreaseFactor = 0.5
	// authRateAdjustCooldown is the minimum time between any two
	// adjustments, so a burst of 429s already in flight before a cut takes
	// effect doesn't keep cutting the rate further on every one of them.
	authRateAdjustCooldown = 2 * time.Second
)

// globalAuthRateLimiter is shared by every proxy goroutine in the process —
// they're all authenticating against the same API, so the limit has to be
// process-wide to mean anything.
var globalAuthRateLimiter = newAuthRateLimiter(1, 10, 15)

func newAuthRateLimiter(min, max rate.Limit, burst int) *authRateLimiter {
	return &authRateLimiter{
		limiter: rate.NewLimiter(max, burst),
		min:     min,
		max:     max,
	}
}

// Wait blocks until the next auth attempt is allowed to proceed, or until ctx
// is done.
func (a *authRateLimiter) Wait(ctx context.Context) error {
	return a.limiter.Wait(ctx)
}

// ReportResult feeds the outcome of an auth attempt back into the limiter so
// it can adapt. Call this after every attempt, success or failure.
func (a *authRateLimiter) ReportResult(err error) {
	if err != nil && isRateLimitedError(err) {
		a.decrease()
		return
	}
	a.recordSuccessAndMaybeIncrease()
}

func (a *authRateLimiter) decrease() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.successStreak = 0
	if !a.lastAdjustedAt.IsZero() && time.Since(a.lastAdjustedAt) < authRateAdjustCooldown {
		// Already cut very recently — this 429 was likely in flight before
		// that cut took effect. Let it land before cutting again.
		return
	}

	oldRate := a.limiter.Limit()
	newRate := rate.Limit(float64(oldRate) * authRateDecreaseFactor)
	if newRate < a.min {
		newRate = a.min
	}
	if newRate == oldRate {
		return
	}
	a.limiter.SetLimit(newRate)
	a.lastAdjustedAt = time.Now()
	fmt.Printf("[proxy][authrate] 429 received — cutting auth rate %.2f -> %.2f req/s\n", float64(oldRate), float64(newRate))
}

func (a *authRateLimiter) recordSuccessAndMaybeIncrease() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.successStreak++
	if a.successStreak < authRateIncreaseThreshold {
		return
	}
	a.successStreak = 0
	if !a.lastAdjustedAt.IsZero() && time.Since(a.lastAdjustedAt) < authRateAdjustCooldown {
		return
	}

	oldRate := a.limiter.Limit()
	newRate := oldRate + authRateIncreaseStep
	if newRate > a.max {
		newRate = a.max
	}
	if newRate == oldRate {
		return
	}
	a.limiter.SetLimit(newRate)
	a.lastAdjustedAt = time.Now()
	fmt.Printf("[proxy][authrate] %d clean attempts — raising auth rate %.2f -> %.2f req/s\n", authRateIncreaseThreshold, float64(oldRate), float64(newRate))
}

// CurrentRate reports the limiter's current requests/sec, for logging and
// tests.
func (a *authRateLimiter) CurrentRate() float64 {
	return float64(a.limiter.Limit())
}

func isRateLimitedError(err error) bool {
	return strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "Too Many Requests")
}
