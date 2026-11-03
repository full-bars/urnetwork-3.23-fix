package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// realProxyDialError produces the error the way production does: a dial through
// ProxySettings.NewDialContext against a SOCKS5 endpoint, not a hand-built
// error that merely mentions a proxy.
func realProxyDialError(t *testing.T, settings *ProxySettings) error {
	t.Helper()
	dial := settings.NewDialContext(context.Background(), &net.Dialer{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", "203.0.113.7:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("the dial must fail")
	}
	var proxyErr *proxyDialError
	if !errors.As(err, &proxyErr) {
		t.Fatalf("test premise broken: %v is not a proxyDialError", err)
	}
	return err
}

func TestH1PlusFailureClassProxyHopFailures(t *testing.T) {
	// a proxy endpoint nobody listens on: the refused dial to the hop itself
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	// a proxy that greets, then refuses the CONNECT with a SOCKS5 reply failure
	replyAddr, _, stop := startSocks5Stub(t, func(socks5Connect) (string, error) {
		return "", errors.New("refuse")
	})
	defer stop()

	cases := map[string]*ProxySettings{
		"refused_dial":  {Network: "tcp", Address: deadAddr},
		"reply_failure": {Network: "tcp", Address: replyAddr},
	}
	for name, settings := range cases {
		t.Run(name, func(t *testing.T) {
			err := realProxyDialError(t, settings)
			if isTimeoutFailure(err) {
				t.Fatalf("test premise broken: %v classifies as a timeout", err)
			}
			if got := classifyH1PlusFailure(err); got != h1PlusFailureProxyConnect {
				t.Fatalf("%v classified as %v; want proxy_connect", err, got)
			}

			stats := &H1PlusStats{}
			RecordH1PlusSelection(stats, time.Millisecond, err)
			snap := stats.Snapshot()
			if snap.HandshakeFailuresProxyConnect != 1 || snap.HandshakeFailures != 1 {
				t.Fatalf("proxy_connect=%d failures=%d; want 1 and 1", snap.HandshakeFailuresProxyConnect, snap.HandshakeFailures)
			}
		})
	}
}
