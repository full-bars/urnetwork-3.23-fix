// Synthetic barriers reproduce detached hedge completion and complete resolver
// results without public DNS, timing races, or retained production identities.
package connect

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Fork note: upstream declares this in net_http_doh_test.go, which this tree
// does not carry. It is a plain RoundTripper adapter for the fixtures below.
type dohRoundTripperFunc func(*http.Request) (*http.Response, error)

func (self dohRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// A failed literal dial must retain its family even under the opposite global
// policy. Only finite result tokens may survive an identity-bearing error.
func TestDohDialObservationIsFiniteAndLiteral(t *testing.T) {
	oldPolicy := ControlIpFamilyPolicy()
	SetControlIpFamilyPolicy(IpFamilyForce4)
	defer SetControlIpFamilyPolicy(oldPolicy)
	for _, test := range []struct {
		address string
		family  string
		err     error
		result  string
	}{
		{address: "192.0.2.53:443", family: "4", err: os.ErrDeadlineExceeded, result: "timeout"},
		{address: "192.0.2.56:443", family: "4", err: errors.New("i/o timeout"), result: "timeout"},
		{address: "[2001:db8::53]:443", family: "6", err: syscall.EAFNOSUPPORT, result: "unsupported_family"},
		{address: "[::ffff:192.0.2.53]:443", family: "4", err: syscall.ECONNREFUSED, result: "refused"},
		{address: "resolver.private.example:443", family: "unknown", err: context.Canceled, result: "canceled"},
		{address: "192.0.2.54", family: "4", err: nil, result: "success"},
		{address: "[2001:db8::54]:443", family: "6", err: errors.New("secret.fixture query.private.example 192.0.2.99 provider.fixture"), result: "error"},
	} {
		line := dohDialObservationLine(context.Background(), dohPathCaller, test.address, test.err)
		for _, want := range []string{"observable=v1 path=caller", "attempted_family=" + test.family, "result=" + test.result, "resolver_owner=unknown resolver_scope=unknown resolver_outcome=unknown"} {
			if !strings.Contains(line, want) {
				t.Errorf("line omitted %q: %s", want, line)
			}
		}
		if test.result == "timeout" && !strings.HasSuffix(line, " err=i/o timeout") {
			t.Fatal("older monitors would lose their exact timeout signature")
		}
		if test.result == "refused" && !strings.HasSuffix(line, " err=connect: connection refused") {
			t.Fatal("older monitors would lose their exact refusal signature")
		}
		for _, private := range []string{"192.0.2.", "2001:db8", "private.example", "secret.fixture", "provider.fixture"} {
			if strings.Contains(line, private) {
				t.Fatal("dial observation retained private input")
			}
		}
	}
}

// Fork note: upstream drives this through tunDohFamilyFixture and
// Tun.DohCache() to reach the owned-tun path. This tree has no tun stack, so
// the same mapping is pinned on the settings shapes directly; dohTun is the
// only thing that can select the tun owner.
func TestDohDialObservationPathOwnership(t *testing.T) {
	var nilSettings *DohSettings
	if path := dohRemoteDialPath(nilSettings); path != dohPathUnknown {
		t.Fatalf("nil settings path=%s", path)
	}
	settings := DefaultDohSettings()
	if path := dohRemoteDialPath(settings); path != dohPathHost {
		t.Fatalf("default path=%s", path)
	}
	settings.ProxySettings = &ProxySettings{Network: "tcp", Address: "synthetic.proxy:1080"}
	if path := dohRemoteDialPath(settings); path != dohPathCaller {
		t.Fatalf("proxy path=%s", path)
	}
	settings.DialContextSettings = &DialContextSettings{}
	if path := dohRemoteDialPath(settings); path != dohPathCaller {
		t.Fatalf("custom path=%s", path)
	}
	settings.DialContextSettings.dohTun = true
	if path := dohRemoteDialPath(settings); path != dohPathTun {
		t.Fatalf("owned tun path=%s", path)
	}
	ctx, observation := newDohResolverObservation(context.Background(), dohPathTun, dohScopeAddress)
	observation.outcome.Store(uint32(dohOutcomeAnswer))
	line := dohDialObservationLine(ctx, dohPathHost, "192.0.2.55:443", nil)
	if !strings.Contains(line, "path=host") || !strings.Contains(line, "resolver_owner=tun") {
		t.Fatalf("local fallback was confused with remote ownership: %s", line)
	}
}

// Fixed counters count every terminal call while emission is throttled. A
// returned answer remains an answer when cancellation races its return.
func TestDohResolverObservationCounterAndTerminalKinds(t *testing.T) {
	var counter dohResolverCounter
	now := time.Unix(1000, 0)
	for outcome := dohOutcomeAnswer; outcome < dohOutcomeCount; outcome++ {
		counts, emit := counter.record(outcome, now)
		if counts[outcome] != 1 || emit != (outcome == dohOutcomeAnswer) {
			t.Fatalf("counter/emission changed for %s: %v %t", outcome, counts, emit)
		}
	}
	counts, emit := counter.record(dohOutcomeAnswer, now.Add(controlDialLogInterval))
	if !emit || counts[dohOutcomeAnswer] != 2 || counts[dohOutcomeFailed] != 1 || counts[dohOutcomeStale] != 1 {
		t.Fatalf("cumulative snapshot lost suppressed outcomes: %v %t", counts, emit)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, expire := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer expire()
	for _, test := range []struct {
		ctx           context.Context
		answered      bool
		authoritative bool
		stale         bool
		want          dohResolverOutcome
	}{
		{ctx: context.Background(), want: dohOutcomeFailed},
		{ctx: canceled, want: dohOutcomeCanceled},
		{ctx: expired, want: dohOutcomeTimeout},
		{ctx: canceled, answered: true, authoritative: true, want: dohOutcomeAnswer},
		{ctx: context.Background(), authoritative: true, want: dohOutcomeEmpty},
		{ctx: context.Background(), answered: true, authoritative: true, stale: true, want: dohOutcomeStale},
	} {
		if got := dohFinalResolverOutcome(test.ctx, test.answered, test.authoritative, test.stale); got != test.want {
			t.Errorf("terminal outcome=%s want=%s", got, test.want)
		}
	}
}

// Fork note: four upstream tests are not taken because nothing in this tree
// creates a dohResolverObservation: TestDohDetachedLosingHedgeRetainsFinalResolverOutcome,
// TestDohResolverObservationIncludesFinalFallbackAndStale,
// TestDohResolverObservationEmptyFailureAndCancellation and
// TestDohResolverObservationForwardAndExternalOneShot all read the observation
// out of the request context, which only the upstream resolver loop attaches.
// DohCache.Forward and DohCache.remoteClient/localClient are upstream-only too.
// The dial wrapper is the one piece of the file with no upstream test, so it is
// pinned here.

// The dial wrapper emits one finite line per allowed target and never changes
// the dial result.
func TestDohDialWrapperKeepsResultAndEmitsFiniteLine(t *testing.T) {
	log := newRecordingLogger()
	var dialed int
	dial := wrapDohDial(log, dohPathCaller, func(ctx context.Context, network, address string) (net.Conn, error) {
		dialed++
		return nil, syscall.ECONNREFUSED
	})
	if _, err := dial(context.Background(), "tcp4", "192.0.2.66:443"); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial error = %v", err)
	}
	if dialed != 1 {
		t.Fatalf("dialed=%d", dialed)
	}
	lines := log.linesWith("observable=v1")
	if len(lines) != 1 {
		t.Fatalf("emitted %d lines", len(lines))
	}
	for _, want := range []string{"path=caller", "attempted_family=4", "result=refused", "resolver_owner=unknown", "resolver_scope=unknown", "resolver_outcome=unknown", "err=connect: connection refused"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("line %q missing %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "192.0.2.66") {
		t.Fatalf("line retained the dial address: %s", lines[0])
	}
}
