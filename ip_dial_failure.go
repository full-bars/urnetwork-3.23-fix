package connect

import (
	"context"
	"errors"
	"os"
	"time"
)

// ip_dial_failure.go — the provider's answer to an upstream dial that failed
// (upstream connect IPV6.md C7). Taken from upstream's ip.go unedited apart from living in its
// own file until this line's ip.go is brought forward.

// inferredDialFailureTimeout is the multi client's silence-inference horizon,
// the retransmit at which it re-races a flow on its own (upstream
// ip_remote_multi_client.go). A dial that fails later than this is not signalled.
const inferredDialFailureTimeout = 3 * time.Second

// dialFailureAction classifies a failed upstream DialContext into the signal
// the provider sends back to the source in place of the SynAck. Without a
// signal the source sits in syn-retransmit backoff (3s..63s) and the blackhole
// timeout eventually removes the provider along with its working flows -- the
// bug this replaces for a dial that fails at once. A dial that times out is
// left to the source's own backoff and the client's silence inference (see
// classifyDialFailure).
type dialFailureAction int

const (
	// dialFailureNone: send nothing (a timed-out dial, a nil path, an
	// unsupported protocol or ip version).
	dialFailureNone dialFailureAction = iota
	// dialFailureRst: the destination itself refused the connection. The caller
	// answers with ConnectionState.RstAck() -- honest tcp semantics, forwarded
	// to the app.
	dialFailureRst
	// dialFailureUnreachable: capacity-class failure. The caller delivers the
	// icmp destination-unreachable packet returned alongside this action.
	dialFailureUnreachable
)

// classifyDialFailure maps an upstream DialContext error, and how long the dial
// took to fail, to the signal the provider should return in place of a SynAck.
// It is pure -- no live socket, no ConnectionState, no clock -- so the errno
// logic is unit-testable.
//
// For dialFailureUnreachable the built icmp packet is returned as well (a fresh
// non-pool buffer from ipOosUnreachable; the caller copies it into the pool
// before delivery). For dialFailureRst and dialFailureNone the packet is nil --
// a RST needs the ConnectionState's sequence state, so the caller builds it.
//
// A dial that timed out gets nothing (isDialTimeout), and neither does one
// that failed in any other way only after dialFailureSignalHorizon. A timed
// out dial fails at the dialer's ConnectTimeout, 15s after the SYN; a dial
// through a host-supplied dialer without a deadline can fail as late. By then
// the source has retransmitted at 1s and 3s, the multi client's silence
// inference has re-raced a flow that is still waiting, and an app racing
// address families has connected over the other family 250ms in and
// abandoned this attempt. A signal that late speeds nothing up. It only
// strikes the exit for a flow no one waits on, and three such strikes for a
// blackholed AAAA downgrade a healthy dual-stack exit to v4-only (upstream connect IPV6.md C7).
//
// Every other failure is immediate and answered. tcp + ECONNREFUSED means the
// destination refused: RST+ACK. Anything else on tcp (no route, unreachable,
// EMFILE/ENFILE, EADDRNOTAVAIL, unrecognized) is capacity-class and gets an
// unreachable; defaulting unrecognized errors to the capacity class is
// deliberate -- new clients intercept the signal and old ones drop it at
// ParseIpPath, so a misclassification is cheap. Go does not fold the Windows
// winsock codes onto syscall.ECONNREFUSED/ETIMEDOUT, so isDialRefused and
// isDialKernelTimeout add the native codes (ip_dial_errno_windows.go). udp "dial"
// cannot produce a meaningful ECONNREFUSED at connect time, so udp is always
// capacity-class.
func classifyDialFailure(ipPath *IpPath, err error, dialDuration time.Duration) (dialFailureAction, []byte) {
	if ipPath == nil || isDialTimeout(err) || dialFailureSignalHorizon <= dialDuration {
		return dialFailureNone, nil
	}
	switch ipPath.Protocol {
	case IpProtocolTcp:
		if isDialRefused(err) {
			return dialFailureRst, nil
		}
		// otherwise fall through to the unreachable build below
	case IpProtocolUdp:
		// always capacity-class; fall through
	default:
		return dialFailureNone, nil
	}
	if packet, ok := ipOosUnreachable(ipPath); ok {
		return dialFailureUnreachable, packet
	}
	return dialFailureNone, nil
}

// dialFailureSignalHorizon is how long a dial may take to fail and still be
// answered with the dial-failure signal: the multi client's silence-inference
// horizon, the retransmit at which it re-races a flow on its own. A later
// failure reaches the source after the inference, or an app's family
// fallback, has acted.
const dialFailureSignalHorizon = inferredDialFailureTimeout

// isDialTimeout reports whether a dial failed by running out of time rather
// than at once. The dialer's own timeout surfaces as context.DeadlineExceeded
// (net's "i/o timeout" matches it), or as os.ErrDeadlineExceeded when the
// socket's write deadline fires before the context's timer; the kernel's own
// connect timeout is ETIMEDOUT.
func isDialTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		isDialKernelTimeout(err)
}
