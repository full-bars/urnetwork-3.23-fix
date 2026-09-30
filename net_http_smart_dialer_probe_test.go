package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// probeStrategy builds a strategy holding exactly the given dialers, enough
// for the probe logic (it only reads the dialer set and the settings).
func probeStrategy(dialers ...*clientDialer) *ClientStrategy {
	set := map[*clientDialer]bool{}
	for _, dialer := range dialers {
		set[dialer] = true
	}
	return &ClientStrategy{
		ctx:      context.Background(),
		settings: DefaultClientStrategySettings(),
		dialers:  set,
	}
}

// withSmartDialer runs the test with the smart dialer set to on, restoring the
// previous setting afterwards (it is process-global).
func withSmartDialer(t *testing.T, on bool) {
	t.Helper()
	previous := SetSmartDialer(on)
	t.Cleanup(func() { SetSmartDialer(previous) })
}

// fixedProbe reports a fresh connection that took the given time per dialer
// description, or an error for a description mapped to a nil duration.
func fixedProbe(costs map[string]time.Duration, calls *atomic.Int32) dialProbe {
	return func(ctx context.Context, dialer *clientDialer) (bool, time.Duration, error) {
		if calls != nil {
			calls.Add(1)
		}
		cost, ok := costs[dialer.description]
		if !ok {
			return false, 0, errors.New("blocked")
		}
		return true, cost, nil
	}
}

// The reason the probe exists. On a healthy network fragment (priority 0)
// always succeeds first, so normal is never dialed and never measured, and the
// measured-cost preference has nothing to compare fragment against. After a
// probe normal has real samples and, being faster, leads the order.
func TestProbeMeasuresATransportThatIsNeverDialedAndLetsItWin(t *testing.T) {
	withSmartDialer(t, true)

	fragment := testDialer("fragment", 0, 0.25, 10, 600*time.Millisecond, 0)
	normal := &clientDialer{description: "normal", priority: 25, minimumWeight: 0.5}
	strategy := probeStrategy(fragment, normal)

	if got := dialerOrder([]*clientDialer{fragment, normal}); !equalOrder(got, []string{"fragment", "normal"}) {
		t.Fatalf("before the probe order = %v, want [fragment normal] (normal is unmeasured)", got)
	}

	probed := strategy.probeDialers(context.Background(), fixedProbe(map[string]time.Duration{
		"normal":   80 * time.Millisecond,
		"fragment": 600 * time.Millisecond,
	}, nil))
	if probed != 2 {
		t.Fatalf("probed %d dialers, want 2 (normal is unmeasured, so every transport is probed on one basis)", probed)
	}

	if _, samples := normal.measuredLatency(); samples < smartDialerMinSamples {
		t.Fatalf("normal has %d samples after the probe, want at least %d", samples, smartDialerMinSamples)
	}
	if got := dialerOrder([]*clientDialer{fragment, normal}); !equalOrder(got, []string{"normal", "fragment"}) {
		t.Fatalf("after the probe order = %v, want [normal fragment]", got)
	}
}

// Off means off: with the smart dialer disabled nothing is dialed, so the
// legacy path stays byte-identical.
func TestProbeDoesNothingWhenSmartDialerIsOff(t *testing.T) {
	withSmartDialer(t, false)

	normal := &clientDialer{description: "normal", priority: 25, minimumWeight: 0.5}
	var calls atomic.Int32
	probed := probeStrategy(normal).probeDialers(context.Background(), fixedProbe(map[string]time.Duration{
		"normal": time.Millisecond,
	}, &calls))

	if probed != 0 || calls.Load() != 0 {
		t.Fatalf("probed=%d calls=%d with the smart dialer off, want 0 and 0", probed, calls.Load())
	}
	if _, samples := normal.measuredLatency(); samples != 0 {
		t.Fatalf("normal has %d samples, want 0", samples)
	}
}

// A dialer with enough recent samples is left alone: the probe must not add
// load or churn for a transport that already has an answer.
func TestProbeSkipsADialerThatIsAlreadyMeasuredRecently(t *testing.T) {
	withSmartDialer(t, true)

	fragment := testDialer("fragment", 0, 0.25, smartDialerMinSamples, 600*time.Millisecond, 0)
	var calls atomic.Int32
	probed := probeStrategy(fragment).probeDialers(context.Background(), fixedProbe(map[string]time.Duration{
		"fragment": time.Millisecond,
	}, &calls))

	if probed != 0 || calls.Load() != 0 {
		t.Fatalf("probed=%d calls=%d for a recently measured dialer, want 0 and 0", probed, calls.Load())
	}
}

