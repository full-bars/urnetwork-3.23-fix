package connect

import "testing"

// The credential begins after the outermost scheme of the token it sits in. A
// password may carry a scheme of its own, and a malformed proxy URL is exactly
// the kind of address an operator ends up with, so the inner "://" is part of
// the credential and never a new URL to start from.
func TestRedactProxyEndpointUserinfoOuterScheme(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	for _, testCase := range []struct {
		name    string
		address string
		in      string
		want    string
	}{
		{
			name: "a scheme inside the password",
			in:   "dial socks5://demo:p=https://tail@gw.example.net:1080: refused",
			want: "dial socks5://proxy[3]: refused",
		},
		{
			name: "a scheme inside the password with no leading word",
			in:   "socks5://demo:p=https://tail@gw.example.net:1080: refused",
			want: "socks5://proxy[3]: refused",
		},
		{
			name: "a scheme inside the password after err=",
			in:   "err=socks5://demo:p=https://tail@gw.example.net:1080 timeout",
			want: "err=socks5://proxy[3] timeout",
		},
		{
			name: "two schemes inside the password",
			in:   "dial socks5://demo:p=a://b@socks5://c@gw.example.net:1080: refused",
			want: "dial socks5://proxy[3]: refused",
		},
		{
			name: "an http scheme after addr=",
			in:   "addr=https://user:pw@gw.example.net:1080 timeout",
			want: "addr=https://proxy[3] timeout",
		},
		{
			name: "a scheme inside the password in quotes",
			in:   `Get "socks5://demo:p=https://tail@gw.example.net:1080": refused`,
			want: `Get "socks5://proxy[3]": refused`,
		},
		{
			name:    "a scheme inside the password of a v6 host",
			address: "[2001:db8::1]:1080",
			in:      "dial socks5://demo:p=https://tail@[2001:db8::1]:1080: refused",
			want:    "dial socks5://proxy[3]: refused",
		},
		{
			name: "the plain form still keeps its scheme",
			in:   "dial socks5://demo:pw@gw.example.net:1080: refused",
			want: "dial socks5://proxy[3]: refused",
		},
		{
			name: "a key still keeps its name",
			in:   "dial err=demo:pw@gw.example.net:1080 failed",
			want: "dial err=proxy[3] failed",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			address := testCase.address
			if address == "" {
				address = hostPort
			}
			got := RedactProxyEndpoint(testCase.in, proxyUserinfoSettings(address))
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
		})
	}
}
