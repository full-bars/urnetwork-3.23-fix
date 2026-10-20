package connect

// proxy_target_family.go — which resolver picks the address family of a
// proxied dial's destination.
//
// This line resolves a proxied target itself, with an A-only query, and hands
// the SOCKS5 proxy an IPv4 literal (lookupProxyTarget, net.go). The v2026
// engine does it the other way: the hostname goes to the proxy untouched, so
// the CONNECT carries the name and the proxy's own resolver picks A or AAAA.
// Both behaviours ship here, chosen by the proxy_target_family setting (control
// key proxy_target_family, environment URNETWORK_PROXY_TARGET_FAMILY).
//
// The default is v4, and it must stay the default during rollout: an upgrade
// that silently changed how every proxied dial resolves would make the proxy's
// resolver authoritative for thousands of dials an hour on every box at once,
// including boxes whose proxies have a worse resolver than this host. `proxy`
// is an operator opt-in.
//
// Process-global and read INSIDE each dial, like the control-plane family
// policy (control_family.go): a client strategy memoizes its http client and
// its tls dialers for the life of the process, so a value captured when a
// dialer was built could never be changed at runtime.

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// ProxyTargetFamilyMode is the resolver that names a proxied dial's
// destination.
type ProxyTargetFamilyMode int32

const (
	// ProxyTargetFamilyV4 resolves the target here with an A-only query and
	// sends the proxy an IPv4 literal. This is the behaviour every release
	// before this setting had, and the default.
	ProxyTargetFamilyV4 ProxyTargetFamilyMode = 0
	// ProxyTargetFamilyProxy leaves the name alone: the SOCKS5 CONNECT carries
	// the destination hostname and the proxy resolves it, so the proxy's own A
	// and AAAA answers decide the family. SOCKS5 never reports which family it
	// used, so this mode adds no per-proxy family evidence (a capability probe
	// is a separate change).
	ProxyTargetFamilyProxy ProxyTargetFamilyMode = 1
)

var proxyTargetFamily atomic.Int32

// ParseProxyTargetFamily reads a control or environment value. An empty value
// is the default, so a persisted file or an unset variable never has to carry
// the default explicitly.
func ParseProxyTargetFamily(value string) (ProxyTargetFamilyMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "v4", "ipv4":
		return ProxyTargetFamilyV4, nil
	case "proxy":
		return ProxyTargetFamilyProxy, nil
	}
	return ProxyTargetFamilyV4, fmt.Errorf("proxy-target-family: must be v4 or proxy (got %q)", value)
}

// SetProxyTargetFamily sets the mode for this process and returns the previous
// one. A value this build does not know is read as the default rather than an
// error: the caller is a persisted file or an environment variable written by
// another version, and the safe reading of "something I do not understand" is
// "keep doing what you did before".
func SetProxyTargetFamily(mode ProxyTargetFamilyMode) ProxyTargetFamilyMode {
	if mode != ProxyTargetFamilyProxy {
		mode = ProxyTargetFamilyV4
	}
	return ProxyTargetFamilyMode(proxyTargetFamily.Swap(int32(mode)))
}

// ProxyTargetFamily returns the mode in force.
func ProxyTargetFamily() ProxyTargetFamilyMode {
	return ProxyTargetFamilyMode(proxyTargetFamily.Load())
}

// ProxyTargetFamilyCarriesName reports whether a proxied dial hands the
// destination hostname to the proxy unresolved. It is the one condition the
// dial path needs, named so the intent at the call site is not a comparison
// against a constant.
func ProxyTargetFamilyCarriesName() bool {
	return ProxyTargetFamily() == ProxyTargetFamilyProxy
}

// String is the operator-facing name of the mode, the same token the control
// key and the environment variable accept.
func (self ProxyTargetFamilyMode) String() string {
	if self == ProxyTargetFamilyProxy {
		return "proxy"
	}
	return "v4"
}
