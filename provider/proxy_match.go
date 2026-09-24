package main

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/urnetwork/connect"
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

// removeExcludePattern removes pattern (case-insensitive exact match)
// from state.ExcludePatterns. Returns true if a pattern was removed.
func removeExcludePattern(state *ProxyURLState, pattern string) bool {
	for i, p := range state.ExcludePatterns {
		if strings.EqualFold(p, pattern) {
			state.ExcludePatterns = append(state.ExcludePatterns[:i], state.ExcludePatterns[i+1:]...)
			return true
		}
	}
	return false
}

// collectMatchingProxies scans the three proxy stores for proxies whose host
// matches pattern and groups their proxy.state identity keys (address, or
// address+user; see ProxySettings.Key()) by the source keys that
// removeDeadProxies consumes ("internal", "file", "url"). URL-cache entries
// are keyed by bare address, so for "url" the key is the address.
//
// cfg is proxy.json (Servers keys may be the credentialed "host:port:user:pass"
// form, or carry credentials in Auths); stateProxies is proxy.state Proxies (empty
// or nil when the provider has never run); stateSource is proxy.state
// Source (the --proxy_file path, "" if none); urlCache is proxy_url.json
// Cache. URL-cache entries not yet present in state (fetched but never
// launched) are still collected so the removal is complete.
//
// display is a sorted, deduplicated human-readable list ("host:port (source)",
// never the raw identity key) for preview/confirm output.
func collectMatchingProxies(
	pattern string,
	cfg *ProxyConfig,
	stateProxies map[string]ProxyEntry,
	stateSource string,
	urlCache map[string]ProxyURLEntry,
) (addrsBySource map[string][]string, display []string) {
	addrsBySource = map[string][]string{}
	seen := map[string]bool{} // "source|addr" dedupe

	// key is a proxy identity key (ProxySettings.Key()), what removeDeadProxies
	// matches on, so two accounts at one gateway are removed independently and
	// a config entry and its proxy.state entry collapse into one proxy.
	add := func(source, key string) {
		seenKey := source + "|" + key
		if seen[seenKey] {
			return
		}
		seen[seenKey] = true
		addrsBySource[source] = append(addrsBySource[source], key)
		display = append(display, fmt.Sprintf("%s (%s)", proxyKeyDisplay(key), source))
	}

	var servers map[string]string
	if cfg != nil {
		servers = cfg.Servers
	}
	for proxyAddress, authKey := range servers {
		s := internalServerSettings(cfg, proxyAddress, authKey)
		if matchProxyHost(pattern, s.Address) {
			add("internal", s.Key())
		}
	}

	for key, entry := range stateProxies {
		addr, _ := connect.SplitProxyKey(key)
		if !matchProxyHost(pattern, addr) {
			continue
		}
		source := entry.Source
		if source == "" {
			if stateSource != "" {
				source = "file"
			} else {
				source = "internal"
			}
		}
		add(source, key)
	}

	for addr := range urlCache {
		if matchProxyHost(pattern, addr) {
			add("url", addr)
		}
	}

	sort.Strings(display)
	return addrsBySource, display
}
