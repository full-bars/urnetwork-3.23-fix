package connect

import (
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"testing"
)

// credentialSweepNetworks are the network names the net package prints in front
// of an address it dialed or read from, including the family-specific ones.
var credentialSweepNetworks = []string{"tcp", "tcp4", "tcp6", "udp", "udp4", "udp6"}

// credentialParts is every piece of a generated credential that must not appear
// in a redacted line: the user name, the password, and each run of letters and
// digits long enough that a match in the line cannot be a coincidence.
func credentialParts(credential string) []string {
	userinfo := strings.TrimSuffix(credential, "@")
	if userinfo == "" {
		return nil
	}
	parts := append(strings.SplitN(userinfo, ":", 2), userinfo)
	const shortestRun = 4
	run := 0
	for i := 0; i <= len(userinfo); i++ {
		if i < len(userinfo) && isAlnumByte(userinfo[i]) {
			run++
			continue
		}
		if shortestRun <= run {
			parts = append(parts, userinfo[i-run:i])
		}
		run = 0
	}
	return parts
}

func isAlnumByte(b byte) bool {
	return ('0' <= b && b <= '9') || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}

// Every combination a printed dial can vary - the network name, the address
// family, a port or no port, and the words in front of the address - keeps the
// address whole and loses the credential whole. The credential is generated from
// the RFC 3986 userinfo set, so its sub-delimiters are exercised too. The line is
// compared whole, because a check against the credential string alone accepted a
// line that kept the user name and its colon in front of the address.
func TestRedactProxyCredentialsNetworkSweep(t *testing.T) {
	const v4 = "203.0.113.9"
	const v6 = "2001:db8::9"
	const port = "1080"
	const charSet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~%!$&*+/?#=:@"
	// '=' at the start of a token after a plain identifier is the key of a log
	// line ("err="), so it is generated only in the password
	const userSet = "abcdefghijklmnopqrstuvwxyz0123456789-._~%!$&*+/?#:@"

	families := []struct{ name, host, token string }{
		{"v4", v4, v4},
		{"v6", v6, "[" + v6 + "]"},
	}
	preludes := []string{"dial ", "err=dial ", "proxy dial ", "socks connect ", "proxyconnect tcp: dial "}
	withPorts := []struct {
		name   string
		suffix string
	}{{"with a port", ":" + port}, {"without a port", ""}}

	random := rand.New(rand.NewSource(20261122))
	generated := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[random.Intn(len(set))]
		}
		return string(b)
	}

	for _, network := range credentialSweepNetworks {
		for _, family := range families {
			for _, withPort := range withPorts {
				address := family.token + withPort.suffix
				for _, prelude := range preludes {
					for _, credential := range []string{"", generated(userSet, 8) + ":" + generated(charSet, 10) + "@"} {
						line := prelude + network + " " + credential + address
						want := prelude + network + " " + address
						name := fmt.Sprintf("%s/%s/%s/%q", network, family.name, withPort.name, prelude)
						t.Run(name, func(t *testing.T) {
							got := RedactProxyCredentials(line)
							for _, part := range credentialParts(credential) {
								if strings.Contains(want, part) {
									continue
								}
								if strings.Contains(got, part) {
									t.Fatalf("a part of the credential stayed: %q\n in : %s\n got: %s", part, line, got)
								}
							}
							if got != want {
								t.Fatalf("the line is not the redacted one:\n in : %s\n got: %s\nwant: %s", line, got, want)
							}
						})
					}
				}
			}
		}
	}
}

