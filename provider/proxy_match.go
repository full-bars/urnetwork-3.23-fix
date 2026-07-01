package main

import (
	"net"
	"strings"
)

// hostOfAddress returns the host portion of a "host:port" address.
// If the address has no port, the whole string is returned.
func hostOfAddress(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// matchProxyHost reports whether the host portion of proxyAddress contains
// pattern, case-insensitively. proxyAddress may be "host:port" or the
// credentialed "host:port:user:pass" form used in proxy.json keys; the
// port and credentials are never matched. An empty pattern matches nothing.
func matchProxyHost(pattern, proxyAddress string) bool {
	if pattern == "" {
		return false
	}
	// Strip credentials if present ("host:port:user:pass" form).
	address, _, _ := parseProxyAddress(proxyAddress)
	host := hostOfAddress(address)
	return strings.Contains(strings.ToLower(host), strings.ToLower(pattern))
}

// hostMatchesAny reports whether address's host matches any of patterns.
func hostMatchesAny(patterns []string, address string) bool {
	for _, p := range patterns {
		if matchProxyHost(p, address) {
			return true
		}
	}
	return false
}

// addExcludePattern appends pattern to state.ExcludePatterns unless an
// equal pattern (case-insensitive) is already present. Returns true if
// the pattern was added.
func addExcludePattern(state *ProxyURLState, pattern string) bool {
	for _, p := range state.ExcludePatterns {
		if strings.EqualFold(p, pattern) {
			return false
		}
	}
	state.ExcludePatterns = append(state.ExcludePatterns, pattern)
	return true
}
