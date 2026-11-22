package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/proxy"
)

// The error shapes below came off real dials on this host, with the loopback
// addresses rewritten to the documentation ones (203.0.113.9 for 127.0.0.1,
// 2001:db8::9 for ::1) and the port numbers normalized, since only the shape of
// the line matters here:
//
//	dial tcp 127.0.0.1:20597: connect: connection refused
//	dial tcp4 127.0.0.1:11517: connect: connection refused
//	dial tcp6 [::1]:21903: connect: connection refused
//	read udp 127.0.0.1:55283->127.0.0.1:54272: read: connection refused
//	read udp6 [::1]:45003->[::1]:13533: read: connection refused
//	socks connect tcp4 127.0.0.1:24825->example.com:443: dial tcp 127.0.0.1:24825: connect: connection refused
//	socks connect tcp 127.0.0.1:32485->example.com:443: read tcp 127.0.0.1:63276->127.0.0.1:32485: read: connection reset by peer
//	Get "https://example.com/": proxyconnect tcp: dial tcp 127.0.0.1:19137: connect: connection refused
//
// A failure that carries no credential passes through whole: the address it
// names is what tells an operator which proxy failed.
func TestRedactProxyCredentialsKeepsTheAddress(t *testing.T) {
	for _, testCase := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "tcp refused",
			in:   "dial tcp 203.0.113.9:1080: connect: connection refused",
			want: "dial tcp 203.0.113.9:1080: connect: connection refused",
		},
		{
			name: "tcp4 refused",
			in:   "dial tcp4 203.0.113.9:1080: connect: connection refused",
			want: "dial tcp4 203.0.113.9:1080: connect: connection refused",
		},
		{
			name: "tcp6 refused",
			in:   "dial tcp6 [2001:db8::9]:1080: connect: connection refused",
			want: "dial tcp6 [2001:db8::9]:1080: connect: connection refused",
		},
		{
			name: "udp peer",
			in:   "read udp 203.0.113.9:45555->203.0.113.9:1080: read: connection refused",
			want: "read udp 203.0.113.9:45555->203.0.113.9:1080: read: connection refused",
		},
		{
			name: "udp6 peer",
			in:   "read udp6 [2001:db8::9]:45555->[2001:db8::9]:1080: read: connection refused",
			want: "read udp6 [2001:db8::9]:45555->[2001:db8::9]:1080: read: connection refused",
		},
		{
			name: "name that did not resolve",
			in:   "dial tcp: lookup no-such-host.invalid: no such host",
			want: "dial tcp: lookup no-such-host.invalid: no such host",
		},
		{
			name: "socks refused after the connect",
			in:   "socks connect tcp4 203.0.113.9:1080->example.com:443: dial tcp 203.0.113.9:1080: connect: connection refused",
			want: "socks connect tcp4 203.0.113.9:1080->example.com:443: dial tcp 203.0.113.9:1080: connect: connection refused",
		},
		{
			name: "socks reset on a family socket",
			in:   "socks connect tcp6 203.0.113.9:1080->example.com:443: read tcp6 203.0.113.9:63276->203.0.113.9:1080: read: connection reset by peer",
			want: "socks connect tcp6 203.0.113.9:1080->example.com:443: read tcp6 203.0.113.9:63276->203.0.113.9:1080: read: connection reset by peer",
		},
		{
			name: "proxy connect through a family dial",
			in:   "Get \"https://example.com/\": proxyconnect tcp: dial tcp 203.0.113.9:1080: connect: connection refused",
			want: "Get \"https://example.com/\": proxyconnect tcp: dial tcp 203.0.113.9:1080: connect: connection refused",
		},
		{
			name: "dial header and error",
			in:   "[net]dial tcp 93.184.216.34:443 err=socks5 connect tcp 203.0.113.9:1080: connection refused",
			want: "[net]dial tcp 93.184.216.34:443 err=socks5 connect tcp 203.0.113.9:1080: connection refused",
		},
		{
			name: "an email in prose is not userinfo",
			in:   "operator user@example.com saw dial 203.0.113.9:1080: refused",
			want: "operator user@example.com saw dial 203.0.113.9:1080: refused",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := RedactProxyCredentials(testCase.in); got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
		})
	}
}