// Every userinfo the generator builds is removed whole, whatever precedes it.
// The character set is the RFC 3986 userinfo set with the sub-delimiters a
// password carries. '=' is generated only in the password, because a plain
// identifier followed by '=' at the start of a token is read as a key
// ("err=...") by the rule this package applies, and that reading is the point
// of the rule.
func TestRedactProxyCredentialsUserinfoSweep(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	const charSet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~%!$&*+/?#=:@"
	prefixes := []string{"dial ", "err=", "proxy ", "socks5://", `"`, ""}
	suffixes := []string{": refused", " timeout", "/path failed", `"`, ""}

	random := rand.New(rand.NewSource(20261121))
	generated := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[random.Intn(len(set))]
		}
		return string(b)
	}
	userSet := strings.ReplaceAll(charSet, "=", "")

	for i := 0; i < 500; i++ {
		user := generated(userSet, 10)
		password := generated(charSet, 14)
		prefix := prefixes[i%len(prefixes)]
		suffix := suffixes[i%len(suffixes)]
		in := prefix + user + ":" + password + "@" + hostPort + suffix
		want := prefix + hostPort + suffix
		got := RedactProxyCredentials(in)
		if got != want {
			t.Fatalf("\n in : %s\n got: %s\nwant: %s", in, got, want)
		}
	}
}

// The rule for '=': at the start of a token, after a plain identifier, it
// separates a key from its value and the key stays. A password whose
// sub-delimiters include '=' is removed whole, and an ordinary key is not.
func TestRedactProxyCredentialsUserinfoEqualsRule(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	for _, testCase := range []struct {
		in   string
		want string
	}{
		{"err=demo:pw@gw.example.net:1080 timeout", "err=gw.example.net:1080 timeout"},
		{"a=demo:p=ss@gw.example.net:1080 timeout", "a=gw.example.net:1080 timeout"},
		{"dial demo:p=ss@gw.example.net:1080: refused", "dial gw.example.net:1080: refused"},
		{"dial demo:=ss@gw.example.net:1080: refused", "dial gw.example.net:1080: refused"},
		{"dial demo:pw=@gw.example.net:1080: refused", "dial gw.example.net:1080: refused"},
		{"dial gw.example.net:1080: refused", "dial gw.example.net:1080: refused"},
	} {
		if got := RedactProxyCredentials(testCase.in); got != testCase.want {
			t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
		}
	}
}

// A token that opens with a URI scheme gives the credential both of its ends:
// the scheme says where it starts and the '@' before the host where it ends. In
// that form the punctuation that bounds a scheme-less credential is part of the
// password, so a ';', a comma, a bracket, a pipe or a backslash inside it does
// not leave the prefix in front of it in the line. The scheme-less limit is
// unchanged, and a log key in front of the value still keeps its name.
func TestRedactProxyCredentialsSchemeDelimiters(t *testing.T) {
	const (
		hostPort = "gw.example.net:1080"
		tail     = "/health\": connection refused"
	)
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
			want: `Get "socks5://` + hostPort + `/health": connection refused`,
		},
		{
			name: "a comma in the password",
			in:   `Get "socks5://demo:***@` + hostPort + tail,
			want: `Get "socks5://` + hostPort + `/health": connection refused`,
		},
		{
			name: "a pipe in the password",
			in:   `Get "socks5://demo:***@` + hostPort + tail,
			want: `Get "socks5://` + hostPort + `/health": connection refused`,
		},
		{
			name: "a bracket in the password",
			in:   `Get "socks5://demo:***@` + hostPort + tail,
			want: `Get "socks5://` + hostPort + `/health": connection refused`,
		},
		{
			name: "a paren and a brace in the password",
			in:   `Get "socks5://demo:***@` + hostPort + tail,
			want: `Get "socks5://` + hostPort + `/health": connection refused`,
		},
		{
			name: "a backslash the printer doubled",
			in:   `Get "socks5://demo:***@` + hostPort + tail,
			want: `Get "socks5://` + hostPort + `/health": connection refused`,
		},
		{
			name: "a key in front of the value still keeps its name",
			in:   `err=socks5://demo:***@` + hostPort,
			want: `err=socks5://` + hostPort,
		},
		{
			name: "an addr= value behind a key still keeps its name",
			in:   `addr=https://user:***@` + hostPort,
			want: `addr=https://` + hostPort,
		},
		{
			name: "a scheme-less credential keeps the documented limit",
			in:   `dial demo:pre;fix@` + hostPort,
			want: `dial demo:pre;` + hostPort,
		},
		{
			name: "a scheme-less credential with a comma",
			in:   `proxy demo:pre,fix@` + hostPort,
			want: `proxy demo:pre,` + hostPort,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactProxyCredentials(testCase.in)
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
		})
	}
}

