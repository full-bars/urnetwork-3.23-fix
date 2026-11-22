package connect

import (
	"strings"
	"testing"
)

// Every shape below came off a real dial on this host, then the loopback
// addresses were rewritten to the documentation ones (203.0.113.9 for
// 127.0.0.1, 2001:db8::9 for ::1) and the port numbers normalized to the proxy
// port, since only the shape of the line matters here. The measured formats,
// with the loopback addresses left in place, were:
//
//	dial tcp 127.0.0.1:20597: connect: connection refused
//	dial tcp4 127.0.0.1:11517: connect: connection refused
//	dial tcp6 [::1]:21903: connect: connection refused
//	read udp 127.0.0.1:55283->127.0.0.1:54272: read: connection refused
//	read udp4 127.0.0.1:57793->127.0.0.1:19479: read: connection refused
//	read udp6 [::1]:45003->[::1]:13533: read: connection refused
//	dial tcp: lookup no-such-host.invalid: no such host
//	socks connect tcp4 127.0.0.1:24825->example.com:443: dial tcp 127.0.0.1:24825: connect: connection refused
//	socks connect tcp6 [::1]:21125->example.com:443: dial tcp [::1]:21125: connect: connection refused
//	socks connect tcp 127.0.0.1:32485->example.com:443: read tcp 127.0.0.1:63276->127.0.0.1:32485: read: connection reset by peer
//	socks connect tcp6 127.0.0.1:32485->example.com:443: read tcp6 127.0.0.1:63282->127.0.0.1:32485: read: connection reset by peer
//	Get "https://example.com/": proxyconnect tcp: dial tcp 127.0.0.1:19137: connect: connection refused
//	Get "https://example.com/": proxyconnect tcp: dial tcp [::1]:29551: connect: connection refused
//
// The proxy is configured by name, so its resolved address is not known to the
// redactor. What says the token is the hop is the network name in front of it,
// and Go prints the family-specific name whenever the dial names one.
func TestRedactProxyEndpointFamilyNetworks(t *testing.T) {
	settings := proxyUserinfoSettings("gw.example.net:1080")
	for _, testCase := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "tcp hop",
			in:   "dial tcp 203.0.113.9:1080: connect: connection refused",
			want: "dial tcp proxy[3]: connect: connection refused",
		},
		{
			name: "tcp4 hop",
			in:   "dial tcp4 203.0.113.9:1080: connect: connection refused",
			want: "dial tcp4 proxy[3]: connect: connection refused",
		},
		{
			name: "tcp6 hop",
			in:   "dial tcp6 [2001:db8::9]:1080: connect: connection refused",
			want: "dial tcp6 proxy[3]: connect: connection refused",
		},
		{
			name: "udp hop",
			in:   "dial udp 203.0.113.9:1080: connect: connection refused",
			want: "dial udp proxy[3]: connect: connection refused",
		},
		{
			name: "udp4 hop",
			in:   "dial udp4 203.0.113.9:1080: connect: connection refused",
			want: "dial udp4 proxy[3]: connect: connection refused",
		},
		{
			name: "udp6 hop",
			in:   "dial udp6 [2001:db8::9]:1080: connect: connection refused",
			want: "dial udp6 proxy[3]: connect: connection refused",
		},
		{
			name: "tcp4 hop without a port",
			in:   "dial tcp4 203.0.113.9: connect: connection refused",
			want: "dial tcp4 proxy[3]: connect: connection refused",
		},
		{
			name: "udp peer on a family socket",
			in:   "read udp4 203.0.113.9:45555->203.0.113.9:1080: read: connection refused",
			want: "read udp4 203.0.113.9:45555->proxy[3]: read: connection refused",
		},
		{
			name: "udp6 peer on a family socket",
			in:   "read udp6 [2001:db8::9]:45555->[2001:db8::9]:1080: read: connection refused",
			want: "read udp6 [2001:db8::9]:45555->proxy[3]: read: connection refused",
		},
		{
			name: "socks hop after a refused connect",
			in:   "socks connect tcp gw.example.net:1080->example.com:443: dial tcp [2001:db8::9]:1080: connect: connection refused",
			want: "socks connect tcp proxy[3]->example.com:443: dial tcp proxy[3]: connect: connection refused",
		},
		{
			name: "socks hop after a reset on a family socket",
			in:   "socks connect tcp6 gw.example.net:1080->example.com:443: read tcp6 [2001:db8::9]:63276->[2001:db8::9]:1080: read: connection reset by peer",
			want: "socks connect tcp6 proxy[3]->example.com:443: read tcp6 [2001:db8::9]:63276->proxy[3]: read: connection reset by peer",
		},
		{
			name: "socks4 style hop without a network name",
			in:   "socks5 connect 203.0.113.9:1080: refused",
			want: "socks5 connect proxy[3]: refused",
		},
		{
			name: "proxy connect through a family dial",
			in:   "Get \"https://example.com/\": proxyconnect tcp: dial tcp6 [2001:db8::9]:1080: connect: connection refused",
			want: "Get \"https://example.com/\": proxyconnect tcp: dial tcp6 proxy[3]: connect: connection refused",
		},
		{
			name: "the destination in front of the error keeps its port",
			in:   "[net]dial tcp6 [2001:db8::9]:1080 err=dial tcp6 [2001:db8::9]:1080: connect: connection refused",
			want: "[net]dial tcp6 [2001:db8::9]:1080 err=dial tcp6 proxy[3]: connect: connection refused",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactProxyEndpoint(testCase.in, settings)
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
		})
	}
}

// A name that did not resolve is not the proxy's address, so it stays; the line
// still names the slot it belongs to.
func TestRedactProxyEndpointUnresolvedNameKeepsTheName(t *testing.T) {
	got := RedactProxyEndpoint("dial tcp: lookup no-such-host.invalid: no such host", proxyUserinfoSettings("gw.example.net:1080"))
	if !strings.Contains(got, "no-such-host.invalid") {
		t.Fatalf("the unresolved name was removed: %s", got)
	}
	if !strings.Contains(got, "proxy[3]") {
		t.Fatalf("the line does not name the slot: %s", got)
	}
}
