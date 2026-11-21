package main

import (
	"context"
	"errors"
	"sync"
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

func newIPv6CheckHarness(t *testing.T) *ipv6CheckHarness {
	t.Helper()
	// the sweep selector reads the probe setting, so a disabling value in the
	// environment or the control state must not leak into these tests
	t.Setenv("URNETWORK_PROXY_IPV6_PROBE", "on")
	if globalControlState != nil {
		previous, found := globalControlState.get("proxy_ipv6_probe")
		if err := globalControlState.set("proxy_ipv6_probe", "on"); err != nil {
			t.Fatalf("could not enable the probe setting: %v", err)
		}
		t.Cleanup(func() {
			if found {
				globalControlState.set("proxy_ipv6_probe", previous)
			} else {
				globalControlState.clear("proxy_ipv6_probe")
			}
		})
	}
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
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
	h.ipv6OK = false
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	if !h.entryIPv6(settings).At.IsZero() {
		t.Fatal("a stage that measured nothing stored a result")
	}
}

func TestNoIPv6ProbeLeavesTheCheckUnchanged(t *testing.T) {
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
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
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())
	h.store.Check(ctx, settings, now)

	var runs atomic.Int32
	inStage := make(chan struct{})
	duplicate := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// a failing run must not leave the goroutines blocked in the stage
	t.Cleanup(releaseAll)
	h.store.ipv6Probe = func(_ context.Context, _ *connect.ProxySettings, previous proxyIPv6Result, at time.Time, _ proxyIPv6Dials) (proxyIPv6Result, bool) {
		switch runs.Add(1) {
		case 1:
			close(inStage)
		case 2:
			duplicate <- struct{}{}
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
	select {
	case <-done: // the second call returns without measuring
	case <-duplicate:
		t.Fatal("both calls entered the stage")
	case <-time.After(5 * time.Second):
		t.Fatal("the second call did not return")
	}
	releaseAll()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the first call did not finish")
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("the stage ran %d times at once, want 1", got)
	}
}

// When the UDP check is due too, its result is published before the stage runs.
func TestIPv6StageRunsAfterTheUDPResultIsPublished(t *testing.T) {
	h := newIPv6CheckHarness(t)
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

// An IPv6 refresh that finishes while a UDP check is running must not be undone
// when that UDP check is cancelled: the measurement and the cleared flag stay.
func TestCancelledUDPCheckKeepsTheIPv6StateOfARefreshThatFinished(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	// a completed entry whose UDP result is stale, with an IPv6 refresh in flight
	stale := now.Add(-2 * time.Hour)
	storeWithEntry(h.store, settings, proxyUDPOK, stale, proxyIPv6Result{})
	h.store.mu.Lock()
	h.store.entries[settings.Key()].ipv6Running = true
	h.store.mu.Unlock()

	probing := make(chan struct{})
	release := make(chan struct{})
	h.store.probe = func(ctx context.Context, _ *connect.ProxySettings) proxyUDPResult {
		close(probing)
		<-release
		return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.store.Check(ctx, settings, now); close(done) }()
	<-probing

	// the refresh finishes meanwhile: it updates the installed entry and clears the flag
	measured := v6Result(proxyIPv6No, "unknown", now, 24*time.Hour)
	h.store.mu.Lock()
	running := h.store.entries[settings.Key()]
	running.ipv6 = measured
	running.ipv6Running = false
	h.store.mu.Unlock()

	cancel()
	close(release)
	<-done

	h.store.mu.Lock()
	entry := h.store.entries[settings.Key()]
	h.store.mu.Unlock()
	if entry == nil {
		t.Fatal("the entry was lost")
	}
	if entry.ipv6 != measured {
		t.Fatalf("the rollback restored the old IPv6 result: %+v", entry.ipv6)
	}
	if entry.ipv6Running {
		t.Fatal("the rollback restored a refresh flag nothing will clear")
	}
}

// A stage that stops on its own keeps the previous result and waits before the
// next sweep tick measures the identity again. Without the wait every tick holds
// a probe slot for the same identity.
// pinSyntheticStageClock keeps a test on the times it passes in.
func pinSyntheticStageClock(t *testing.T) {
	t.Helper()
	previous := ipv6CompletionTime
	ipv6CompletionTime = func(now time.Time) time.Time { return now }
	t.Cleanup(func() { ipv6CompletionTime = previous })
}

func TestIPv6StageThatStopsEarlyBacksOff(t *testing.T) {
	pinSyntheticStageClock(t)
	h := newIPv6CheckHarness(t)
	h.ipv6OK = false
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())

	h.store.Check(ctx, settings, now)
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("stage ran %d times, want 1", got)
	}
	if !h.entryIPv6(settings).At.IsZero() {
		t.Fatal("a stage that stopped early must keep the previous (empty) result")
	}
	h.store.mu.Lock()
	// the stamp is now plus the backoff plus however long the run took, so the
	// checks below allow a second for that
	dueSoon := h.store.entries[settings.Key()].ipv6DueLocked(now.Add(time.Minute))
	dueLater := h.store.entries[settings.Key()].ipv6DueLocked(now.Add(proxyIPv6StopBackoff + time.Second))
	h.store.mu.Unlock()
	if dueSoon || !dueLater {
		t.Fatalf("due after 1 minute = %v (want false), after the backoff = %v (want true)", dueSoon, dueLater)
	}
	h.store.Check(ctx, settings, now.Add(time.Minute))
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("stage ran again inside the backoff: %d runs", got)
	}
	h.store.Check(ctx, settings, now.Add(proxyIPv6StopBackoff+time.Second))
	if got := h.ipv6Runs.Load(); got != 2 {
		t.Fatalf("stage did not run after the backoff: %d runs", got)
	}
	// a measurement that completes clears the wait
	h.ipv6OK = true
	h.store.Check(ctx, settings, now.Add(2*proxyIPv6StopBackoff+2*time.Second))
	h.store.mu.Lock()
	retry := h.store.entries[settings.Key()].ipv6RetryAt
	h.store.mu.Unlock()
	if !retry.IsZero() || h.entryIPv6(settings).At.IsZero() {
		t.Fatalf("a completed measurement must clear the wait and store the result, retry=%v", retry)
	}
}