// The credential begins after the outermost scheme of the token it sits in. A
// password may carry a scheme of its own, and a malformed proxy URL is exactly
// the kind of address an operator ends up with, so the inner "://" is part of
// the credential and never a new URL to start from.
func TestRedactProxyCredentialsOuterScheme(t *testing.T) {
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
			want: "dial socks5://gw.example.net:1080: refused",
		},
		{
			name: "a scheme inside the password with no leading word",
			in:   "socks5://demo:p=https://tail@gw.example.net:1080: refused",
			want: "socks5://gw.example.net:1080: refused",
		},
		{
			name: "a scheme inside the password after err=",
			in:   "err=socks5://demo:p=https://tail@gw.example.net:1080 timeout",
			want: "err=socks5://gw.example.net:1080 timeout",
		},
		{
			name: "two schemes inside the password",
			in:   "dial socks5://demo:p=a://b@socks5://c@gw.example.net:1080: refused",
			want: "dial socks5://gw.example.net:1080: refused",
		},
		{
			name: "an http scheme after addr=",
			in:   "addr=https://user:pw@gw.example.net:1080 timeout",
			want: "addr=https://gw.example.net:1080 timeout",
		},
		{
			name: "a scheme inside the password in quotes",
			in:   `Get "socks5://demo:p=https://tail@gw.example.net:1080": refused`,
			want: `Get "socks5://gw.example.net:1080": refused`,
		},
		{
			name:    "a scheme inside the password of a v6 host",
			address: "[2001:db8::1]:1080",
			in:      "dial socks5://demo:p=https://tail@[2001:db8::1]:1080: refused",
			want:    "dial socks5://[2001:db8::1]:1080: refused",
		},
		{
			name: "the plain form still keeps its scheme",
			in:   "dial socks5://demo:pw@gw.example.net:1080: refused",
			want: "dial socks5://gw.example.net:1080: refused",
		},
		{
			name: "a key still keeps its name",
			in:   "dial err=demo:pw@gw.example.net:1080 failed",
			want: "dial err=gw.example.net:1080 failed",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := RedactProxyCredentials(testCase.in)
			if got != testCase.want {
				t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
			}
			if strings.Contains(got, "tail") && !strings.Contains(testCase.want, "tail") {
				t.Fatalf("a piece of the password stayed: %q", got)
			}
		})
	}
}

// A password carrying a scheme of its own is removed whole, whatever precedes
// the credential and whatever the outer scheme is. The injected "://" is the
// shape a malformed proxy URL ends up with.
func TestRedactProxyCredentialsOuterSchemeSweep(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	const charSet = "abcdefghijklmnopqrstuvwxyz0123456789-._~%!$&*+/?#=:@"
	const userSet = "abcdefghijklmnopqrstuvwxyz0123456789-._~%!$&*+/?#:@"
	innerSchemes := []string{"p=https://", "p=socks5://", "a=http://", "x://", "p=://", "socks://", "p=wss://"}
	prefixes := []string{"", "dial ", "err=", "addr=", "proxy ", `"`, `Get "`}
	outerSchemes := []string{"", "socks5://", "http://", "SOCKS5H://"}

	random := rand.New(rand.NewSource(20261123))
	generated := func(set string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = set[random.Intn(len(set))]
		}
		return string(b)
	}

	for i := 0; i < 400; i++ {
		user := generated(userSet, 9)
		password := generated(charSet, 6) + innerSchemes[i%len(innerSchemes)] + generated(charSet, 8)
		outer := outerSchemes[i%len(outerSchemes)]
		prefix := prefixes[i%len(prefixes)]
		line := prefix + outer + user + ":" + password + "@" + hostPort + " timeout"
		want := prefix + outer + hostPort + " timeout"
		got := RedactProxyCredentials(line)
		if strings.Contains(got, user) || strings.Contains(got, password) {
			t.Fatalf("the credential stayed:\n in : %s\n got: %s", line, got)
		}
		if got != want {
			t.Fatalf("\n in : %s\n got: %s\nwant: %s", line, got, want)
		}
	}
}
