package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// ipv6CheckHarness builds a check store whose UDP probe and IPv6 stage are
// counters, so the integration of the two can be tested without a network.
type ipv6CheckHarness struct {
	store     *proxyUDPCheckStore
	udpRuns   atomic.Int32
	ipv6Runs  atomic.Int32
	udpResult proxyUDPResult
	ipv6Next  proxyIPv6Result
	ipv6OK    bool
}

func newIPv6CheckHarness() *ipv6CheckHarness {
	h := &ipv6CheckHarness{
		udpResult: proxyUDPResult{Status: proxyUDPOK, TCPExit: "198.51.100.7", TCPExitObserved: true},
		ipv6OK:    true,
	}
	h.store = newProxyUDPCheckStore(func(context.Context, *connect.ProxySettings) proxyUDPResult {
		h.udpRuns.Add(1)
		return h.udpResult
	})
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, now time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		h.ipv6Runs.Add(1)
		if !h.ipv6OK {
			return previous, false
		}
		return previous.measured(now, proxyIPv6No, "unknown", "", ""), true
	}
	return h
}

func ipv6TestSettings() *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: "192.0.2.1:1080"}
}

func (h *ipv6CheckHarness) entryIPv6(settings *connect.ProxySettings) proxyIPv6Result {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	entry := h.store.entries[settings.Key()]
	if entry == nil {
		return proxyIPv6Result{}
	}
	return entry.ipv6
}

func TestIPv6StageRunsOnlyInSweepChecks(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	h.store.Check(context.Background(), settings, now)
	if got := h.ipv6Runs.Load(); got != 0 {
		t.Fatalf("a check outside the sweep ran the IPv6 stage %d times", got)
	}
	if !h.entryIPv6(settings).At.IsZero() {
		t.Fatal("a check outside the sweep stored an IPv6 result")
	}
}

func TestIPv6StageRunsInSweepAndIsStored(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	if got := h.udpRuns.Load(); got != 1 {
		t.Fatalf("UDP probe ran %d times, want 1", got)
	}
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("IPv6 stage ran %d times, want 1", got)
	}
	stored := h.entryIPv6(settings)
	if stored.Status != proxyIPv6No || stored.Source != "measured" || stored.At != now {
		t.Fatalf("stored IPv6 result = %+v", stored)
	}
}

func TestIPv6StageNotDueDoesNotRunAgain(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())

	h.store.Check(ctx, settings, now)
	// five minutes later the UDP result is fresh and the IPv6 result is not due
	h.store.Check(ctx, settings, now.Add(5*time.Minute))
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("IPv6 stage ran %d times before it was due, want 1", got)
	}
	if got := h.udpRuns.Load(); got != 1 {
		t.Fatalf("UDP probe ran %d times while its result was fresh, want 1", got)
	}
}

func TestIPv6StageDueWithFreshUDPSkipsTheUDPProbe(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())

	h.store.Check(ctx, settings, now)
	// make the IPv6 result due again after ten minutes, while the UDP result
	// stays fresh for the lazy recheck window
	h.store.mu.Lock()
	h.store.entries[settings.Key()].ipv6.Interval = 10 * time.Minute
	h.store.mu.Unlock()
	h.store.Check(ctx, settings, now.Add(11*time.Minute))
	if got := h.ipv6Runs.Load(); got != 2 {
		t.Fatalf("IPv6 stage ran %d times, want 2", got)
	}
	if got := h.udpRuns.Load(); got != 1 {
		t.Fatalf("UDP probe ran %d times, a fresh UDP result must not be probed again", got)
	}
	if at := h.entryIPv6(settings).At; !at.Equal(now.Add(11 * time.Minute)) {
		t.Fatalf("IPv6 result time = %v", at)
	}
}

func TestIPv6StageRunsAfterAFailedUDPCheck(t *testing.T) {
	h := newIPv6CheckHarness()
	h.udpResult = proxyUDPResult{Status: proxyUDPUnknown, Err: errors.New("relay unreachable")}
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	result := h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	if result.Status != proxyUDPUnknown {
		t.Fatalf("UDP status = %v, want unknown", result.Status)
	}
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("IPv6 stage ran %d times after a failed UDP check, want 1", got)
	}
}

func TestIPv6StageWithoutAMeasurementKeepsThePreviousResult(t *testing.T) {
	h := newIPv6CheckHarness()
	h.ipv6OK = false
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	if !h.entryIPv6(settings).At.IsZero() {
		t.Fatal("a stage that measured nothing stored a result")
	}
}

