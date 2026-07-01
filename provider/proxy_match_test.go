package main

import "testing"

func TestHostOfAddress(t *testing.T) {
	cases := []struct{ in, want string }{
		{"dc.decodo.com:8001", "dc.decodo.com"},
		{"191.101.31.7:4444", "191.101.31.7"},
		{"noport.example.com", "noport.example.com"},
		{"[2001:db8::1]:1080", "2001:db8::1"},
	}
	for _, c := range cases {
		if got := hostOfAddress(c.in); got != c.want {
			t.Errorf("hostOfAddress(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMatchProxyHost(t *testing.T) {
	cases := []struct {
		pattern, address string
		want             bool
	}{
		{"dc.decodo.com", "dc.decodo.com:8001", true},
		{"DECODO", "dc.decodo.com:8001", true},              // case-insensitive
		{"decodo", "gate.smartproxy.com:7000", false},
		{"191.3.", "191.3.44.7:1080", true},                 // IP prefix
		{"8001", "dc.decodo.com:8001", false},               // never match port
		{"user", "host.com:1080:user:pass", false},          // never match credentials
		{"", "dc.decodo.com:8001", false},                   // empty pattern matches nothing
		{"decodo", "dc.decodo.com:8001:alice:secret", true}, // credentialed form still matches host
	}
	for _, c := range cases {
		if got := matchProxyHost(c.pattern, c.address); got != c.want {
			t.Errorf("matchProxyHost(%q, %q) = %v, want %v", c.pattern, c.address, got, c.want)
		}
	}
}