// A credential in any of the same shapes goes, and the address stays.
func TestRedactProxyCredentialsRemovesTheCredential(t *testing.T) {
	const user = "sentinel-user"
	const password = "s3n@t:in/el%2Fpw"
	cred := user + ":" + password + "@"
	for _, testCase := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "scheme-less in a dial target",
			in:   "dial tcp " + cred + "203.0.113.9:1080: connect: connection refused",
			want: "dial tcp 203.0.113.9:1080: connect: connection refused",
		},
		{
			name: "scheme-less on a v6 address",
			in:   "dial tcp6 " + cred + "[2001:db8::9]:1080: connect: connection refused",
			want: "dial tcp6 [2001:db8::9]:1080: connect: connection refused",
		},
		{
			name: "scheme-less with a trailing port colon",
			in:   "dial " + cred + "203.0.113.9:1080: refused",
			want: "dial 203.0.113.9:1080: refused",
		},
		{
			name: "socks wrapper keeps the target",
			in:   "socks connect tcp4 " + cred + "203.0.113.9:1080->example.com:443: dial tcp 203.0.113.9:1080: connection refused",
			want: "socks connect tcp4 203.0.113.9:1080->example.com:443: dial tcp 203.0.113.9:1080: connection refused",
		},
		{
			name: "log key keeps its name",
			in:   "err=" + cred + "203.0.113.9:1080 timeout",
			want: "err=203.0.113.9:1080 timeout",
		},
		{
			name: "socks url in quotes",
			in:   "Get \"socks5://" + cred + "203.0.113.9:1080\": proxyconnect tcp: connection refused",
			want: "Get \"socks5://203.0.113.9:1080\": proxyconnect tcp: connection refused",
		},
		{
			name: "http url after a key",
			in:   "addr=http://" + cred + "gw.example.net:8080 failed",
			want: "addr=http://gw.example.net:8080 failed",
		},
		{
			name: "two credentials on one line",
			in:   "tried alpha:" + password + "@198.51.100.20:80 then demo:pw@198.51.100.21:80",
			want: "tried 198.51.100.20:80 then 198.51.100.21:80",
		},
		{
			name: "the same address twice",
			in:   "dial " + cred + "203.0.113.9:1080 -> " + cred + "203.0.113.9:1080 failed",
			want: "dial 203.0.113.9:1080 -> 203.0.113.9:1080 failed",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactProxyCredentials(testCase.in)
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
			for _, leaked := range []string{user, password, "el%2Fpw", "@t:in"} {
				if strings.Contains(got, leaked) {
					t.Fatalf("a part of the credential stayed (%q): %s", leaked, got)
				}
			}
		})
	}
}

// Go prints a url.Error with the URL it was given. It masks the userinfo up to
// the first '@', and a password may carry one, so the tail of the password is
// still the redactor's to remove.
func TestRedactProxyCredentialsURLUserinfo(t *testing.T) {
	const (
		user     = "sentinel-user"
		password = "s3n@t:in/el%2Fpw"
	)
	link := "socks5://" + user + ":" + password + "@gw.example.net:443"
	got := RedactProxyCredentials((&url.Error{
		Op:  "Get",
		URL: link,
		Err: errors.New("proxyconnect tcp: connection refused"),
	}).Error())
	for _, leaked := range []string{user, "el%2Fpw", "@t:in", "s3n"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("a url.Error kept %q: %q", leaked, got)
		}
	}
	if !strings.Contains(got, "gw.example.net:443") {
		t.Fatalf("a url.Error lost the address: %q", got)
	}
	if !strings.Contains(got, "proxyconnect tcp: connection refused") {
		t.Fatalf("a url.Error lost the reason: %q", got)
	}
}

// errorDialer fails every dial with the error it was built with, so the real
// x/net socks wrapper builds its own error offline.
type errorDialer struct{ err error }

func (self errorDialer) Dial(_, _ string) (net.Conn, error) { return nil, self.err }

func (self errorDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, self.err
}

// realSocksDialFailure is the error the x/net socks client returns for a proxy
// dial that failed to connect, built by the real wrapper over an in-memory
// forward dialer.
func realSocksDialFailure(t *testing.T, address string) error {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("bad test address %q: %v", address, err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil {
		t.Fatalf("bad test port %q: %v", portText, err)
	}
	refused := &net.OpError{
		Op:   "dial",
		Net:  "tcp4",
		Addr: &net.TCPAddr{IP: net.ParseIP(host), Port: port},
		Err:  errors.New("connect: connection refused"),
	}
	socks, err := proxy.SOCKS5("tcp4", address, nil, errorDialer{refused})
	if err != nil {
		t.Fatalf("the socks wrapper did not build: %v", err)
	}
	_, dialFailure := socks.(proxy.ContextDialer).DialContext(context.Background(), "tcp4", "example.com:443")
	if dialFailure == nil {
		t.Fatal("the failing forward dialer returned no error")
	}
	return dialFailure
}

// The dial line prints the destination and the error whole, the way it always
// did, so a failed proxied dial still says which address it reached and why.
func TestProxyDialLogKeepsTheAddress(t *testing.T) {
	const hop = "203.0.113.9:1080"
	dialFailure := realSocksDialFailure(t, hop)

	log := &captureLogger{enabled: true}
	settings := &ConnectSettings{
		Log:           log,
		ProxySettings: &ProxySettings{Index: 7, Network: "tcp4", Address: hop},
		DialContextSettings: &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, dialFailure
			},
		},
	}
	if _, err := settings.DialContext(context.Background(), "tcp4", "example.com:443"); err == nil {
		t.Fatal("the dial was expected to fail")
	}
	log.mu.Lock()
	text := strings.Join(append(append([]string{}, log.info...), log.verbose...), "")
	log.mu.Unlock()
	for _, kept := range []string{"[net]dial tcp4 example.com:443", hop, "connection refused"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("the dial log lost %q: %q", kept, text)
		}
	}
}