// The operator turns the probe off while the UDP check runs. Check read the
// setting before that, so the stage must read it again before it measures.
func TestIPv6StageDoesNotStartAfterTheProbeIsSwitchedOff(t *testing.T) {
	h := newIPv6CheckHarness(t)
	h.store.probe = func(context.Context, *connect.ProxySettings) proxyUDPResult {
		h.udpRuns.Add(1)
		if err := globalControlState.set("proxy_ipv6_probe", "off"); err != nil {
			t.Errorf("could not switch the probe off: %v", err)
		}
		return h.udpResult
	}
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	if got := h.udpRuns.Load(); got != 1 {
		t.Fatalf("UDP probe ran %d times, want 1", got)
	}
	if got := h.ipv6Runs.Load(); got != 0 {
		t.Fatalf("the stage started %d times after the probe was switched off", got)
	}
}

// Check decides that the stage is due before its UDP probe, so refreshIPv6
// decides again under the lock. A measurement that another call completed, or a
// wait set after the first decision, must not start a second one.
func TestIPv6RefreshRechecksTheDueStateUnderTheLock(t *testing.T) {
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	ctx := withIPv6Sweep(context.Background())

	// one completed measurement makes the result fresh
	h.store.Check(ctx, settings, now)
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("stage ran %d times, want 1", got)
	}
	h.store.refreshIPv6(ctx, settings, settings.Key(), now.Add(time.Minute))
	if got := h.ipv6Runs.Load(); got != 1 {
		t.Fatalf("a refresh of a fresh result started the stage again: %d runs", got)
	}

	// a stage that stopped early sets a wait, and a refresh decided earlier honors it
	h2 := newIPv6CheckHarness(t)
	h2.ipv6OK = false
	h2.store.Check(ctx, settings, now)
	h2.store.refreshIPv6(ctx, settings, settings.Key(), now.Add(time.Minute))
	if got := h2.ipv6Runs.Load(); got != 1 {
		t.Fatalf("a refresh inside the wait started the stage: %d runs", got)
	}
	h2.store.refreshIPv6(ctx, settings, settings.Key(), now.Add(proxyIPv6StopBackoff+time.Second))
	if got := h2.ipv6Runs.Load(); got != 2 {
		t.Fatalf("a refresh after the wait did not start the stage: %d runs", got)
	}
}

