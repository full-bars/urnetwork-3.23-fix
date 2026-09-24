package main

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy liveness counters, maintained solely to populate systemd's STATUS=
// line. They are deliberately not a source of truth for anything else: no
// control-flow decision reads them, so a transient miscount degrades an
// operator-facing string and nothing more.
//
// These exist because READY=1 no longer carries health (see
// notifySystemdReady). Splitting the two is what lets the unit reach "active"
// promptly while still telling an operator, honestly and continuously,
// whether any traffic can actually flow.
var (
	proxiesConfigured    atomic.Int64
	proxiesAuthenticated atomic.Int64

	// proxiesParked is how many configured proxies the proxy audit engine is
	// deliberately holding out, and proxyAuditPaused is whether it is unable to
	// act. Parked proxies are configured but not authenticated on purpose, so
	// they must not read as an outage in the line.
	proxiesParked    atomic.Int64
	proxyAuditPaused atomic.Bool
)

// proxyResolutionStatus tracks whether proxy resolution has been attempted
// and the outcome. This lets systemdStatusLine distinguish "never tried"
// (startup) from "tried and broke" from "tried and got nothing" when
// total==0.
var (
	proxyResolutionStatus atomic.Int32
	proxyResolutionReason atomic.Value // string — last failure/rejection reason
)

// Resolution status constants. The value matters only inside
// systemdStatusLine's total==0 branch; no control-flow reads these.
const (
	proxyResolutionPending int32 = 0 // no resolution attempted yet
	proxyResolutionFailed  int32 = 1 // attempted, source unreachable
	proxyResolutionEmpty   int32 = 2 // succeeded but zero usable proxies
	proxyResolutionOK      int32 = 3 // resolved with at least one proxy
)

// Status severity bands for the live/configured proxy ratio. The exact
// percentage always renders alongside the word so the scale reads
// continuously; a node with 40% live and one with 9% live are both
// "critical", and the number says how bad it is.
const (
	statusActiveBand   = 90 // >= this percent of configured proxies live: healthy steady state
	statusPartialBand  = 70 // 70-89%: first real attention
	statusDegradedBand = 50 // 50-69%: significant loss; below 50% is critical
)

// setProxyResolutionStatus records the outcome of a proxy resolution attempt
// and refreshes the systemd STATUS= line.
func setProxyResolutionStatus(status int32, reason string) {
	proxyResolutionStatus.Store(status)
	if reason != "" {
		proxyResolutionReason.Store(reason)
	}
	reportProxyStatusToSystemd()
}

// setProxyResolutionOK marks resolution as successful (called when proxies
// are found). Clears any stale failure/rejection state.
func setProxyResolutionOK() {
	proxyResolutionStatus.Store(proxyResolutionOK)
	reportProxyStatusToSystemd()
}

