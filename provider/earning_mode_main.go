package main

import "strings"

// isDirectAddr reports whether a proxy-health bandwidth key is the native
// [direct] identity rather than a real proxy.
//
// The direct identity is registered at index 0 with key "direct"
// (connect.RegisterProxy(0, "direct", "direct")). The bandwidth map that
// ProxyHealthSnapshot returns is keyed by formatProxyEntry, i.e.
// "proxy[0] (direct)" — NOT the bare key — while the sibling snapshot at
// proxy_health.go:511 keys by h.key. Both shapes must therefore be recognised
// here, or a direct-only box keeps reporting proxies_up=1. Match on the
// identity-bearing parts, not a prefix of the whole string.
func isDirectAddr(addr string) bool {
	if addr == directProxyKey {
		return true
	}
	// "proxy[0] (direct)" and any port-suffixed variant.
	if strings.Contains(addr, "("+directProxyKey+")") {
		return true
	}
	return strings.HasPrefix(addr, directProxyKey+":")
}

// earningMode names the transport shape a provider is running, for the
// [profit] line's mode= field. Four live transport states:
//
//	direct  — the native identity is up, no proxies are up
//	proxies — proxies are up, the direct transport is down or disabled
//	mixed   — both are up
//	none    — neither direct nor any proxy is up
//
// The distinction matters because a direct-only node earns real billable
// traffic, so earning=yes is correct, but it is not running any proxies. An
// operator reading only the counters could not tell the two apart.
func earningMode(directUp bool, proxiesUp int) string {
	hasProxies := proxiesUp > 0
	switch {
	case directUp && hasProxies:
		return "mixed"
	case hasProxies:
		return "proxies"
	case directUp:
		return "direct"
	default:
		return "none"
	}
}

// profitModeNote returns the human-readable tail for the [profit] line, or ""
// when the line needs no explanation.
//
// The rule is "say it only when it adds information": a pool with proxies up
// and earning is self-explanatory and gets no tail, so the common case stays
// uncluttered. The tail is a suffix on an otherwise machine-parseable line, so
// grep/alerting keys on mode= and the numeric fields and simply ignores it.
func profitModeNote(mode string, proxiesUp int) string {
	switch mode {
	case "direct":
		return " (direct mode, no proxies configured)"
	case "mixed":
		if proxiesUp == 0 {
			return " (direct on, no proxies up yet)"
		}
		return ""
	case "proxies":
		if proxiesUp == 0 {
			return " (no proxies up yet)"
		}
		return ""
	}
	return ""
}
