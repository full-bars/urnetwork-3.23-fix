package main

import (
	"fmt"
	"sync/atomic"
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
		return "starting: resolving proxies"
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