// getProxyResolutionReason returns the stored reason string.
func getProxyResolutionReason() string {
	v := proxyResolutionReason.Load()
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// maxStatusReasonLen caps the failure reason embedded in the STATUS= line so
// the total stays well under systemd's practical limit.
const maxStatusReasonLen = 60

// setConfiguredProxyCount records how many proxies this provider will attempt,
// known once the proxy list is resolved.
func setConfiguredProxyCount(n int) {
	proxiesConfigured.Store(int64(n))
	reportProxyStatusToSystemd()
}

// setProxyAuditSystemdState records the proxy audit's parked count and
// whether it is paused, and refreshes STATUS= only when either changed (the
// proxy audit calls this every tick).
func setProxyAuditSystemdState(parked int, paused bool) {
	if parked < 0 {
		parked = 0
	}
	changed := proxiesParked.Swap(int64(parked)) != int64(parked)
	if proxyAuditPaused.Swap(paused) != paused {
		changed = true
	}
	if changed {
		reportProxyStatusToSystemd()
	}
}

// proxyBecameLive/proxyWentDown bracket a proxy's live transport. Both report
// immediately so `systemctl status` tracks reality rather than lagging until
// the next event.
func proxyBecameLive() {
	proxiesAuthenticated.Add(1)
	reportProxyStatusToSystemd()
}

func proxyWentDown() {
	if proxiesAuthenticated.Add(-1) < 0 {
		// Clamp rather than let an unbalanced pair drive the count negative
		// and render as nonsense in systemctl status.
		proxiesAuthenticated.Store(0)
	}
	reportProxyStatusToSystemd()
}

// systemdStatusLine renders the current state. Split out from the send so it
// can be asserted directly in tests without a NOTIFY_SOCKET.
func systemdStatusLine() string {
	live := proxiesAuthenticated.Load()
	total := proxiesConfigured.Load()

	switch {
	case total == 0:
		switch proxyResolutionStatus.Load() {
		case proxyResolutionFailed:
			reason := getProxyResolutionReason()
			if reason == "" {
				reason = "unknown error"
			}
			if len(reason) > maxStatusReasonLen {
				reason = reason[:maxStatusReasonLen] + "..."
			}
			return fmt.Sprintf("degraded: proxy source unreachable (%s), retrying", reason)
		case proxyResolutionEmpty:
			return "degraded: proxy source returned no usable proxies, retrying"
		default: // proxyResolutionPending or stale OK
			return "starting: resolving proxies"
		}
	default:
		// Parks and pauses are deliberate, not outages: the word is chosen
		// against the proxies that SHOULD be live (configured minus parked),
		// while the rendered percentage still reflects the configured set.
		// The band compares on the true ratio, not the rounded percentage, so
		// an 89.5% node reads partial, not an accidental active.
		parked := proxiesParked.Load()
		if parked > total {
			parked = total
		}
		eff := total - parked
		ratio := 0.0
		if total > 0 {
			ratio = float64(live) / float64(total)
		}
		effRatio := ratio
		if eff > 0 {
			effRatio = float64(live) / float64(eff)
		}
		pct := int(math.Round(ratio * 100))
		if pct > 100 {
			// live > configured happens transiently when a reload shrinks
			// the desired set; a >100% figure would be nonsense in
			// systemctl status.
			pct = 100
		}
		word := "critical"
		switch {
		case effRatio*100 >= statusActiveBand:
			word = "active"
		case effRatio*100 >= statusPartialBand:
			word = "partial"
		case effRatio*100 >= statusDegradedBand:
			word = "degraded"
		}
		line := fmt.Sprintf("%s: %d/%d proxies authenticated (%d%%)", word, live, total, pct)
		// The percentage and the park/pause notes always render, even at
		// zero live: an operator with every proxy parked still sees why.
		if parked > 0 {
			line += fmt.Sprintf(", %d parked by proxy audit", parked)
		}
		if proxyAuditPaused.Load() {
			line += "; proxy audit paused (paid proxy list unreadable)"
		}
		if word == "degraded" || word == "critical" {
			return line + ", retrying"
		}
		return line
	}
}

// reportProxyStatusToSystemd pushes the current line to systemd. Errors are
// discarded on purpose: this is cosmetic reporting, and a provider must never
// fail or stall because a status datagram did not land.
func reportProxyStatusToSystemd() {
	_ = notifySystemdStatus(systemdStatusLine())
}

// proxySourceWarningRateLimiter ensures that when a proxy source fails, the
// operator sees a WARNING the first time and then periodically — not on
// every fetch cycle (which can be every 15 s).
type proxySourceWarningRateLimiter struct {
	mu       sync.Mutex
	lastWarn time.Time
	interval time.Duration
}

var proxySourceWarnings = &proxySourceWarningRateLimiter{
	interval: 5 * time.Minute,
}

// warnProxySourceFailure emits a rate-limited operator WARNING the first
// time a source yields nothing and then periodically. The URL and reason are
// named so an operator can triage without reading the per-cycle debug line.
func warnProxySourceFailure(url string, reason string) {
	now := time.Now()
	proxySourceWarnings.mu.Lock()
	defer proxySourceWarnings.mu.Unlock()
	if now.Sub(proxySourceWarnings.lastWarn) < proxySourceWarnings.interval {
		return
	}
	proxySourceWarnings.lastWarn = now
	importantLogf("[proxy][url] WARNING: proxy source %s: %s — retrying\n", url, reason)
}

// truncateReason shortens a failure reason to maxLen, appending "..." if
// truncated. Exported so tests can reuse it.
func truncateReason(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// resetProxyResolutionStatus resets resolution state to pending. Intended
// for tests; production code never needs this.
func resetProxyResolutionStatus() {
	proxyResolutionStatus.Store(proxyResolutionPending)
	proxyResolutionReason.Store("")
}

// Startup phases, as reported to the live status snapshot. An empty phase means
// startup has settled.
const (
	startupResolving         = "resolving proxies"
	startupSourceUnreachable = "proxy source unreachable"
	startupSourceEmpty       = "proxy source returned no usable proxies"
)

// proxyStartupPhase reports what proxy startup is still waiting on, or "" once it
// has settled. It is the zero-proxy branch of systemdStatusLine, read the same
// way from the same inputs, so the two never disagree: a node systemd calls
// "starting: resolving proxies" is not IDLE in `urnet-tools status`.
//
// With proxies configured startup has settled. With none, the resolution status
// says what it is waiting on. The first reload after launch always resolves that
// status (empty, failed or ok), so a node with no proxies leaves "resolving"
// within moments and reads degraded, exactly as the systemd line does.
func proxyStartupPhase() string {
	if proxiesConfigured.Load() != 0 {
		return ""
	}
	switch proxyResolutionStatus.Load() {
	case proxyResolutionFailed:
		return startupSourceUnreachable
	case proxyResolutionEmpty:
		return startupSourceEmpty
	default: // proxyResolutionPending, or a stale OK with no proxies
		return startupResolving
	}
}
