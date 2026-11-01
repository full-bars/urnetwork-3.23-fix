package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
)

// newH3PacketConnFactory returns the PlatformTransportSettings.H3PacketConnFactory
// for an identity, or nil when the engine's default socket is right.
//
// A proxied identity (proxySettings != nil) always gets a factory: without one
// the engine opens a socket on the host and the identity's QUIC would leave from
// the host's address instead of its proxy's. The factory relays through the
// proxy and, if the proxy cannot, returns an error rather than a host socket, so
// the platform transport falls back to a TCP mode that already goes through the
// proxy.
//
// A direct identity keeps the engine's default socket and its physical-egress
// binding.
func newH3PacketConnFactory(proxySettings *connect.ProxySettings) func(context.Context) (net.PacketConn, error) {
	if proxySettings == nil {
		return nil
	}
	return func(ctx context.Context) (net.PacketConn, error) {
		// the grade only saw this proxy's TCP path; before QUIC goes through
		// its UDP relay make sure the relay works and exits from the address
		// TCP does
		switch result := globalProxyUDPCheck.Check(ctx, proxySettings, time.Now()); result.Status {
		case proxyUDPNoRelay, proxyUDPExitMismatch:
			return nil, fmt.Errorf("h3 not used through proxy %s: udp check %s", redactedProxyAddress(proxySettings.Address), result.Status)
		}
		relayed, err := dialSocks5UDP(ctx, proxySettings.Network, proxySettings.Address, proxySettings.Auth)
		if err != nil {
			// Do not wrap the dial error. It embeds the address the dialer was
			// given, and a malformed address can carry userinfo or a token path,
			// so wrapping would undo the redaction above. A cancellation stays
			// matchable so callers can tell a shutdown or timeout from a dead
			// relay.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("h3 through proxy %s: relay dial failed: %s", redactedProxyAddress(proxySettings.Address), dialFailureClass(err))
		}
		return relayed, nil
	}
}

// dialFailureClass names a dial failure without echoing the address the dialer
// was given. The underlying net error carries that address verbatim (userinfo
// and token path included) and this message reaches operator logs.
func dialFailureClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	return "dial error"
}
