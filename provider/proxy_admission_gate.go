package main

import (
	"context"
	"math/rand"
	"sync"
)

// proxyAdmissionGate sits in front of the shared globalAuthRateLimiter and
// decides, among every proxy currently waiting for an auth slot, which one
// gets the next token. Without this, every proxy's retry competes for the
// same scarce budget with identical priority — a proxy on its 9th retry
// (already very likely dead) gets exactly the same chance at the next slot
// as a proxy that's never been tried, which wastes most of the budget
// re-poking probably-dead proxies instead of discovering whether a fresh
// one works.
//
// Priority is a weighted lottery, not a strict ordering: every waiter gets
// ticket weight 1/(failures+1), so untried proxies are heavily favored but a
// proxy with a long failure history is never fully excluded. A strict
// "untried first" rule would starve retries entirely once the candidate
// pool is large enough to keep refilling itself — which it always is here,
// via the 15-minute URL-source requeue constantly reintroducing candidates.
// proxyAdmissionMaxConcurrency bounds how many admitted auth attempts can be
// in flight at once, independent of the rate limiter's start rate. The rate
// limiter only throttles how fast new attempts are *released* — it doesn't
// know how long a released attempt takes to finish. A SOCKS5 proxy that
// hangs for several seconds before timing out, released at even a modest
// rate, can pile up far more simultaneous in-flight connections to the API
// than the configured rate would suggest. This semaphore caps that
// regardless of how slow individual attempts are.
const proxyAdmissionMaxConcurrency = 5

type proxyAdmissionGate struct {
	limiter     *authRateLimiter
	concurrency chan struct{}

	mu      sync.Mutex
	waiters []*admissionWaiter
	wake    chan struct{}
}

type admissionWaiter struct {
	weight float64
	ready  chan struct{}
}

func newProxyAdmissionGate(limiter *authRateLimiter) *proxyAdmissionGate {
	g := &proxyAdmissionGate{
		limiter:     limiter,
		wake:        make(chan struct{}, 1),
		concurrency: make(chan struct{}, proxyAdmissionMaxConcurrency),
	}
	go g.dispatchLoop()
	return g
}

var globalProxyAdmissionGate = newProxyAdmissionGate(globalAuthRateLimiter)

// Admit blocks until the caller is granted the next auth-rate-limiter slot,
// or ctx is done. failureCount is the calling proxy's lifetime failure count
// (see proxyFailureHistory) — lower means higher priority in the lottery.
//
// On success, Admit also returns a release func that the caller MUST call
// (typically via defer) once its auth attempt finishes, success or failure.
// This is what enforces proxyAdmissionMaxConcurrency: the in-flight slot
// reserved by dispatchLoop for this waiter isn't freed until release() runs.
func (g *proxyAdmissionGate) Admit(ctx context.Context, failureCount int) (release func(), err error) {
	w := &admissionWaiter{
		weight: 1.0 / float64(failureCount+1),
		ready:  make(chan struct{}),
	}
	g.push(w)

	select {
	case <-w.ready:
		return func() { <-g.concurrency }, nil
	case <-ctx.Done():
		g.remove(w)
		select {
		case <-w.ready:
			// ctx was cancelled after the dispatch loop selected this
			// waiter and claimed a concurrency slot for it — release
			// the slot now so it isn't permanently leaked.
			<-g.concurrency
		default:
		}
		return nil, ctx.Err()
	}
}

func (g *proxyAdmissionGate) push(w *admissionWaiter) {
	g.mu.Lock()
	g.waiters = append(g.waiters, w)
	wasEmpty := len(g.waiters) == 1
	g.mu.Unlock()

	if wasEmpty {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
}

func (g *proxyAdmissionGate) remove(w *admissionWaiter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, x := range g.waiters {
		if x == w {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			return
		}
	}
}

func (g *proxyAdmissionGate) dispatchLoop() {
	for {
		g.waitForWaiter()

		// Block here until an in-flight slot is free, before even
		// consulting the rate limiter, so the rate limiter's start rate is
		// never used to release more concurrent attempts than this allows.
		g.concurrency <- struct{}{}

		if err := g.limiter.Wait(context.Background()); err != nil {
			<-g.concurrency
			return
		}
		if !g.releaseWeightedRandom() {
			// The waiter that justified entering this iteration was
			// removed out from under us (e.g. its ctx was canceled) before
			// we got to release anyone. Nobody is holding this slot — give
			// it back instead of leaking it forever.
			<-g.concurrency
		}
	}
}

func (g *proxyAdmissionGate) waitForWaiter() {
	for {
		g.mu.Lock()
		n := len(g.waiters)
		g.mu.Unlock()
		if n > 0 {
			return
		}
		<-g.wake
	}
}

// releaseWeightedRandom picks one currently-queued waiter via weighted
// random selection (weighted by ticket value) and releases it. Random
// rather than strict top-weight selection so a long-shot retry still has a
// real, if small, chance to land instead of being deterministically starved
// out by a steady stream of higher-weight untried proxies. Returns false if
// there was no waiter left to release.
func (g *proxyAdmissionGate) releaseWeightedRandom() bool {
	g.mu.Lock()
	if len(g.waiters) == 0 {
		g.mu.Unlock()
		return false
	}

	total := 0.0
	for _, w := range g.waiters {
		total += w.weight
	}

	r := rand.Float64() * total
	chosen := 0
	cum := 0.0
	for i, w := range g.waiters {
		cum += w.weight
		if r <= cum {
			chosen = i
			break
		}
	}

	w := g.waiters[chosen]
	g.waiters = append(g.waiters[:chosen:chosen], g.waiters[chosen+1:]...)
	g.mu.Unlock()

	close(w.ready)
	return true
}
