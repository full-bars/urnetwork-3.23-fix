package connect

// transport_family_route_test.go covers a pinned transport whose family the
// routing has no route for. Such a transport sleeps instead of redialing
// (pinnedRouteHold, IPV6.md A5).
//
// The field case: a host whose only v6 address is a ULA, with its v6 default
// routes scoped to vpn tunnels. The address probe counts the ULA, so the v6
// pin was never held, and every dial of it was refused at once with no route
// to host (EHOSTUNREACH), every few minutes, forever.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noRouteDialError is what a stream dial of a family the routing has no route
// for returns: connect refused with no route to host.
func noRouteDialError(network string) error {
	return &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
}

// refusedDialError is a dial that reached the host and found no listener: the
// family has a route.
func refusedDialError(network string) error {
	return &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// newPinnedRouteTestTransport is a v6-pinned transport with the parts a dial
// verdict touches, and no run loops.
func newPinnedRouteTestTransport() *PlatformTransport {
	transport := &PlatformTransport{
		ipFamily:         6,
		log:              NewNoopLogger(),
		pinnedBackoff:    newPinnedDialBackoff(time.Second, time.Minute),
		routeHold:        newPinnedRouteHold(time.Hour, 4*time.Hour),
		held:             NewMonitorValue(false),
		kickMonitor:      NewMonitor(),
		connectedMonitor: NewMonitor(),
	}
	transport.enabled.Store(true)
	return transport
}

// The verdict is no route on every attempt, of either kind, and nothing else.
// The deadline or cancellation that closes an exhausted budget counts neither
// way.
func TestDialRouteRefused(t *testing.T) {
	exhausted := func(errs ...error) error {
		causes := newHttpRequestCauses(context.Background())
		for _, err := range errs {
			causes.record(err)
		}
		return causes.exhausted(context.Background(), nil)
	}
	attemptUrl := "ws://platform.space.example:443"
	noRoute := &url.Error{Op: "Get", URL: attemptUrl, Err: noRouteDialError("tcp6")}
	noNetwork := &net.OpError{Op: "dial", Net: "tcp6", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)}
	noRouteSend := &net.OpError{Op: "write", Net: "udp6", Err: os.NewSyscallError("sendmsg", syscall.EHOSTUNREACH)}
	refused := &url.Error{Op: "Get", URL: attemptUrl, Err: refusedDialError("tcp6")}
	timedOut := &net.OpError{Op: "dial", Net: "tcp6", Err: os.ErrDeadlineExceeded}

	for _, c := range []struct {
		name    string
		err     error
		refused bool
	}{
		{name: "no error", err: nil, refused: false},
		{name: "no route to host", err: noRouteDialError("tcp6"), refused: true},
		{name: "no route to network", err: noNetwork, refused: true},
		{name: "every address of an attempt", err: errors.Join(noRouteDialError("tcp6"), noNetwork), refused: true},
		{name: "exhausted with no route on every attempt", err: exhausted(noRoute, noRoute, noRoute), refused: true},
		{name: "h3 send", err: fmt.Errorf("h3 handshake failed: %w", noRouteSend), refused: true},
		{name: "exhausted with one refused port", err: exhausted(noRoute, refused), refused: false},
		{name: "one address refused", err: errors.Join(noRouteDialError("tcp6"), refusedDialError("tcp6")), refused: false},
		{name: "exhausted with no attempt", err: exhausted(), refused: false},
		{name: "canceled", err: context.Canceled, refused: false},
		{name: "refused port", err: refusedDialError("tcp6"), refused: false},
		{name: "timed out", err: timedOut, refused: false},
		{name: "tls", err: errors.New("tls: handshake failure"), refused: false},
	} {
		if refused := dialRouteRefused(c.err); refused != c.refused {
			t.Errorf("%s: dialRouteRefused = %t, want %t", c.name, refused, c.refused)
		}
	}
}