func TestNoIPv6ProbeLeavesTheCheckUnchanged(t *testing.T) {
	h := newIPv6CheckHarness()
	h.store.ipv6Probe = nil
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())

	h.store.Check(ctx, settings, now)
	h.store.Check(ctx, settings, now.Add(time.Minute))
	if got := h.udpRuns.Load(); got != 1 {
		t.Fatalf("UDP probe ran %d times, want 1 (fresh result)", got)
	}
	if got := h.ipv6Runs.Load(); got != 0 {
		t.Fatalf("IPv6 stage ran %d times without a probe", got)
	}
}

func TestSweepSelectsAProxyWhoseIPv6ResultIsDue(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	fresh := udpProbeCandidate{key: "a", online: true, status: proxyUDPOK, lastChecked: now.Add(-time.Minute)}
	if got := selectUDPProbeBatch(now, []udpProbeCandidate{fresh}, 24*time.Hour, time.Minute, nil); len(got) != 0 {
		t.Fatalf("a fresh proxy was selected: %v", got)
	}
	fresh.ipv6Due = true
	if got := selectUDPProbeBatch(now, []udpProbeCandidate{fresh}, 24*time.Hour, time.Minute, nil); len(got) != 1 {
		t.Fatalf("a proxy with a due IPv6 result was not selected: %v", got)
	}
}

// While the IPv6 stage measures a proxy whose UDP result is fresh, the
// completed UDP result stays readable and nobody waits for the stage.
func TestIPv6RefreshKeepsTheUDPResultAvailable(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())
	h.store.Check(ctx, settings, now)

	inStage := make(chan struct{})
	release := make(chan struct{})
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		close(inStage)
		<-release
		return previous.measured(at, proxyIPv6No, "unknown", "", ""), true
	}
	h.store.mu.Lock()
	h.store.entries[settings.Key()].ipv6.Interval = 10 * time.Minute
	h.store.mu.Unlock()
	later := now.Add(11 * time.Minute)
	finished := make(chan struct{})
	go func() {
		h.store.Check(ctx, settings, later)
		close(finished)
	}()
	<-inStage

	// a foreground check returns the completed result at once, with a tight deadline
	quick, cancel := context.WithTimeout(context.Background(), time.Second)
	result := h.store.Check(quick, settings, later)
	cancel()
	if result.Status != proxyUDPOK || result.Err != nil {
		t.Fatalf("a foreground check during the stage returned %+v", result)
	}
	h.store.mu.Lock()
	entry := h.store.entries[settings.Key()]
	completed := entry != nil && entry.done == nil && entry.result.Status == proxyUDPOK
	h.store.mu.Unlock()
	if !completed {
		t.Fatal("the stage replaced the completed UDP entry")
	}
	close(release)
	<-finished
	if h.udpRuns.Load() != 1 {
		t.Fatalf("UDP probe ran %d times, want 1", h.udpRuns.Load())
	}
}

// Two sweep calls for one proxy measure IPv6 once.
func TestIPv6RefreshDoesNotRunTwiceAtOnce(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())
	h.store.Check(ctx, settings, now)

	var runs atomic.Int32
	inStage := make(chan struct{})
	release := make(chan struct{})
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		if runs.Add(1) == 1 {
			close(inStage)
		}
		<-release
		return previous.measured(at, proxyIPv6No, "unknown", "", ""), true
	}
	h.store.mu.Lock()
	h.store.entries[settings.Key()].ipv6.Interval = 10 * time.Minute
	h.store.mu.Unlock()
	later := now.Add(11 * time.Minute)
	done := make(chan struct{}, 2)
	go func() { h.store.Check(ctx, settings, later); done <- struct{}{} }()
	<-inStage
	go func() { h.store.Check(ctx, settings, later); done <- struct{}{} }()
	<-done // the second call returns without measuring
	close(release)
	<-done
	if got := runs.Load(); got != 1 {
		t.Fatalf("the stage ran %d times at once, want 1", got)
	}
}

// When the UDP check is due too, its result is published before the stage runs.
func TestIPv6StageRunsAfterTheUDPResultIsPublished(t *testing.T) {
	h := newIPv6CheckHarness()
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	published := false
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		h.store.mu.Lock()
		entry := h.store.entries[settings.Key()]
		published = entry != nil && entry.done == nil && entry.result.Status == proxyUDPOK
		h.store.mu.Unlock()
		return previous.measured(at, proxyIPv6No, "unknown", "", ""), true
	}
	h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	if !published {
		t.Fatal("the stage ran before the UDP result was published")
	}
}
