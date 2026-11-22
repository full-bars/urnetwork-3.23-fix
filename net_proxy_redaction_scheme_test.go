package connect

import (
	"errors"
	"net/url"
	"testing"
)

// A token that opens with a URI scheme gives the credential both of its ends:
// the scheme says where it starts and the '@' before the host where it ends. In
// that form the punctuation that bounds a scheme-less credential is part of the
// password, so a ';', a comma, a bracket, a pipe or a backslash inside it does
// not leave the prefix in front of it in the line. The scheme-less limit is
// unchanged, and a log key in front of the value still keeps its name.
func TestRedactProxyEndpointSchemeCredentialDelimiters(t *testing.T) {
	const (
		hostPort = "gw.example.net:1080"
		tail     = "/health\": connection refused"
	)
	// a url.Error prints the URL it was given, password included
	urlError := (&url.Error{
		Op:  "Get",
		URL: (&url.URL{Scheme: "socks5", User: url.UserPassword("demo", "prefix;tail"), Host: hostPort, Path: "/health"}).String(),
		Err: errors.New("connection refused"),
	}).Error()

	for _, testCase := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a semicolon in the password of a printed url.Error",
			in:   urlError,
			want: `Get "socks5://proxy[3]/health": connection refused`,
		},
		{
			name: "a comma in the password",
			in:   `Get "socks5://demo:pre,fix@` + hostPort + tail,
			want: `Get "socks5://proxy[3]/health": connection refused`,
		},
		{
			name: "a pipe in the password",
			in:   `Get "socks5://demo:pre|fix@` + hostPort + tail,
			want: `Get "socks5://proxy[3]/health": connection refused`,
		},
		{
			name: "a bracket in the password",
			in:   `Get "socks5://demo:pre[fix]@` + hostPort + tail,
			want: `Get "socks5://proxy[3]/health": connection refused`,
		},
		{
			name: "a paren and a brace in the password",
			in:   `Get "socks5://demo:pre(fix){x}@` + hostPort + tail,
			want: `Get "socks5://proxy[3]/health": connection refused`,
		},
		{
			name: "a backslash the printer doubled",
			in:   `Get "socks5://demo:pre\\fix@` + hostPort + tail,
			want: `Get "socks5://proxy[3]/health": connection refused`,
		},
		{
			name: "a key in front of the value still keeps its name",
			in:   `err=socks5://demo:pre;fix@` + hostPort,
			want: `err=socks5://proxy[3]`,
		},
		{
			name: "an addr= value behind a key still keeps its name",
			in:   `addr=https://user:pw;tail@` + hostPort,
			want: `addr=https://proxy[3]`,
		},
		{
			name: "a scheme-less credential keeps the documented limit",
			in:   `dial demo:pre;fix@` + hostPort,
			want: `dial demo:pre;proxy[3]`,
		},
		{
			name: "a scheme-less credential with a comma",
			in:   `proxy demo:pre,fix@` + hostPort,
			want: `proxy demo:pre,proxy[3]`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactProxyEndpoint(testCase.in, proxyUserinfoSettings(hostPort))
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
		})
	}
}
