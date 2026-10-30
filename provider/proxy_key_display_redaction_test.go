package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// host:port is accepted only when both halves are safe to print, so a port
// that smuggles a parameter cannot ride through a valid-looking split.
func TestProxyKeyDisplayRejectsUnsafePortAndHost(t *testing.T) {
	unsafe := []string{
		"host:1080;password=secret",
		"host:1080 password=secret",
		"host:+1080",
		"host:0",
		"host:65536",
		"host:http",
		"ho;st:1080",
		"ho=st:1080",
		"host:1080%3Bpassword=secret",
		"socks5://alice:secret@host:1080/token",
	}

	for _, address := range unsafe {
		for _, key := range []string{address, (&connect.ProxySettings{Address: address, Auth: &proxy.Auth{User: "sppmr4vcnj"}}).Key()} {
			got := proxyKeyDisplay(key)
			for _, leak := range []string{"secret", "password", "1080;", "\x1f", "sppmr4vcnj"} {
				if strings.Contains(got, leak) {
					t.Errorf("display of %q leaks %q: %q", address, leak, got)
				}
			}
		}
	}

	for _, ok := range []string{"1.2.3.4:1080", "gw-1.example.com:65535", "[2001:db8::1]:1080", "direct"} {
		if got := redactedProxyAddress(ok); got != ok {
			t.Errorf("redactedProxyAddress(%q) = %q, want %q", ok, got, ok)
		}
	}
}

func TestPercentEncodedProxyUserinfoIsRedacted(t *testing.T) {
	address := "socks5%3A%2F%2Falice%3Asecret%40host:1080"
	got := redactedProxyAddress(address)
	for _, secret := range []string{"alice", "secret"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted address leaks %q: %q", secret, got)
		}
	}
}

// Errors from provide formatting proxySettings.Address must not leak credentials
// if the address string is malformed and contains userinfo or parameters.
func TestProvideProxyAddressErrorsAreRedacted(t *testing.T) {
	malformed := "socks5://alice:secret@host:1080/token"
	settings := &connect.ProxySettings{
		Address: malformed,
	}

	errUnreachable := fmt.Errorf("proxy unreachable: %s", redactedProxyAddress(settings.Address))
	for _, leak := range []string{"alice", "secret", "token"} {
		if strings.Contains(errUnreachable.Error(), leak) {
			t.Errorf("errUnreachable leaks %q: %v", leak, errUnreachable)
		}
	}

	errBelowBar := fmt.Errorf("%w: %s (score %.2f)", errProxyURLBelowBar, redactedProxyAddress(settings.Address), 0.42)
	for _, leak := range []string{"alice", "secret", "token"} {
		if strings.Contains(errBelowBar.Error(), leak) {
			t.Errorf("errBelowBar leaks %q: %v", leak, errBelowBar)
		}
	}
}
