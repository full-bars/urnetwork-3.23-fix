package main

import (
	"context"
	"errors"
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

	mu                     sync.Mutex
	min                    rate.Limit
	max                    rate.Limit
	successStreak          int
	unprovenOverloadStreak int
	lastAdjustedAt         time.Time
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
	// authUnprovenOverloadThreshold is how many consecutive overload-looking
	// errors through never-proven proxies — with no success of any kind
	// landing in between — are required before treating them as a real
	// signal about the API rather than noise from individually broken
	// proxies. One bad proxy in a list of hundreds shouldn't cut the rate;
	// dozens of unrelated proxies all failing back-to-back with nothing
	// succeeding is what an actual outage looks like, even before any of
	// them has individually proven itself. This is what keeps a mass
	// timeout-based outage at cold start from being entirely invisible to
	// the limiter just because no proxy has had the chance to prove itself
	// yet.
	authUnprovenOverloadThreshold = 8
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
	if err == nil {
		a.recordSuccessAndMaybeIncrease()
		return
	}
	if isRateLimitedError(err) {
		a.decrease("429 received")
		return
	}
	if isOverloadError(err) {
		// A timeout/connection failure means the API is struggling under
		// the current load just as much as an explicit 429 — back off
		// either way. Other errors (e.g. a bad JWT) say nothing about
		// request rate, so leave the limiter where it is rather than
		// treating them as progress.
		a.decrease("timeout/connection error")
	}
}

// ReportResultForProxy is like ReportResult, but for a result that came
// through a specific proxy hop. A bare timeout/connection error through a
// proxy that has never once succeeded is just as likely to be that proxy's
// own broken SOCKS5 service — undetectable by the TCP-only reachability
// probe in proxy_probe.go, which only confirms the port accepts a
// connection — as it is real API backpressure. A single such error is
// treated as no signal at all (same as the reachability-probe bypass)
// instead of cutting the rate, so a list full of "port open, proxy broken"
// entries can't throttle every other proxy the way a list full of dead
// ports used to.
//
// But a long unbroken run of them (authUnprovenOverloadThreshold, with no
// success of any kind landing in between) IS treated as real signal — a
// mass outage doesn't wait for proxies to individually prove themselves
// first, and without this, a timeout-based outage hitting entirely
// never-proven proxies (the common case right at cold start or just after a
// big batch of new proxies is added) would otherwise be completely invisible
// to the limiter.
//
// An explicit 429 is unambiguous regardless of the proxy's track record —
// the API said so itself — so it always counts immediately.
func (a *authRateLimiter) ReportResultForProxy(err error, proxyHasSucceeded bool) {
	if err == nil {
		a.clearUnprovenOverloadStreak()
		a.recordSuccessAndMaybeIncrease()
		return
	}
	if isRateLimitedError(err) {
		a.clearUnprovenOverloadStreak()
		a.decrease("429 received")
		return
	}
	if !isOverloadError(err) {
		return
	}
	if proxyHasSucceeded {
		a.clearUnprovenOverloadStreak()
		a.decrease("timeout/connection error via a proven proxy")
		return
	}
	if a.bumpUnprovenOverloadStreak() {
		a.decrease(fmt.Sprintf("%d unproven timeouts in a row with no success in between", authUnprovenOverloadThreshold))
	}
}

func (a *authRateLimiter) clearUnprovenOverloadStreak() {
	a.mu.Lock()
	a.unprovenOverloadStreak = 0
	a.mu.Unlock()
}

// bumpUnprovenOverloadStreak increments the streak of overload-looking
// errors from never-proven proxies, uninterrupted by any success or any
// proven-proxy failure, and reports whether it just crossed
// authUnprovenOverloadThreshold — the point where it stops looking like
// scattered broken-proxy noise and starts looking like a real outage.
func (a *authRateLimiter) bumpUnprovenOverloadStreak() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.unprovenOverloadStreak++
	if a.unprovenOverloadStreak < authUnprovenOverloadThreshold {
		return false
	}
	a.unprovenOverloadStreak = 0
	return true
}

// decrease cuts the rate. reason is purely for the log line, so operators
// can tell a genuine 429 apart from a proven-proxy timeout or a mass
// unproven-timeout trip instead of every cut being misreported as "429
// received" regardless of what actually triggered it.
func (a *authRateLimiter) decrease(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.successStreak = 0
	if !a.lastAdjustedAt.IsZero() && time.Since(a.lastAdjustedAt) < authRateAdjustCooldown {
		// Already cut very recently — this result was likely in flight
		// before that cut took effect. Let it land before cutting again.
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
	fmt.Printf("[proxy][authrate] %s — cutting auth rate %.2f -> %.2f req/s\n", reason, float64(oldRate), float64(newRate))
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

// isOverloadError reports whether err looks like a network/timeout failure
// rather than a rejected token. The API doesn't always answer with an
// explicit 429 under load — it can just stop responding in time — and that
// looks identical to a rate-limit problem from the caller's perspective, so
// it should drive the same backoff. Mirrors the cause classification in the
// auth retry loop in main.go.
func isOverloadError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	errMsg := err.Error()
	return strings.Contains(errMsg, "Timeout") ||
		strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "deadline exceeded") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "no such host")
}
