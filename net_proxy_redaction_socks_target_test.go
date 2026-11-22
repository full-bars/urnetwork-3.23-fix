package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/proxy"
)

// errorDialer fails every dial with the error it was built with, so the real
// x/net socks wrapper builds its own error offline.
type errorDialer struct{ err error }

func (self errorDialer) Dial(_, _ string) (net.Conn, error) { return nil, self.err }

func (self errorDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, self.err
}

// The socks wrapper prints the proxy it connected to before the arrow and the
// command target after it ("socks connect tcp4 <proxy>-><target>"), so the
// target is the destination, never the hop. The hop inside the wrapped dial
// error is still the proxy's and is still redacted.
func TestRedactProxyEndpointSocksConnectKeepsTheTarget(t *testing.T) {
	const (
		named       = "gw.example.net:1080"
		hop         = "203.0.113.9:1080"
		target      = "198.51.100.20:1080"
		settingsIdx = 3
	)
	settings := &ProxySettings{Index: settingsIdx, Network: "tcp4", Address: named}
	dialErr := &net.OpError{
		Op:   "dial",
		Net:  "tcp4",
		Addr: &net.TCPAddr{IP: net.ParseIP("203.0.113.9"), Port: 1080},
		Err:  errors.New("connection refused"),
	}
	socks, err := proxy.SOCKS5("tcp4", named, nil, errorDialer{dialErr})
	if err != nil {
		t.Fatalf("the socks wrapper did not build: %v", err)
	}
	_, dialFailure := socks.(proxy.ContextDialer).DialContext(context.Background(), "tcp4", target)
	if dialFailure == nil {
		t.Fatal("the failing forward dialer returned no error")
	}

	got := RedactProxyEndpoint(dialFailure.Error(), settings)
	for _, leaked := range []string{named, hop} {
		if strings.Contains(got, leaked) {
			t.Fatalf("the proxy or its hop survived: %q in %q", leaked, got)
		}
	}
	for _, kept := range []string{target, "connection refused", "proxy[3]"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("the line lost the target, the reason or the slot: %q in %q", kept, got)
		}
	}

	// the same shapes in a dial header keep the destination too
	for _, line := range []string{
		"socks connect tcp4 " + named + "->" + target + ": dial tcp4 " + hop + ": connection refused",
		"[net]dial tcp4 " + target + " err=socks5 connect tcp4 " + named + "->" + target + ": refused",
	} {
		out := RedactProxyEndpoint(line, settings)
		if strings.Contains(out, named) || strings.Contains(out, hop) {
			t.Fatalf("the proxy or its hop survived in %q: %q", line, out)
		}
		if !strings.Contains(out, target) {
			t.Fatalf("the destination was redacted in %q: %q", line, out)
		}
	}
}
