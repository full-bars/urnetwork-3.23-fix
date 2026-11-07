package main

import (
	"net"
	"slices"
	"strings"

	"github.com/urnetwork/connect"
)

// extender_hosts.go decides which hosts the extender will forward to.
//
// The platform proves an extender before listing it: it dials back over tcp 443
// and forwards a real request to its OWN api host through the extender. An
// extender forwards only to hosts on its allow-list, so an empty list refuses
// the platform's own probe and the extender can never be activated. The sdk
// avoids that by allowing the platform's space by default; this does the same.

// extenderPlatformAllowedHosts is the platform's own space under an api url:
// the space host and one wildcard level beneath it (api.bringyour.com gives
// bringyour.com and *.bringyour.com). An api host with no space to derive, an IP
// literal or a single label, is allowed exactly and nothing more. A url that
// names no host allows nothing.
func extenderPlatformAllowedHosts(apiUrl string) []string {
	apiHost, err := connect.ExtenderApiHostName(apiUrl)
	if err != nil {
		return nil
	}
	if net.ParseIP(apiHost) != nil {
		return []string{apiHost}
	}
	space := connect.ExtenderNetworkHostName(apiHost)
	if space == apiHost {
		return []string{apiHost}
	}
	return []string{space, "*." + space}
}

// mergeExtenderHosts adds `extra` to `base`, keeping base's order and dropping
// repeats. An operator's own hosts go on top of the platform's, never in place
// of them: replacing them would quietly stop the extender being activated.
func mergeExtenderHosts(base []string, extra []string) []string {
	merged := slices.Clone(base)
	for _, host := range extra {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" && !slices.Contains(merged, host) {
			merged = append(merged, host)
		}
	}
	return merged
}