// A stage the sweep's tick deadline cut short measured nothing. It must not stay
// due on every tick, or the IPv6-due order keeps it at the front of the batch
// and the other due identities are never dispatched.
func TestIPv6StageCutShortByTheTickDefersTheRetry(t *testing.T) {
	pinSyntheticStageClock(t)
	h := newIPv6CheckHarness(t)
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	h.store.mu.Lock()
	h.store.entries[settings.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPOK}, at: now, completedAt: now}
	h.store.mu.Unlock()

	// the tick deadline fires before the stage can start
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.store.refreshIPv6(ctx, settings, settings.Key(), now)

	h.store.mu.Lock()
	entry := h.store.entries[settings.Key()]
	retry := entry.ipv6RetryAt
	dueNextTick := entry.ipv6DueLocked(now.Add(time.Second))
	dueAfter := entry.ipv6DueLocked(now.Add(proxyIPv6CutShortRetry + time.Second))
	h.store.mu.Unlock()
	if got := h.ipv6Runs.Load(); got != 0 {
		t.Fatalf("the stage ran with a cancelled context: %d runs", got)
	}
	if dueNextTick {
		t.Fatal("a cut-short stage left the identity due on the next tick")
	}
	if !dueAfter {
		t.Fatalf("the identity is not due after the short deferral (retry %v, want about %v)", retry, now.Add(proxyIPv6CutShortRetry))
	}
}

// The retry backoff runs from when the stage finished, not from the sweep time
// that was passed in, so time spent waiting for a slot and probing does not eat
// into it.
func TestIPv6StageRetryBackoffStartsAtCompletion(t *testing.T) {
	previous := ipv6CompletionTime
	// the batch took five minutes to reach this refresh
	ipv6CompletionTime = func(now time.Time) time.Time { return now.Add(5 * time.Minute) }
	t.Cleanup(func() { ipv6CompletionTime = previous })

	h := newIPv6CheckHarness(t)
	h.ipv6OK = false
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

	h.store.Check(withIPv6Sweep(context.Background()), settings, now)
	h.store.mu.Lock()
	retry := h.store.entries[settings.Key()].ipv6RetryAt
	h.store.mu.Unlock()
	want := now.Add(5*time.Minute + proxyIPv6StopBackoff)
	if !retry.Equal(want) {
		t.Fatalf("retry at %v, want %v (backoff measured from completion)", retry, want)
	}
}

// A parent deadline that fires after the stage measured something must not throw
// the measurement away: publish it and drop the retry, or the identity waits out
// a backoff for a result it already has.
func TestIPv6StagePublishesAPartialResultWhenTheParentExpires(t *testing.T) {
	pinSyntheticStageClock(t)
	h := newIPv6CheckHarness(t)
	h.ipv6OK = true
	settings := ipv6TestSettings()
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	h.store.mu.Lock()
	h.store.entries[settings.Key()] = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPOK}, at: now, completedAt: now}
	h.store.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// the stage measures, then the parent ends before refreshIPv6 returns
	measured := h.store.ipv6Probe
	h.store.ipv6Probe = func(ctx context.Context, settings *connect.ProxySettings, previous proxyIPv6Result, now time.Time, dials proxyIPv6Dials) (proxyIPv6Result, bool) {
		result, ok := measured(ctx, settings, previous, now, dials)
		cancel()
		return result, ok
	}
	h.store.refreshIPv6(ctx, settings, settings.Key(), now)

	h.store.mu.Lock()
	entry := h.store.entries[settings.Key()]
	at := entry.ipv6.At
	retry := entry.ipv6RetryAt
	h.store.mu.Unlock()
	if at.IsZero() {
		t.Fatal("a measurement was thrown away because the parent context ended after it")
	}
	if !retry.IsZero() {
		t.Fatalf("a published measurement still carries a retry stamp: %v", retry)
	}
}
