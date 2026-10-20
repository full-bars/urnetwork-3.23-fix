package connect

// control_family_proxy_strike_test.go — a proxied dial failure must never
// strike the address family. This is the NY5 bug: a fleet of proxies that could
// not reach the platform read as "IPv4 is blackholed", the ledger demoted IPv4,
// and control stayed off it for six hours.
//
// The tests drive the REAL error out of ProxySettings.NewDialContext against a
// loopback stub, then feed it to the guarded strike entry. A synthetic error
// would not prove the marker survives the dial path, and the marker is the
// whole mechanism.

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// TestProxiedDialFailureNeverStrikesTheFamily covers both proxy target family
// modes on purpose: the family ledger must be just as unreachable from a stall
// that happened while the provider was resolving the target locally as from one
// where the proxy resolved it.
func TestProxiedDialFailureNeverStrikesTheFamily(t *testing.T) {
	for _, mode := range []ProxyTargetFamilyMode{ProxyTargetFamilyV4, ProxyTargetFamilyProxy} {
		t.Run(mode.String(), func(t *testing.T) {
			assertProxiedDialCannotStrike(t, mode)
		})
	}
}

func assertProxiedDialCannotStrike(t *testing.T, mode ProxyTargetFamilyMode) {
	previousMode := SetProxyTargetFamily(mode)
	defer SetProxyTargetFamily(previousMode)

	// the demotion would otherwise be permitted: policy Auto, and the other
	// family proven usable on this device
	previousPolicy := ControlIpFamilyPolicy()
	SetControlIpFamilyPolicy(IpFamilyAuto)
	defer SetControlIpFamilyPolicy(previousPolicy)
	restoreProbe := swapControlFamilyProbe(func(family int) bool { return true })
	defer restoreProbe()
	controlFamilyClear()
	defer controlFamilyClear()

	// a stub that completes the greeting and then never answers the CONNECT, so
	// the dial fails with the caller's own deadline. This is the shape that
	// qualifies as family evidence on a direct path (isPathTimeout).
	proxyAddr, _, stop := startSocks5Stub(t, nil)
	defer stop()

	settings := &ProxySettings{Network: "tcp", Address: proxyAddr}
	dial := settings.NewDialContext(context.Background(), &net.Dialer{Timeout: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	conn, err := dial(ctx, "tcp", "api.bringyour.test:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("the stub answered nothing, so the dial must fail")
	}
	// the control that makes this test worth having: the error WOULD be a
	// strike on a direct path
	if !isPathTimeout(err) {
		t.Fatalf("test premise broken: %v is not a path timeout", err)
	}
	var proxyErr *proxyDialError
	if !errors.As(err, &proxyErr) {
		t.Fatalf("a proxied dial returned an unmarked error: %v", err)
	}
	if proxyErr.proxyIndex() != 0 {
		t.Fatalf("marker carries proxy index %d; want 0", proxyErr.proxyIndex())
	}
	// the message must survive verbatim, because connect classification reads it
	if proxyErr.Error() != proxyErr.Unwrap().Error() {
		t.Fatal("the marker changed the dial error's message")
	}

	if recordControlFamilyStrike(4, err) {
		t.Fatal("a proxied dial failure struck IPv4")
	}
	if until := controlFamilyDemotedUntil(4); !until.IsZero() {
		t.Fatalf("IPv4 was demoted until %s by a proxied failure", until)
	}
	if family := controlFamilyDemotedFamily(); family != 0 {
		t.Fatalf("family %d is demoted after a proxied failure", family)
	}
	// the observable consequence: control dials are still family-agnostic
	if got, dialErr := controlDialNetwork("tcp", "api.bringyour.com:443"); dialErr != nil || got != "tcp" {
		t.Fatalf("control dial network = %q, %v; want tcp (no demotion)", got, dialErr)
	}

	// a refusal through a proxy, which is the other shape a proxied dial fails
	// in, is refused for the same reason
	if recordControlFamilyStrike(6, &proxyDialError{index: 3, err: errors.New("socks connect: connection refused")}) {
		t.Fatal("a proxied refusal struck IPv6")
	}

	// positive control: the same strike without the proxy marker takes, so the
	// test above is not passing because the ledger refuses everything
	directTimeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	if !recordControlFamilyStrike(4, directTimeout) {
		t.Fatal("a direct post-connect timeout must still strike the family")
	}
	if controlFamilyDemotedUntil(4).IsZero() {
		t.Fatal("a direct post-connect timeout recorded no demotion")
	}
}

// TestProxiedFailureDoesNotFollowTheMode confirms the guard is not wired to the
// proxy target family setting: a proxied dial is not family evidence in proxy
// mode, and it was never evidence in v4 mode either.
func TestProxiedFailureDoesNotFollowTheMode(t *testing.T) {
	previousMode := SetProxyTargetFamily(ProxyTargetFamilyProxy)
	defer SetProxyTargetFamily(previousMode)

	previousPolicy := ControlIpFamilyPolicy()
	SetControlIpFamilyPolicy(IpFamilyAuto)
	defer SetControlIpFamilyPolicy(previousPolicy)
	restoreProbe := swapControlFamilyProbe(func(family int) bool { return true })
	defer restoreProbe()
	controlFamilyClear()
	defer controlFamilyClear()

	err := error(&proxyDialError{index: 1, err: context.DeadlineExceeded})
	if recordControlFamilyStrike(4, err) {
		t.Fatal("proxy mode: a proxied deadline struck IPv4")
	}
	if !controlFamilyDemotedUntil(4).IsZero() {
		t.Fatal("proxy mode: IPv4 was demoted")
	}
}
