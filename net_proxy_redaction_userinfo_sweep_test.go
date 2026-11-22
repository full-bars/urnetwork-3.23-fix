package connect

import (
	"math/rand"
	"strings"
	"testing"
)

// Every userinfo the generator builds is removed whole, whatever precedes it.
// The character set is the RFC 3986 userinfo set with the sub-delimiters a
// password carries. '=' is generated only in the password, because a plain
// identifier followed by '=' at the start of a token is read as a key
// ("err=...") by the rule this package applies, and that reading is the point
// of the rule.
func TestRedactProxyEndpointUserinfoSweep(t *testing.T) {
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
	// '=' at the start of a token after a plain identifier is the key of a log
	// line ("err="), so it is generated only in the password, where the ':'
	// between user and password is already in front of it
	userSet := strings.ReplaceAll(charSet, "=", "")

	for i := 0; i < 500; i++ {
		user := generated(userSet, 10)
		password := generated(charSet, 14)
		prefix := prefixes[i%len(prefixes)]
		suffix := suffixes[i%len(suffixes)]
		in := prefix + user + ":" + password + "@" + hostPort + suffix
		want := prefix + "proxy[3]" + suffix
		got := RedactProxyEndpoint(in, proxyUserinfoSettings(hostPort))
		if got != want {
			t.Fatalf("\n in : %s\n got: %s\nwant: %s", in, got, want)
		}
	}
}

// The rule for '=': at the start of a token, after a plain identifier, it
// separates a key from its value and the key stays. A password whose
// sub-delimiters include '=' is removed whole, and an ordinary key is not.
func TestRedactProxyEndpointUserinfoEqualsRule(t *testing.T) {
	const hostPort = "gw.example.net:1080"
	for _, testCase := range []struct {
		in   string
		want string
	}{
		{"err=demo:pw@gw.example.net:1080 timeout", "err=proxy[3] timeout"},
		{"a=demo:p=ss@gw.example.net:1080 timeout", "a=proxy[3] timeout"},
		{"dial demo:p=ss@gw.example.net:1080: refused", "dial proxy[3]: refused"},
		{"dial demo:=ss@gw.example.net:1080: refused", "dial proxy[3]: refused"},
		{"dial demo:pw=@gw.example.net:1080: refused", "dial proxy[3]: refused"},
		{"dial gw.example.net:1080: refused", "dial proxy[3]: refused"},
	} {
		if got := RedactProxyEndpoint(testCase.in, proxyUserinfoSettings(hostPort)); got != testCase.want {
			t.Fatalf("\n in : %s\n got: %s\nwant: %s", testCase.in, got, testCase.want)
		}
	}
}