// Networks change. A measurement older than the refresh age is taken again so
// a transport that became slow (or fast) is noticed without a restart.
func TestProbeRefreshesAStaleMeasurement(t *testing.T) {
	withSmartDialer(t, true)

	fragment := testDialer("fragment", 0, 0.25, smartDialerMinSamples, 600*time.Millisecond, 0)
	fragment.mutex.Lock()
	fragment.connectObservedAt = time.Now().Add(-2 * smartDialerProbeRefreshAge)
	fragment.mutex.Unlock()

	var calls atomic.Int32
	probed := probeStrategy(fragment).probeDialers(context.Background(), fixedProbe(map[string]time.Duration{
		"fragment": 100 * time.Millisecond,
	}, &calls))

	if probed != 1 || calls.Load() < 1 {
		t.Fatalf("probed=%d calls=%d for a stale dialer, want 1 and at least 1", probed, calls.Load())
	}
	avg, _ := fragment.measuredLatency()
	if avg >= 600*time.Millisecond {
		t.Fatalf("stale average %v was not refreshed toward the new 100ms cost", avg)
	}
}

// A transport that cannot connect here must not gain samples from a probe. It
// takes the failure like a real dial would, so it cannot lead the order.
func TestProbeFailureIsRecordedAndProducesNoSample(t *testing.T) {
	withSmartDialer(t, true)

	fragment := testDialer("fragment", 0, 0.25, 10, 600*time.Millisecond, 0)
	normal := &clientDialer{description: "normal", priority: 25, minimumWeight: 0.5}
	probeStrategy(fragment, normal).probeDialers(context.Background(), fixedProbe(map[string]time.Duration{}, nil))

	if _, samples := normal.measuredLatency(); samples != 0 {
		t.Fatalf("a failed probe left %d samples on normal, want 0", samples)
	}
	if normal.Stats().errorCount == 0 {
		t.Fatalf("a failed probe was not recorded as an error on normal")
	}
	if normal.hasMeasuredLatency() {
		t.Fatalf("a dialer whose only attempts failed must not count as measured")
	}
	if got := dialerOrder([]*clientDialer{fragment, normal}); !equalOrder(got, []string{"fragment", "normal"}) {
		t.Fatalf("order = %v, want [fragment normal]: a blocked transport must not lead", got)
	}
}