// A hold doubles per refusal after the one before ran out, up to the cap; a
// refusal while a hold is in force adds no strike; a reset forgets the strikes;
// and a timer armed for an ended hold cannot end the next one.
func TestPinnedRouteHoldDoublesToItsCap(t *testing.T) {
	hold := newPinnedRouteHold(5*time.Minute, time.Hour)
	for _, expected := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour,
	} {
		started := hold.refuse()
		if started != expected {
			t.Fatalf("hold = %s, want %s", started, expected)
		}
		if again := hold.refuse(); again != 0 {
			t.Fatalf("a refusal during a %s hold started another of %s", started, again)
		}
		hold.end(hold.value().generation)
	}

	hold.reset()
	if started := hold.refuse(); started != 5*time.Minute {
		t.Fatalf("hold after a reset = %s, want the base", started)
	}
	stale := hold.value().generation
	hold.reset()
	hold.refuse()
	hold.end(stale)
	if state := hold.value(); state.hold != 5*time.Minute {
		t.Fatalf("a stale timer ended the hold in force: %+v", state)
	}

	// a cap at or below the base does not grow; a non-positive base holds
	// nothing; a family-agnostic transport has no hold at all
	flat := newPinnedRouteHold(time.Hour, time.Minute)
	for range 3 {
		if started := flat.refuse(); started != time.Hour {
			t.Fatalf("hold under a cap below its base = %s, want %s", started, time.Hour)
		}
		flat.end(flat.value().generation)
	}
	if started := newPinnedRouteHold(0, time.Hour).refuse(); started != 0 {
		t.Fatalf("a disabled hold started %s", started)
	}
	var none *pinnedRouteHold
	if started := none.refuse(); started != 0 {
		t.Fatalf("a nil hold started %s", started)
	}
	none.reset()
	none.end(0)
}

// REGRESSION: a pinned dial that the routing refused on every attempt puts the
// transport to sleep for the base hold. Before the no-route hold, the dial
// failure only fed the pinned backoff, so the transport kept redialing its
// family that has no route.
func TestPinnedTransportSleepsWhenRoutingRefusesItsFamily(t *testing.T) {
	transport := newPinnedRouteTestTransport()

	transport.noteDialFailure(noRouteDialError("tcp6"))
	if hold := transport.routeHold.value().hold; hold != time.Hour {
		t.Fatalf("no-route hold after a refused dial = %s, want %s", hold, time.Hour)
	}
	if state := transport.State(); state != PlatformTransportStateSleeping {
		t.Fatalf("state = %s after a refused dial, want sleeping", state)
	}
	if !transport.held.Value() {
		t.Fatal("a transport asleep on no route must not dial")
	}

	// a refused port is a route, so it starts no hold
	transport = newPinnedRouteTestTransport()
	transport.noteDialFailure(refusedDialError("tcp6"))
	if hold := transport.routeHold.value().hold; hold != 0 {
		t.Fatalf("no-route hold after a refused port = %s, want none", hold)
	}
	if state := transport.State(); state == PlatformTransportStateSleeping {
		t.Fatal("a refused port put the transport to sleep")
	}
}

// The "[t]auth error" line names the transport's mode and pinned family and the
// causes the exhausted strategy retained.
func TestPlatformTransportAuthErrorLogNamesItsCauses(t *testing.T) {
	ctx := context.Background()
	causes := newHttpRequestCauses(ctx)
	causes.record(noRouteDialError("tcp6"))
	err := causes.exhausted(ctx, nil)

	log := &captureLogger{enabled: true}
	transport := newPinnedRouteTestTransport()
	transport.log = log
	transport.logAuthError(NewId(), TransportModeH1, err)

	log.mu.Lock()
	lines := append(append([]string{}, log.info...), log.verbose...)
	log.mu.Unlock()
	if len(lines) == 0 {
		t.Fatal("no auth error line")
	}
	for _, line := range lines {
		for _, expected := range []string{
			"(h1 ipv6)",
			(&HttpRequestExhaustedError{}).Error() + ": ",
			syscall.EHOSTUNREACH.Error(),
		} {
			if !strings.Contains(line, expected) {
				t.Errorf("auth error line %q does not name %q", line, expected)
			}
		}
	}
}
