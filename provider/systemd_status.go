package main

import (
	"fmt"
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
	case live == 0:
		return fmt.Sprintf("degraded: 0/%d proxies authenticated, retrying", total)
	case live < total:
		return fmt.Sprintf("partial: %d/%d proxies authenticated", live, total)
	default:
		return fmt.Sprintf("active: %d/%d proxies authenticated", live, total)
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
