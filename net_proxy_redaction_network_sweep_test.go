package connect

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// Every combination a printed dial can vary - the network name, the address
// family, a port or no port, the proxy configured by name or by literal, and the
// words in front of the address - leaves neither the resolved proxy address nor
// a character of the credential in the line. The credential is generated from
// the RFC 3986 userinfo set, so its sub-delimiters are exercised too.
func TestRedactProxyEndpointNetworkSweep(t *testing.T) {
	const v4 = "203.0.113.9"
	const v6 = "2001:db8::9"
	const port = "1080"
	const nameAddress = "gw.example.net:" + port
	const charSet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~%!$&*+/?#=:@"
	// '=' at the start of a token after a plain identifier is the key of a log
	// line ("err="), so it is generated only in the password
	const userSet = "abcdefghijklmnopqrstuvwxyz0123456789-._~%!$&*+/?#:@"

	families := []struct{ name, host, token string }{
		{"v4", v4, v4},
		{"v6", v6, "[" + v6 + "]"},
	}
	// the words in front of the address, each with the network name after it
	preludes := []string{"dial ", "err=dial ", "proxy dial ", "socks connect ", "proxyconnect tcp: dial "}
	withPorts := []struct {
		name   string
		suffix string
	}{{"with a port", ":" + port}, {"without a port", ""}}

	random := rand.New(rand.NewSource(20261122))
	generated := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[random.Intn(len(set))]
		}
		return string(b)
	}

	for _, network := range proxyNetworks {
		for _, family := range families {
			for _, withPort := range withPorts {
				for _, settingsForm := range []string{"name", "literal"} {
					address := family.token + withPort.suffix
					settings := proxyUserinfoSettings(nameAddress)
					if settingsForm == "literal" {
						settings = proxyUserinfoSettings(family.token + ":" + port)
					}
					for _, prelude := range preludes {
						for _, credential := range []string{"", generated(userSet, 8) + ":" + generated(charSet, 10) + "@"} {
							line := prelude + network + " " + credential + address
							name := fmt.Sprintf("%s/%s/%s/%s proxy/%q", network, family.name, withPort.name, settingsForm, prelude)
							t.Run(name, func(t *testing.T) {
								got := RedactProxyEndpoint(line, settings)
								if strings.Contains(got, family.host) {
									t.Fatalf("the proxy address stayed:\n in : %s\n got: %s", line, got)
								}
								if !strings.Contains(got, "proxy[3]") {
									t.Fatalf("the line does not name the slot:\n in : %s\n got: %s", line, got)
								}
								if userinfo := strings.TrimSuffix(credential, "@"); userinfo != "" && strings.Contains(got, userinfo) {
									t.Fatalf("the credential stayed:\n in : %s\n got: %s", line, got)
								}
							})
						}
					}
				}
			}
		}
	}
}

// A password carrying a scheme of its own is removed whole, whatever precedes
// the credential and whatever the outer scheme is. The injected "://" is the
// shape a malformed proxy URL ends up with.
func TestRedactProxyEndpointOuterSchemeSweep(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	const charSet = "abcdefghijklmnopqrstuvwxyz0123456789-._~%!$&*+/?#=:@"
	const userSet = "abcdefghijklmnopqrstuvwxyz0123456789-._~%!$&*+/?#:@"
	// a scheme-looking run inside the password, with the '=' a malformed URL
	// leaves in front of it and without one
	innerSchemes := []string{"p=https://", "p=socks5://", "a=http://", "x://", "p=://", "socks://", "p=wss://"}
	prefixes := []string{"", "dial ", "err=", "addr=", "proxy ", `"`, `Get "`}
	outerSchemes := []string{"", "socks5://", "http://", "SOCKS5H://"}

	random := rand.New(rand.NewSource(20261123))
	generated := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[random.Intn(len(set))]
		}
		return string(b)
	}

	for i := 0; i < 400; i++ {
		user := generated(userSet, 9)
		password := generated(charSet, 6) + innerSchemes[i%len(innerSchemes)] + generated(charSet, 8)
		outer := outerSchemes[i%len(outerSchemes)]
		prefix := prefixes[i%len(prefixes)]
		line := prefix + outer + user + ":" + password + "@" + hostPort + " timeout"
		want := prefix + outer + "proxy[3] timeout"
		got := RedactProxyEndpoint(line, proxyUserinfoSettings(hostPort))
		if strings.Contains(got, user) || strings.Contains(got, password) {
			t.Fatalf("the credential stayed:\n in : %s\n got: %s", line, got)
		}
		if got != want {
			t.Fatalf("\n in : %s\n got: %s\nwant: %s", line, got, want)
		}
	}
}
