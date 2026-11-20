package connect

import "testing"

// The display helpers must refuse a host that carries credentials, however it
// is shaped, and keep every address a proxy list can legitimately hold.
func TestSafeProxyDisplayAddressRefusesCredentialLookingHosts(t *testing.T) {
	const bad = "<unparsable proxy address>"
	refused := []string{
		"[canary-user-7731:canary-pass-9042]:1080",
		"canary-user-7731:canary-pass-9042:example.com:1080",
		"canary-user-7731@example.com:1080",
		"example.com:0",
		"example.com:99999999999999999999",
		"[fe80::1%]:1080",
		"[fe80::1%eth0]:1080",
		"[fe80::1%a b]:1080",
		"[::1]:abc",
		"socks5://canary-user-7731:canary-pass-9042@example.com:1080/?token=canary-token-1",
	}
	for _, in := range refused {
		got := safeProxyDisplayAddress(in)
		if in == "socks5://canary-user-7731:canary-pass-9042@example.com:1080/?token=canary-token-1" {
			// the URL form keeps only its host and port
			if got != "example.com:1080" {
				t.Errorf("%q -> %q", in, got)
			}
			continue
		}
		if got != bad {
			t.Errorf("%q -> %q, want the placeholder", in, got)
		}
	}
	allowed := []string{
		"example.com:1080",
		"203.0.113.7:1080",
		"[2001:db8::1]:1080",
		"[::1]:1",
		"my-proxy_1.example.net:65535",
		"direct",
		"",
	}
	for _, in := range allowed {
		if got := safeProxyDisplayAddress(in); got != in {
			t.Errorf("%q -> %q, want it unchanged", in, got)
		}
	}
}

func TestProxyHostAndPortAllowed(t *testing.T) {
	for host, want := range map[string]bool{
		"example.com": true, "203.0.113.7": true, "2001:db8::1": true, "fe80::1%eth0": false,
		"": false, "a:b": false, "user:pass": false, "a b": false, "ex@mple.com": false, "::1%": false,
	} {
		if got := ProxyHostAllowed(host); got != want {
			t.Errorf("ProxyHostAllowed(%q) = %v", host, got)
		}
	}
	for port, want := range map[string]bool{"1": true, "1080": true, "65535": true, "0": false, "65536": false, "": false, "12a": false, "000001": false} {
		if got := ProxyPortAllowed(port); got != want {
			t.Errorf("ProxyPortAllowed(%q) = %v", port, got)
		}
	}
}
