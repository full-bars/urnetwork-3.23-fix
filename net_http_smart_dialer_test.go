package connect

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDialTimingReportsReuseAgainstRealServer pins the httptrace wiring
// against a real HTTP server: the first request establishes a fresh
// connection (reported as a sample), and a second request on the kept-alive
// connection reports reuse, so the call sites fold no dial-cost sample for
// it. This is the end-to-end half of the guarantee the unit tests above take
// on faith.
func TestDialTimingReportsReuseAgainstRealServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := server.Client()
	ctx := context.Background()

	request := func() (bool, time.Duration) {
		timing := &dialTiming{}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(timing.trace(ctx, req, time.Now()))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return timing.sample()
	}

	fresh1, establish1 := request()
	if !fresh1 || establish1 <= 0 {
		t.Fatalf("first request: fresh=%v establish=%v, want a fresh establishment", fresh1, establish1)
	}

	fresh2, _ := request()
	if fresh2 {
		t.Fatalf("second request over the kept-alive connection reported a fresh establishment; keep-alive reuse must not create dial-cost samples")
	}
}

// testDialer builds a dialer with a measured history: successes whose
// connection establishment cost the given duration, and optionally some
// failures. The successes are genuine establishments (observeConnect), which
// is what a measured history in these tests represents.
func testDialer(description string, priority int, minimumWeight float32, successes int, cost time.Duration, failures int) *clientDialer {
	dialer := &clientDialer{
		description:   description,
		priority:      priority,
		minimumWeight: minimumWeight,
	}
	ctx := context.Background()
	for i := 0; i < failures; i++ {
		dialer.Update(ctx, context.DeadlineExceeded, 0)
	}
	for i := 0; i < successes; i++ {
		dialer.Update(ctx, nil, cost)
		dialer.observeConnect(cost)
	}
	return dialer
}

func dialerOrder(dialers []*clientDialer) []string {
	order := []string{}
	for _, dialer := range orderDialersByMeasuredCost(dialers) {
		order = append(order, dialer.description)
	}
	return order
}

func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSmartDialerToggleReportsPrevious pins the on/off plumbing the provider
// drives through its control socket.
func TestSmartDialerToggleReportsPrevious(t *testing.T) {
	SetSmartDialer(false)
	t.Cleanup(func() { SetSmartDialer(false) })

	if previous := SetSmartDialer(true); previous {
		t.Fatal("first enable reported a previous value of true")
	}
	if !SmartDialerEnabled() {
		t.Fatal("did not report enabled after enabling")
	}
	if previous := SetSmartDialer(false); !previous {
		t.Fatal("disable did not report the previous value of true")
	}
	if SmartDialerEnabled() {
		t.Fatal("still reports enabled after disabling")
	}
}

// TestSmartDialerOffKeepsExistingWeights pins that with the setting off the
// weights are exactly the existing ones (the fixed-baseline scoring).
func TestSmartDialerOffKeepsExistingWeights(t *testing.T) {
	SetSmartDialer(false)
	t.Cleanup(func() { SetSmartDialer(false) })

	fast := testDialer("fast", 0, 0.25, 10, 200*time.Millisecond, 0)
	slow := testDialer("slow", 25, 0.25, 10, 6*time.Second, 0)

	strategy := &ClientStrategy{dialers: map[*clientDialer]bool{fast: true, slow: true}}
	weights := strategy.dialerWeights()

	if weights[fast] != fast.Weight() {
		t.Fatalf("off: fast weight %v, want the existing Weight() %v", weights[fast], fast.Weight())
	}
	if weights[slow] != slow.Weight() {
		t.Fatalf("off: slow weight %v, want the existing Weight() %v", weights[slow], slow.Weight())
	}
}

// TestSmartDialerOnScalesByMeasuredCost pins that with the setting on the
// weights come from measured cost relative to the fastest working dialer.
func TestSmartDialerOnScalesByMeasuredCost(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	fast := testDialer("fast", 0, 0.25, 10, 200*time.Millisecond, 0)
	slow := testDialer("slow", 25, 0.25, 10, 20*time.Second, 0)

	strategy := &ClientStrategy{dialers: map[*clientDialer]bool{fast: true, slow: true}}
	weights := strategy.dialerWeights()

	if weights[slow] >= weights[fast] {
		t.Fatalf("on: slow weight %v did not fall below the faster one %v", weights[slow], weights[fast])
	}
	if weights[slow] < slow.minimumWeight {
		t.Fatalf("on: slow weight %v dropped below the floor %v", weights[slow], slow.minimumWeight)
	}
}

// TestSmartDialerPrefersFasterMeasuredDialer pins the ordering: the measured
// faster transport is tried first and the fragmented one still follows.
func TestSmartDialerPrefersFasterMeasuredDialer(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	fragment := testDialer("fragment", 0, 0.25, 10, 6*time.Second, 0)
	normal := testDialer("normal", 25, 0.25, 10, time.Second, 0)
	unmeasured := testDialer("reorder", 100, 0.25, 0, 0, 0)

	got := dialerOrder([]*clientDialer{fragment, unmeasured, normal})
	if want := []string{"normal", "fragment", "reorder"}; !equalOrder(got, want) {
		t.Fatalf("order = %v, want %v (fastest measured first, the rest unmoved)", got, want)
	}
}