// Cancellation stops the probe promptly and leaves no partial claims.
func TestProbeStopsWhenContextIsCanceled(t *testing.T) {
	withSmartDialer(t, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	normal := &clientDialer{description: "normal", priority: 25, minimumWeight: 0.5}
	var calls atomic.Int32
	probed := probeStrategy(normal).probeDialers(ctx, fixedProbe(map[string]time.Duration{
		"normal": time.Millisecond,
	}, &calls))

	if probed != 0 || calls.Load() != 0 {
		t.Fatalf("probed=%d calls=%d with a canceled context, want 0 and 0", probed, calls.Load())
	}
}

// The real wiring against a real server: every transport the default strategy
// builds (normal, fragment, reorder, fragment+reorder) gets fresh-connection
// samples, because each probe uses its own connection instead of a kept-alive
// one that would report reuse and count for nothing.
func TestProbeDialersMeasuresEveryDefaultTransportOverRealHttp(t *testing.T) {
	withSmartDialer(t, true)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	strategy := NewClientStrategyWithDefaults(ctx)

	probed := strategy.ProbeDialers(ctx, server.URL)
	if probed == 0 {
		t.Fatalf("ProbeDialers probed nothing against a healthy server")
	}
	if requests.Load() == 0 {
		t.Fatalf("the server saw no requests")
	}

	strategy.mutex.Lock()
	dialers := make([]*clientDialer, 0, len(strategy.dialers))
	for dialer := range strategy.dialers {
		dialers = append(dialers, dialer)
	}
	strategy.mutex.Unlock()

	for _, dialer := range dialers {
		if _, samples := dialer.measuredLatency(); samples < smartDialerMinSamples {
			t.Fatalf("%s has %d fresh-connection samples, want at least %d", dialer.description, samples, smartDialerMinSamples)
		}
		if !dialer.hasMeasuredLatency() {
			t.Fatalf("%s is not measured after the probe", dialer.description)
		}
	}
}

// A probe that fails must not demote a transport that has real successes. The
// leader's measurement goes stale because its live connection is long-lived,
// so it is refreshed; one failed GET to the api host at that moment must not
// drop it from the serial list or halve its weight while it still carries
// traffic.
func TestProbeFailureDoesNotDemoteALeaderThatHasRealSuccesses(t *testing.T) {
	withSmartDialer(t, true)

	normal := testDialer("normal", 25, 0.5, 10, 80*time.Millisecond, 0)
	normal.mutex.Lock()
	normal.connectObservedAt = time.Now().Add(-2 * smartDialerProbeRefreshAge)
	normal.mutex.Unlock()
	fragment := testDialer("fragment", 0, 0.25, 10, 300*time.Millisecond, 0)

	// every probe fails: the api host hiccuped
	probeStrategy(normal, fragment).probeDialers(context.Background(), fixedProbe(map[string]time.Duration{}, nil))

	if !normal.IsLastSuccess() {
		t.Fatalf("a failed refresh probe made the leading transport count as failing")
	}
	if !normal.hasMeasuredLatency() {
		t.Fatalf("a failed refresh probe made the leading transport unmeasured")
	}
	if normal.Stats().errorCount != 0 {
		t.Fatalf("a failed probe was recorded as an error on a transport with real successes")
	}
	if got := dialerOrder([]*clientDialer{fragment, normal}); !equalOrder(got, []string{"normal", "fragment"}) {
		t.Fatalf("order = %v, want [normal fragment]: one failed probe must not change the leader", got)
	}
}

// Probed samples are only comparable if they are taken together, against the
// same host, so when any transport needs a probe the leader is probed too.
func TestProbeProbesTheLeaderInTheSameRoundAsAnUnmeasuredTransport(t *testing.T) {
	withSmartDialer(t, true)

	fragment := testDialer("fragment", 0, 0.25, smartDialerMinSamples, 600*time.Millisecond, 0)
	normal := &clientDialer{description: "normal", priority: 25, minimumWeight: 0.5}

	probedFragment := false
	probe := func(ctx context.Context, dialer *clientDialer) (bool, time.Duration, error) {
		if dialer == fragment {
			probedFragment = true
		}
		return true, 100 * time.Millisecond, nil
	}
	probeStrategy(fragment, normal).probeDialers(context.Background(), probe)

	if !probedFragment {
		t.Fatalf("the measured leader was not probed alongside the unmeasured transport")
	}
}

// Turning the smart dialer off stops the probe at once, not at the end of the
// round.
func TestProbeStopsMidRoundWhenSmartDialerIsTurnedOff(t *testing.T) {
	withSmartDialer(t, true)

	a := &clientDialer{description: "a", priority: 0, minimumWeight: 0.25}
	b := &clientDialer{description: "b", priority: 25, minimumWeight: 0.5}
	var calls atomic.Int32
	probe := func(ctx context.Context, dialer *clientDialer) (bool, time.Duration, error) {
		calls.Add(1)
		SetSmartDialer(false)
		return true, time.Millisecond, nil
	}
	probeStrategy(a, b).probeDialers(context.Background(), probe)

	if got := calls.Load(); got != 1 {
		t.Fatalf("%d probe calls after the smart dialer was turned off, want 1", got)
	}
}

// With custom extenders configured the strategy only uses the extenders, so
// the plain transports would be probed for nothing.
func TestProbeIsSkippedWhileCustomExtendersAreConfigured(t *testing.T) {
	withSmartDialer(t, true)

	normal := &clientDialer{description: "normal", priority: 25, minimumWeight: 0.5}
	strategy := probeStrategy(normal)
	strategy.extenderIpSecrets = map[netip.Addr]string{netip.MustParseAddr("192.0.2.1"): "secret"}

	var calls atomic.Int32
	probed := strategy.probeDialers(context.Background(), fixedProbe(map[string]time.Duration{
		"normal": time.Millisecond,
	}, &calls))

	if probed != 0 || calls.Load() != 0 {
		t.Fatalf("probed=%d calls=%d with custom extenders, want 0 and 0", probed, calls.Load())
	}
}

// The probe must dial through the dialer's own TLS function: that is where a
// transport's fragmenting, reordering or proxy path lives, and cloning the
// transport must not lose it. The plain-http test above cannot show this
// because http never calls DialTLSContext.
func TestProbeDialsThroughTheDialersTlsFunctionOverHttps(t *testing.T) {
	withSmartDialer(t, true)

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var tlsDials atomic.Int32
	dialer := &clientDialer{
		description:   "counting",
		priority:      0,
		minimumWeight: 0.25,
		settings:      DefaultClientStrategySettings(),
		dialTlsContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			tlsDials.Add(1)
			d := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
			return d.DialContext(ctx, network, address)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if probed := probeStrategy(dialer).ProbeDialers(ctx, server.URL); probed != 1 {
		t.Fatalf("probed %d, want 1", probed)
	}

	if tlsDials.Load() < int32(smartDialerMinSamples) {
		t.Fatalf("the dialer's TLS function ran %d times, want at least %d: the probe bypassed it", tlsDials.Load(), smartDialerMinSamples)
	}
	if _, samples := dialer.measuredLatency(); samples < smartDialerMinSamples {
		t.Fatalf("%d samples over https, want at least %d", samples, smartDialerMinSamples)
	}
}
