package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// A proxy line that carries its credentials as userinfo is parsed into an
// address and an Auth, so no later line can print the password from the address.
func TestParseProxyAddressUserinfo(t *testing.T) {
	for _, testCase := range []struct {
		name, in, address, user, password string
	}{
		{
			name:     "userinfo in front of the address",
			in:       "demo:s3cr3t@203.0.113.10:1080",
			address:  "203.0.113.10:1080",
			user:     "demo",
			password: "s3cr3t",
		},
		{
			name:     "userinfo with a scheme",
			in:       "socks5://demo:s3cr3t@203.0.113.10:1080",
			address:  "203.0.113.10:1080",
			user:     "demo",
			password: "s3cr3t",
		},
		{
			name:     "userinfo with an http scheme",
			in:       "http://demo:s3cr3t@gw.example.net:8080",
			address:  "gw.example.net:8080",
			user:     "demo",
			password: "s3cr3t",
		},
		{
			name:     "userinfo with a password that carries an at-sign",
			in:       "socks5://demo:p@ss@203.0.113.10:1080",
			address:  "203.0.113.10:1080",
			user:     "demo",
			password: "p@ss",
		},
		{
			name:     "userinfo on a bracketed v6 address",
			in:       "demo:s3cr3t@[2001:db8::1]:1080",
			address:  "[2001:db8::1]:1080",
			user:     "demo",
			password: "s3cr3t",
		},
		{
			name:    "a user name with no password",
			in:      "demo@203.0.113.10:1080",
			address: "203.0.113.10:1080",
			user:    "demo",
		},
		{
			name:    "a plain host:port is unchanged",
			in:      "203.0.113.10:1080",
			address: "203.0.113.10:1080",
		},
		{
			name:     "the trailing credential form is unchanged",
			in:       "203.0.113.10:1080:alice:s3cret",
			address:  "203.0.113.10:1080",
			user:     "alice",
			password: "s3cret",
		},
		{
			name:    "a port that is not a port is rejected",
			in:      "demo:s3cr3t@203.0.113.10:http",
			address: "",
		},
		{
			name:    "a path after the address is rejected",
			in:      "socks5://demo:***@203.0.113.10:1080/token",
			address: "",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address, user, password := parseProxyAddress(testCase.in)
			if address != testCase.address || user != testCase.user || password != testCase.password {
				t.Fatalf("parseProxyAddress(%q) = %q %q %q, want %q %q %q",
					testCase.in, address, user, password, testCase.address, testCase.user, testCase.password)
			}
		})
	}
}

// The address a parsed line yields never holds a credential, so a line that
// prints it cannot print one.
func TestParsedProxyAddressHoldsNoCredential(t *testing.T) {
	for _, in := range []string{
		"demo:s3cr3t@203.0.113.10:1080",
		"socks5://demo:s3cr3t@203.0.113.10:1080",
		"socks5h://demo:p@ss@[2001:db8::1]:1080",
	} {
		address, _, password := parseProxyAddress(in)
		if password == "" {
			t.Fatalf("%q did not parse a password", in)
		}
		if strings.Contains(address, "@") || strings.Contains(address, password) || strings.Contains(address, "demo") {
			t.Fatalf("%q parsed to an address holding a credential: %q", in, address)
		}
	}
}

// The startup list prints the address with the credentials removed and the
// obfuscated user and password markers, and a plain address exactly as before.
func TestProxyStartupLine(t *testing.T) {
	plain := &connect.ProxySettings{Index: 3, Address: "203.0.113.10:1080"}
	if got, want := proxyStartupLine(plain), "  proxy[3] 203.0.113.10:1080 (<no user>/<no password>)"; got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}

	lined := &connect.ProxySettings{
		Index:   4,
		Address: "demo:s3cr3t@203.0.113.10:1080",
		Auth:    &proxy.Auth{User: "demo", Password: "s3cr3t"},
	}
	got := proxyStartupLine(lined)
	if want := "  proxy[4] 203.0.113.10:1080 (***/***)"; got != want {
		t.Fatalf("startup line = %q, want %q", got, want)
	}
	for _, leaked := range []string{"s3cr3t", "demo@"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("the startup line leaked %q: %q", leaked, got)
		}
	}
}

// An address that still holds userinfo when it reaches a display sink comes out
// with the address and without the credential.
func TestRedactedProxyAddressKeepsTheAddress(t *testing.T) {
	for _, testCase := range []struct{ in, want string }{
		{"203.0.113.10:1080", "203.0.113.10:1080"},
		{"demo:s3cr3t@203.0.113.10:1080", "203.0.113.10:1080"},
		{"socks5://demo:s3cr3t@203.0.113.10:1080", "203.0.113.10:1080"},
		{"direct", "direct"},
		{"host:1080;password=secret", "<unparsable proxy address>"},
	} {
		if got := redactedProxyAddress(testCase.in); got != testCase.want {
			t.Fatalf("redactedProxyAddress(%q) = %q, want %q", testCase.in, got, testCase.want)
		}
	}
}