// The select line names the dialer by its proxy address, and the error beside it
// keeps its own address with any credential removed.
func TestProxyDialSelectLineKeepsTheAddress(t *testing.T) {
	const hop = "198.51.100.7:1080"
	dialer := &clientDialer{
		settings:    &ClientStrategySettings{ProxySettings: &ProxySettings{Index: 5, Network: "tcp", Address: hop}},
		description: "h1",
	}
	line := dialer.logLine(nil)
	for _, kept := range []string{"proxy[5]", hop, "h1", "success="} {
		if !strings.Contains(line, kept) {
			t.Fatalf("the select line lost %q: %q", kept, line)
		}
	}

	const credential = "sentinel-user:s3n@t:in/el%2Fpw@"
	failed := dialer.logLine(errors.New("socks5 connect tcp " + credential + hop + ": connection refused"))
	for _, kept := range []string{"proxy[5]", hop, "connection refused"} {
		if !strings.Contains(failed, kept) {
			t.Fatalf("the failed select line lost %q: %q", kept, failed)
		}
	}
	if strings.Contains(failed, "sentinel-user") || strings.Contains(failed, "in/el%2Fpw") {
		t.Fatalf("the failed select line kept the credential: %q", failed)
	}
}

// A log line may wrap a value in its own brackets, and a v6 proxy address
// already carries a pair of its own. The closing bracket of the wrapper ends
// the address token, and the closing bracket of a v6 literal is read with the
// literal, so both a wrapped and an unwrapped address keep every byte and the
// credential in front of either one goes.
func TestRedactProxyCredentialsBracketedWrapper(t *testing.T) {
	const cred = "demo:secret@"
	for _, testCase := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wrapped scheme-less credential",
			in:   "note [" + cred + "203.0.113.10:1080] refused",
			want: "note [203.0.113.10:1080] refused",
		},
		{
			name: "wrapped scheme-less credential on a v6 address",
			in:   "note [" + cred + "[2001:db8::1]:1080] refused",
			want: "note [[2001:db8::1]:1080] refused",
		},
		{
			name: "wrapped credential with a scheme",
			in:   "note [socks5://" + cred + "203.0.113.10:1080] refused",
			want: "note [socks5://203.0.113.10:1080] refused",
		},
		{
			name: "wrapped credential with a scheme on a v6 address",
			in:   "note [socks5://" + cred + "[2001:db8::1]:1080] refused",
			want: "note [socks5://[2001:db8::1]:1080] refused",
		},
		{
			name: "nested wrappers",
			in:   "note ([{" + cred + "203.0.113.10:1080}]) refused",
			want: "note ([{203.0.113.10:1080}]) refused",
		},
		{
			name: "bracket inside a brace",
			in:   "note [{" + cred + "203.0.113.10:1080}] refused",
			want: "note [{203.0.113.10:1080}] refused",
		},
		{
			name: "a wrapper that is never closed",
			in:   "note [" + cred + "203.0.113.10:1080 refused",
			want: "note [203.0.113.10:1080 refused",
		},
		{
			name: "unwrapped credential on a v6 address",
			in:   "dial tcp6 " + cred + "[2001:db8::1]:1080: connection refused",
			want: "dial tcp6 [2001:db8::1]:1080: connection refused",
		},
		{
			name: "unwrapped credential on a bare v6 address",
			in:   "dial " + cred + "[2001:db8::1] refused",
			want: "dial [2001:db8::1] refused",
		},
		{
			name: "a bare v6 address with no credential is unchanged",
			in:   "dial tcp6 [2001:db8::9]:1080: connect: connection refused",
			want: "dial tcp6 [2001:db8::9]:1080: connect: connection refused",
		},
		{
			name: "a bracketed v6 peer pair with no credential is unchanged",
			in:   "read udp6 [2001:db8::9]:45555->[2001:db8::9]:1080: read: connection refused",
			want: "read udp6 [2001:db8::9]:45555->[2001:db8::9]:1080: read: connection refused",
		},
		{
			name: "a broken v6 literal is left whole",
			in:   "note [" + cred + "[2001:db8::1:1080 refused",
			want: "note [" + cred + "[2001:db8::1:1080 refused",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactProxyCredentials(testCase.in)
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
			if strings.Contains(got, "secret") && !strings.Contains(testCase.want, "secret") {
				t.Fatalf("the credential stayed: %s", got)
			}
		})
	}
}