// TestSmartDialerNeedsEnoughSamples pins hysteresis: below the sample floor the
// existing order stands, so one cold or unlucky dial cannot reorder anything.
func TestSmartDialerNeedsEnoughSamples(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	fragment := testDialer("fragment", 0, 0.25, smartDialerMinSamples-1, 9*time.Second, 0)
	normal := testDialer("normal", 25, 0.25, smartDialerMinSamples-1, time.Second, 0)

	got := dialerOrder([]*clientDialer{fragment, normal})
	if want := []string{"fragment", "normal"}; !equalOrder(got, want) {
		t.Fatalf("below the sample floor: order = %v, want the existing order %v", got, want)
	}
}

// TestSmartDialerReusedConnectionDoesNotCountAsDialCost pins the measurement
// method: a request served from a reused keep-alive connection is a success
// but performed no dial, so it must not create a latency sample and must not
// qualify the dialer for measured-cost ordering. A transport whose real
// connects are slow must not look cheap because its reused requests answer
// fast.
func TestSmartDialerReusedConnectionDoesNotCountAsDialCost(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	dialer := &clientDialer{description: "fragment", priority: 0, minimumWeight: 0.25}
	ctx := context.Background()

	// Many reused-connection successes: fast request times, no dials.
	for i := 0; i < 10; i++ {
		dialer.Update(ctx, nil, 5*time.Millisecond)
	}
	if _, samples := dialer.measuredLatency(); samples != 0 {
		t.Fatalf("reused-connection successes created latency samples: %d", samples)
	}
	if dialer.hasMeasuredLatency() {
		t.Fatalf("reused-connection successes qualified the dialer for measured ordering without a single real dial")
	}

	// One genuine (slow) connection establishment is the only dial sample.
	dialer.Update(ctx, nil, 8*time.Second)
	dialer.observeConnect(8 * time.Second)
	avg, samples := dialer.measuredLatency()
	if samples != 1 {
		t.Fatalf("expected exactly one dial sample, got %d", samples)
	}
	if avg != 8*time.Second {
		t.Fatalf("average %v should be the genuine establishment time, not diluted by reused connections", avg)
	}
}

// TestSmartDialerFailingAfterMeasuringCannotLeadOrSetReference pins the
// health gate: a dialer that measured fast earlier but whose last attempt
// failed must not lead the measured order and must not set the fastest-working
// reference, because failures outrank latency.
func TestSmartDialerFailingAfterMeasuringCannotLeadOrSetReference(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	// A dialer that measured fast earlier (5+ samples at 10ms) and then
	// started failing: its last attempt errored, so it must not lead the
	// serial order and must not set the fastest reference.
	failedFast := testDialer("fragment", 0, 0.25, 10, 10*time.Millisecond, 0)
	failedFast.Update(context.Background(), errors.New("timeout"), 0)
	healthySlow := testDialer("normal", 0, 0.25, 10, 5*time.Second, 0)

	order := dialerOrder([]*clientDialer{healthySlow, failedFast})
	if order[0] != "normal" {
		t.Fatalf("dialer whose last attempt failed led the measured order: got %v first", order[0])
	}

	weights := map[*clientDialer]float32{}
	for _, dialer := range []*clientDialer{failedFast, healthySlow} {
		weights[dialer] = dialer.weightWithoutLatency()
	}
	applySmartDialerWeights(weights)

	if weights[healthySlow] < weights[failedFast] {
		t.Fatalf(
			"stale fast samples from a now-failing dialer reduced the healthy dialer's weight: healthy=%v failing=%v",
			weights[healthySlow], weights[failedFast],
		)
	}
}

// TestSmartDialerOnlyRefinesWorkingDialers pins the guardrail that matters on a
// censored network: a transport that has never succeeded here must not outrank
// one that works, however slow the working one is.
func TestSmartDialerOnlyRefinesWorkingDialers(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	blockedFast := testDialer("fragment", 0, 0.25, 0, 0, 20)
	workingSlow := testDialer("normal", 25, 0.25, 10, 5*time.Second, 0)

	weights := map[*clientDialer]float32{
		blockedFast: blockedFast.weightWithoutLatency(),
		workingSlow: workingSlow.weightWithoutLatency(),
	}
	applySmartDialerWeights(weights)

	if weights[workingSlow] < weights[blockedFast] {
		t.Fatalf(
			"working transport weight %v fell below the blocked one %v: a censored network would lose its working path",
			weights[workingSlow], weights[blockedFast],
		)
	}
}

// TestSmartDialerSoleWorkingDialerUnchanged pins that the comparison reference
// is the fastest dialer that works here, so a client with one working transport
// is not penalised for being the only option.
func TestSmartDialerSoleWorkingDialerUnchanged(t *testing.T) {
	SetSmartDialer(true)
	t.Cleanup(func() { SetSmartDialer(false) })

	only := testDialer("fragment", 0, 0.25, 10, 7*time.Second, 0)
	weights := map[*clientDialer]float32{only: only.weightWithoutLatency()}
	before := weights[only]

	applySmartDialerWeights(weights)

	if weights[only] != before {
		t.Fatalf("sole working dialer weight changed from %v to %v", before, weights[only])
	}
}

// TestWeightWithoutLatencyIgnoresCost pins that the base score used by the
// measured-cost preference carries no latency assumption of its own.
func TestWeightWithoutLatencyIgnoresCost(t *testing.T) {
	fast := testDialer("fast", 0, 0.25, 10, 100*time.Millisecond, 0)
	slow := testDialer("slow", 0, 0.25, 10, 30*time.Second, 0)

	if fast.weightWithoutLatency() != slow.weightWithoutLatency() {
		t.Fatalf(
			"same history, different cost: %v vs %v (the base score must not assume a latency baseline)",
			fast.weightWithoutLatency(), slow.weightWithoutLatency(),
		)
	}
}
