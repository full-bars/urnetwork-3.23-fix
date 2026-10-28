package main

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var proxyDiagnosticScheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
var proxyDiagnosticAddress = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[A-Za-z0-9-]+)?$`)
var proxyDiagnosticPort = regexp.MustCompile(`^:[A-Za-z0-9-]+$`)

// redactProxySource describes a proxy without printing its credentials.
func redactProxySource(line string) string {
	line = strings.TrimSpace(line)
	scheme, rest := "", line
	if idx := strings.Index(line, "://"); idx >= 0 {
		if proxyDiagnosticScheme.MatchString(line[:idx]) {
			scheme = line[:idx]
		}
		rest = line[idx+3:]
	}
	address, user, password := parseProxyAddress(rest)
	// The parser does not support userinfo; colon-rich IPv6 can otherwise
	// masquerade as parsed credentials while leaving userinfo in address.
	unparsed := strings.Contains(rest, "@")
	if unparsed {
		address = rest
	}

	// The parser returns malformed input whole. Strip path/query/fragment
	// before userinfo so an @ in a query cannot expose its token.
	if cut := strings.IndexAny(address, "/?#"); cut >= 0 {
		address = address[:cut]
	}
	if unparsed && !strings.Contains(address, "@") {
		address = "<redacted>"
	} else {
		if at := strings.LastIndex(address, "@"); at >= 0 {
			address = address[at+1:]
		}
		if strings.HasPrefix(address, "[") && strings.Contains(address, "]") {
			end := strings.Index(address, "]")
			if net.ParseIP(address[1:end]) == nil || (address[end+1:] != "" && !proxyDiagnosticPort.MatchString(address[end+1:])) {
				address = "<redacted>"
			}
		} else {
			address = redactProxyLine(address)
			if !proxyDiagnosticAddress.MatchString(address) {
				address = "<redacted>"
			}
		}
	}
	userState, passwordState := "absent", "absent"
	if user != "" {
		userState = "present"
	}
	if password != "" {
		passwordState = "present"
	}
	if unparsed {
		userState, passwordState = "unparsed", "unparsed"
	}
	if scheme != "" {
		address = scheme + "://" + address
	}
	return fmt.Sprintf("%s (user=%s, password=%s)", address, userState, passwordState)
}
