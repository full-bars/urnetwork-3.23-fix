package connect

import "time"

// auxCarrierGiveUp is the consecutive-failure park for auxiliary carriers:
// the H3 fallback set, the dns and dns-pump packet translators. It exists so
// that hundreds of identities cannot hold a handshake loop open for the life
// of the process: with a give-up window configured, a carrier that keeps
// failing parks for a bounded window and gets one probe per window after it.
//
// Disabled by default (AuxCarrierGiveUpAfter <= 0): failures are counted but
// nothing parks, and the established bounded backoff is the only brake.
//
// The transport's mode loop is the only caller, on its own goroutine, so the
// machine carries no lock.
type auxCarrierGiveUp struct {
	failures    int
	parkedUntil time.Time
}

// recordFailure counts one failed connect attempt, or one session that
// authenticated and then died before the healthy floor (a young session is
// not evidence the carrier works). It parks the carrier once the consecutive
// count reaches giveUpAfter and returns true when this failure parked it.
func (g *auxCarrierGiveUp) recordFailure(now time.Time, giveUpAfter int, parkWindow time.Duration) bool {
	if giveUpAfter <= 0 {
		return false
	}
	g.failures += 1
	if g.failures >= giveUpAfter && parkWindow > 0 {
		g.parkedUntil = now.Add(parkWindow)
		return true
	}
	return false
}

// recordHealthy clears the counter: a session that lived at least the healthy
// floor is evidence the carrier works.
func (g *auxCarrierGiveUp) recordHealthy() {
	g.failures = 0
	g.parkedUntil = time.Time{}
}

// clear resets everything: a network change, an operator gate change or a
// process restart invalidates the evidence a park was based on.
func (g *auxCarrierGiveUp) clear() {
	g.recordHealthy()
}

// waitUntil is the remaining park at now, zero when not parked (including
// parkedUntil already passed: the next attempt is then allowed unconditionally).
func (g *auxCarrierGiveUp) waitUntil(now time.Time) time.Duration {
	if g.parkedUntil.IsZero() || !now.Before(g.parkedUntil) {
		return 0
	}
	return g.parkedUntil.Sub(now)
}

// auxRetryWait picks the longer of the backoff wait and the park.
func auxRetryWait(backoff, park time.Duration) time.Duration {
	if park > backoff {
		return park
	}
	return backoff
}
