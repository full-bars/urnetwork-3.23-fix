package main

import (
	"strings"
	"testing"
)

// parseProxyAddressLegacy is the parser as it stood before the userinfo forms
// were added: trailing host:port:user:password first, brackets for a v6 host,
// and every other line kept whole as the address. It is the reference the new
// parser is compared against, so a line whose shape the new forms do not
// select keeps byte for byte the reading it always had.
func parseProxyAddressLegacy(proxyAddress string) (address string, user string, password string) {
	if strings.HasPrefix(proxyAddress, "[") {
		if end := strings.Index(proxyAddress, "]"); end > 0 {
			rest := proxyAddress[end+1:]
			if strings.HasPrefix(rest, ":") {
				fields := strings.Split(rest[1:], ":")
				if isDigits(fields[0]) {
					switch len(fields) {
					case 1:
						return proxyAddress, "", ""
					case 3:
						return proxyAddress[:end+1] + ":" + fields[0], fields[1], fields[2]
					}
				}
			}
		}
		return proxyAddress, "", ""
	}
	if groups := proxyAddressWithCredentials.FindStringSubmatch(proxyAddress); groups != nil {
		return groups[1], groups[2], groups[3]
	}
	return proxyAddress, "", ""
}

// Every line the parser tests already used, read by the old parser and by the
// new one. The old reading is the reference: a line whose shape does not select
// a userinfo form comes back exactly as it did.
func TestParseProxyAddressLegacyFormsMatchTheBase(t *testing.T) {
	for _, in := range []string{
		"1.2.3.4:1080",
		"1.2.3.4:1080:alice:s3cret",
		"host.example.com:8000:u:p",
		"1.2.3.4:1080::",
		"1.2.3.4:1080:onlyuser",
		"",
		"[2001:db8::1]:1080",
		"[2001:db8::1]:1080:bob:pw",
		"[::1]:9050",
		"[::1]:9050::",
		"[2001:db8::1]:1080:u:p:extra",
		"[2001:db8::1]:1080:onlyuser",
		"[2001:db8::1]",
		"[2001:db8::1]:port",
		"[unterminated:1080",
		"203.0.113.10:1080:alice:p@ss",
	} {
		legacyAddress, legacyUser, legacyPassword := parseProxyAddressLegacy(in)
		address, user, password := parseProxyAddress(in)
		if address != legacyAddress || user != legacyUser || password != legacyPassword {
			t.Errorf("parseProxyAddress(%q) = %q %q %q, the earlier reading was %q %q %q",
				in, address, user, password, legacyAddress, legacyUser, legacyPassword)
		}
	}
}

// A password may end with an '@' and a host:port of its own. The trailing form
// owns a line that opens with host:port:, so that '@' belongs to the password
// and not to a userinfo run. The earlier parser read the last two colons, which
// cannot hold a host:port inside the password, so it kept such a line whole;
// the fields now cut at the address's own colon pair instead.
func TestParseProxyAddressTrailingPasswordKeepsItsAtSign(t *testing.T) {
	for _, testCase := range []struct {
		in, address, user, password string
		earlierMatches              bool
	}{
		{
			in:       "203.0.113.10:1080:alice:p@198.51.100.1:80",
			address:  "203.0.113.10:1080",
			user:     "alice",
			password: "p@198.51.100.1:80",
		},
		{
			in:       "host.example.com:8000:alice:p@198.51.100.1:80",
			address:  "host.example.com:8000",
			user:     "alice",
			password: "p@198.51.100.1:80",
		},
		{
			in:             "203.0.113.10:1080:alice:p@ss",
			address:        "203.0.113.10:1080",
			user:           "alice",
			password:       "p@ss",
			earlierMatches: true,
		},
	} {
		address, user, password := parseProxyAddress(testCase.in)
		if address != testCase.address || user != testCase.user || password != testCase.password {
			t.Fatalf("parseProxyAddress(%q) = %q %q %q, want %q %q %q",
				testCase.in, address, user, password, testCase.address, testCase.user, testCase.password)
		}
		if !testCase.earlierMatches {
			continue
		}
		legacyAddress, legacyUser, legacyPassword := parseProxyAddressLegacy(testCase.in)
		if address != legacyAddress || user != legacyUser || password != legacyPassword {
			t.Fatalf("parseProxyAddress(%q) = %q %q %q, the earlier reading was %q %q %q",
				testCase.in, address, user, password, legacyAddress, legacyUser, legacyPassword)
		}
	}
}

// The userinfo forms keep working for the shapes that select them: a scheme, or
// a user name in front of the address's own colon.
func TestParseProxyAddressUserinfoFormsStillApply(t *testing.T) {
	for _, testCase := range []struct {
		in, address, user, password string
	}{
		{"demo:s3cr3t@203.0.113.10:1080", "203.0.113.10:1080", "demo", "s3cr3t"},
		{"socks5://demo:s3cr3t@203.0.113.10:1080", "203.0.113.10:1080", "demo", "s3cr3t"},
		{"demo:s3cr3t@[2001:db8::1]:1080", "[2001:db8::1]:1080", "demo", "s3cr3t"},
		{"demo@203.0.113.10:1080", "203.0.113.10:1080", "demo", ""},
	} {
		address, user, password := parseProxyAddress(testCase.in)
		if address != testCase.address || user != testCase.user || password != testCase.password {
			t.Fatalf("parseProxyAddress(%q) = %q %q %q, want %q %q %q",
				testCase.in, address, user, password, testCase.address, testCase.user, testCase.password)
		}
	}
}
