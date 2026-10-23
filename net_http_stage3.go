package connect

// Stage 3 adapter methods on the fork ClientStrategy: the surface the upstream
// platform transport (transport.go, transport_family.go) calls. Upstream grows
// these from its extender directory and delivery scoring, which this tree does
// not carry, so the extender-facing accessors return nil and the H1 dial runs on
// the fork's own evaluation loop.

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"time"
)

// DialerInfo describes the dialer a completed dial was won by. The extender ip is
// the zero value for a direct dial, which is what the platform transport reads to
// decide whether the connection is carried by an extender.
type DialerInfo struct {
	Description string
	ExtenderIp  netip.Addr
}

func (self *ClientStrategy) dialerInfo(dialer *clientDialer) *DialerInfo {
	info := &DialerInfo{Description: dialer.String()}
	if dialer.extenderConfig != nil {
		info.ExtenderIp = dialer.extenderConfig.Ip
	}
	return info
}

// H3ExtenderConfig is the extender an h3 carrier would dial through. This tree
// has no quic or dns extender dialers, so there never is one.
func (self *ClientStrategy) H3ExtenderConfig() *ExtenderConfig { return nil }

// ExtenderDirectory is the directory of discovered extenders (E1,
// net_extender_directory.go). The strategy does not draw candidates from it
// yet: the extender strategy rework is a later phase, so this returns nil and
// the platform transport reports no in-use addresses.
func (self *ClientStrategy) ExtenderDirectory() *ExtenderDirectory { return nil }

// ConnectSettings is the connect settings the strategy dials with.
func (self *ClientStrategy) ConnectSettings() *ConnectSettings {
	return &self.settings.ConnectSettings
}

// H1DialContextWithDialer dials the platform url as a framed H1 message
// connection (custom-upgrade negotiation with a fresh WS fallback inside each
// strategy attempt) on the fork's evaluation loop. An authorization denial is
// terminal for the logical dial and does not count against the dialer.
func (self *ClientStrategy) H1DialContextWithDialer(ctx context.Context, address string, requestHeader http.Header, maximum int, enabled bool, stats *H1PlusStats) (H1MessageConn, *DialerInfo, error) {
	causes := newHttpRequestCauses(ctx)
	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		info := self.dialerInfo(dialer)
		startDial := time.Now()
		timing := &dialTiming{}
		conn, err := dialH1MessagesWithinDeadline(timing.traceCtx(handleCtx, startDial), address, requestHeader, dialer.WsDialer(self.settings), H1FramerProtocol, maximum, enabled, stats, upgradeRouteScope(self.ipFamily, dialer))
		terminal, refusalOnly := httpUpgradeTerminalCauses(err)
		if !refusalOnly {
			// the same bookkeeping WsDialContext does for a websocket dial: the
			// strategy's probe record and the dialer's connect-latency sample (the
			// connect-only time when the trace saw it, else the whole dial)
			if dialer.Update(handleCtx, err, time.Since(startDial)) {
				self.probes.record(err == nil)
			}
			if err == nil {
				freshConnect, establishTime := timing.sample()
				if !freshConnect {
					establishTime = time.Since(startDial)
				}
				dialer.observeConnect(establishTime)
			}
			observeDialAttempt(handleCtx, err)
		}
		return causes.track(&evalResult{h1Conn: conn, dialerInfo: info, err: err, terminal: terminal})
	}
	// singleRound true, as WsDialContext: during a platform outage one failed
	// round ends the call instead of retrying every dialer until the request
	// deadline
	result, dialErr := self.parallelEvalErr(ctx, eval, true)
	if result == nil {
		return nil, nil, &h1DialError{dial: dialErr, causes: causes.exhausted(ctx, self.ctx)}
	}
	return result.h1Conn, result.dialerInfo, result.err
}

// upgradeRouteScope names the route one dialer takes, for the framed-upgrade
// rejection cache (see framedUpgradeCacheKey): the strategy's family pin plus
// the dialer's stable route identity (routeIdentity). The strategy's dialers
// are cached and reused, and a rebuilt dialer set keeps the same identity, so
// a rejection seen on one route is not forgotten by a rebuild and is never
// inherited by another route or by the other family's pin. The old pointer
// form had both failure modes: a rebuild forgot the rejection, and a reused
// pointer could inherit one.
func upgradeRouteScope(ipFamily int, dialer *clientDialer) string {
	return familyRouteScopePrefix(ipFamily) + dialer.routeIdentity()
}

// familyRouteScopePrefix keeps the pins of the dual-stack strategies from
// aliasing each other: the v4 pin, the v6 pin and a family-agnostic dialer are
// three different routes even when the dialer identity is the same.
func familyRouteScopePrefix(ipFamily int) string {
	switch ipFamily {
	case 4:
		return "ipv4|"
	case 6:
		return "ipv6|"
	default:
		return "any|"
	}
}

// h1DialError is the error of an H1 dial that produced no result. It reads as the
// strategy's dial error (the *dialTimeoutError the connect governor classifies)
// and also exposes the retained physical causes, which the credential-refusal and
// exhaustion checks inspect.
type h1DialError struct {
	dial   error
	causes error
}

func (self *h1DialError) Error() string {
	if self.dial != nil {
		return self.dial.Error()
	}
	return self.causes.Error()
}

// AuthRejected reports that the platform refused this identity (401 or 403 on the
// upgrade), so the connect governor files it as a refusal, not a backend failure.
func (self *h1DialError) AuthRejected() bool {
	var upgrade *HTTPUpgradeError
	return errors.As(self, &upgrade) && (upgrade.StatusCode == 401 || upgrade.StatusCode == 403)
}

func (self *h1DialError) Unwrap() []error {
	errs := []error{}
	if self.dial != nil {
		errs = append(errs, self.dial)
	}
	if self.causes != nil {
		errs = append(errs, self.causes)
	}
	return errs
}
