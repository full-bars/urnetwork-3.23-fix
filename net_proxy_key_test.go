package connect

import (
	"strings"
	"testing"

	"golang.org/x/net/proxy"
)

func TestProxySettingsKey_NoAuthEqualsAddress(t *testing.T) {
	s := &ProxySettings{Network: "tcp", Address: "1.2.3.4:1080"}
	if got := s.Key(); got != s.Address {
		t.Errorf("Key() = %q, want bare address %q for a nil-auth proxy", got, s.Address)
	}
}

func TestProxySettingsKey_EmptyUserEqualsAddress(t *testing.T) {
	s := &ProxySettings{Network: "tcp", Address: "1.2.3.4:1080", Auth: &proxy.Auth{User: "", Password: "x"}}
	if got := s.Key(); got != s.Address {
		t.Errorf("Key() = %q, want bare address %q for an empty-user auth", got, s.Address)
	}
}

func TestProxySettingsKey_SameAddressDifferentUserAreDifferentKeys(t *testing.T) {
	a := &ProxySettings{Address: "dc.decodo.com:10058", Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "pw1"}}
	b := &ProxySettings{Address: "dc.decodo.com:10058", Auth: &proxy.Auth{User: "user-sppmr4vcnj-country-us-city-metro", Password: "pw1"}}
	if a.Key() == b.Key() {
		t.Fatalf("two different users at the same address collapsed to the same key: %q", a.Key())
	}
}

func TestProxySettingsKey_SameAddressSameUserDifferentPasswordIsSameKey(t *testing.T) {
	// A password rotation must not mint a new identity — otherwise ID,
	// health history, and earnings would be orphaned on every credential
	// rotation, which is exactly the LA7 rotation feature's normal case.
	a := &ProxySettings{Address: "dc.decodo.com:10058", Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "old-pw"}}
	b := &ProxySettings{Address: "dc.decodo.com:10058", Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "new-pw"}}
	if a.Key() != b.Key() {
		t.Fatalf("a password-only change produced a different key: %q vs %q", a.Key(), b.Key())
	}
}

func TestProxySettingsKey_NeverContainsPassword(t *testing.T) {
	s := &ProxySettings{Address: "dc.decodo.com:10058", Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "super-secret-password"}}
	if got := s.Key(); strings.Contains(got, "super-secret-password") {
		t.Fatalf("Key() leaked the password: %q", got)
	}
}

func TestSplitProxyKey_RoundTripsWithKey(t *testing.T) {
	s := &ProxySettings{Address: "dc.decodo.com:10058", Auth: &proxy.Auth{User: "sppmr4vcnj", Password: "pw"}}
	addr, user := SplitProxyKey(s.Key())
	if addr != s.Address || user != s.Auth.User {
		t.Errorf("SplitProxyKey(%q) = (%q, %q), want (%q, %q)", s.Key(), addr, user, s.Address, s.Auth.User)
	}
}

func TestSplitProxyKey_NoAuthKeyHasEmptyUser(t *testing.T) {
	s := &ProxySettings{Address: "1.2.3.4:1080"}
	addr, user := SplitProxyKey(s.Key())
	if addr != s.Address || user != "" {
		t.Errorf("SplitProxyKey(%q) = (%q, %q), want (%q, \"\")", s.Key(), addr, user, s.Address)
	}
}
