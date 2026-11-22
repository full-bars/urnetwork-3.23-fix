package connect

import "testing"

// proxyUserinfoSettings is a proxy on a name with a port, the shape an operator
// writes and the shape a transport error repeats.
func proxyUserinfoSettings(address string) *ProxySettings {
	return &ProxySettings{Index: 3, Network: "tcp", Address: address}
}

// A proxy address that embeds credentials keeps them out of a log line in every
// form the transport prints it. The address a transport repeats is not always
// followed by a space: an error puts a colon after it ("dial addr: refused"),
// and a scheme-less credential run may carry a password sub-delimiter, both of
// which used to leave the credential next to the redacted endpoint.
func TestRedactProxyEndpointUserinfo(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	for _, testCase := range []struct {
		name    string
		address string
		in      string
		want    string
	}{
		{
			name: "ampersand password in a dial target",
			in:   "dial demo:part&suffix@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "scheme with ampersand password",
			in:   "dial socks5://demo:part&suffix@gw.example.net:1080: refused",
			want: "dial socks5://proxy[3]: refused",
		},
		{
			name: "plain password in a dial target",
			in:   "dial demo:plain@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "ampersand password after err=",
			in:   "err=demo:part&suffix@gw.example.net:1080 timeout",
			want: "err=proxy[3] timeout",
		},
		{
			name: "scheme after err=",
			in:   "err=socks5://demo:part&suffix@gw.example.net:1080 timeout",
			want: "err=socks5://proxy[3] timeout",
		},
		{
			name: "ampersand password after a word",
			in:   "proxy demo:part&suffix@gw.example.net:1080/path failed",
			want: "proxy proxy[3]/path failed",
		},
		{
			name: "at sign inside the password",
			in:   "dial demo:p@ss@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "at sign inside the password after err=",
			in:   "err=demo:p@ss@gw.example.net:1080 timeout",
			want: "err=proxy[3] timeout",
		},
		{
			name: "percent-encoded password",
			in:   "dial demo:p%40ss%2Fw@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "slash in the password",
			in:   "dial demo:p/w@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "question mark and equals in the password",
			in:   "dial demo:p?w=1@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "hash in the password",
			in:   "dial demo:p#w@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "equals and colon in the password",
			in:   "dial demo:p=w:x@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "sub-delimiters throughout the password",
			in:   "dial demo:pa&=ss@gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name:    "v6 host",
			address: "[2001:db8::1]:1080",
			in:      "dial demo:pw@[2001:db8::1]:1080: refused",
			want:    "dial proxy[3]: refused",
		},
		{
			name: "the same address twice",
			in:   "dial demo:pw@gw.example.net:1080 -> demo:pw@gw.example.net:1080 failed",
			want: "dial proxy[3] -> proxy[3] failed",
		},
		{
			name: "the address in quotes",
			in:   `Get "demo:pw@gw.example.net:1080": refused`,
			want: `Get "proxy[3]": refused`,
		},
		{
			name: "the address in single quotes after a paren",
			in:   "retry ('demo:pw@gw.example.net:1080'): refused",
			want: "retry ('proxy[3]'): refused",
		},
		{
			name: "the address after a comma",
			in:   "tried alpha,demo:pw@gw.example.net:1080; again",
			want: "tried alpha,proxy[3]; again",
		},
		{
			name: "an unrelated email stays",
			in:   "user@example.com saw dial demo:pw@gw.example.net:1080: refused",
			want: "user@example.com saw dial proxy[3]: refused",
		},
		{
			name: "no userinfo at all",
			in:   "dial gw.example.net:1080: refused",
			want: "dial proxy[3]: refused",
		},
		{
			name: "a key keeps its name",
			in:   "addr=demo:pw@gw.example.net:1080 timeout",
			want: "addr=proxy[3] timeout",
		},
		{
			name: "a key after a word keeps its name",
			in:   "dial err=demo:pw@gw.example.net:1080 failed",
			want: "dial err=proxy[3] failed",
		},
		{
			name:    "credentials in the configured address",
			address: "socks5://ops:pw@gw.example.net:1080",
			in:      "dial gw.example.net:1080: refused",
			want:    "dial proxy[3]: refused",
		},
		{
			name:    "credentials in the configured address and in the line",
			address: "socks5://ops:pw@gw.example.net:1080",
			in:      "dial socks5://ops:pw@gw.example.net:1080: refused",
			want:    "dial socks5://proxy[3]: refused",
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
