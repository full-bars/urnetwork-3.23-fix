package main

import (
	"context"
	"testing"
	"time"
)

// T1: Pin package-level timing intervals before the supervisor refactor.
func TestSupervisorParityConstants(t *testing.T) {
	durationChecks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"pressureSampleInterval", pressureSampleInterval, 30 * time.Second},
		{"gcSubtickInterval", gcSubtickInterval, 10 * time.Second},
		{"reloadSlotTimeout", reloadSlotTimeout, 10 * time.Minute},
		{"reloadHardLimit", reloadHardLimit, 20 * time.Minute},
		{"reloadWatchdogInterval", reloadWatchdogInterval, 30 * time.Second},
		{"reloadWatchdogReFire", reloadWatchdogReFire, 5 * time.Minute},
		{"loopBackoffBase", loopBackoffBase, 1 * time.Second},
		{"loopBackoffMax", loopBackoffMax, 5 * time.Minute},
		{"loopHealthyRun", loopHealthyRun, 10 * time.Minute},
	}
	for _, tc := range durationChecks {
		if tc.got != tc.want {
			t.Errorf("supervisor parity: %s changed; if intentional update docs/design/self-healing-supervisor.md section 4.4", tc.name)
		}
	}
}

// T3: when self-heal is off the real pressure loop republishes zero on every
// tick, so consumers see currentPressure() == 0 even though a high score was
// live (the state an earlier pressure episode leaves behind). This drives
// runPressureMonitor itself with a millisecond sample interval; the waits poll
// a condition under a generous deadline, so the outcome does not depend on timing.
func TestSelfHealOffPublishesZeroPressure(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	withTempHome(t) // the loop clears pressure_status in the home directory
	// leave the GC knob alone: the loop restores GOGC when it exits
	t.Setenv(adaptiveGCDisableEnv, "off")

	origInterval := pressureSampleInterval
	pressureSampleInterval = 5 * time.Millisecond
	t.Cleanup(func() { pressureSampleInterval = origInterval })

	origPressure := currentPressure()
	origPressureNoCPU := currentPressureNoCPU()
	t.Cleanup(func() {
		setPressure(origPressure)
		setPressureNoCPU(origPressureNoCPU)
		applyPressureMemoryBudget(0)
	})

	if resolveSelfHealEnabled(false) {
		t.Fatalf("resolveSelfHealEnabled(false) = true, want false")
	}

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}

	setPressure(0.95)
	setPressureNoCPU(0.95)
	if scaledProbeConcurrency(currentPressure()) >= proxyProbeConcurrency {
		t.Fatalf("pre-condition probe concurrency = %d, expected throttled below max %d",
			scaledProbeConcurrency(currentPressure()), proxyProbeConcurrency)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPressureMonitor(ctx, false)
	}()
	stopLoop := func() {
		cancel()
		<-done
	}
	defer stopLoop()

	waitFor("the loop to publish zero", func() bool {
		return currentPressure() == 0 && currentPressureNoCPU() == 0
	})
	if got := scaledProbeConcurrency(currentPressure()); got != proxyProbeConcurrency {
		t.Fatalf("scaledProbeConcurrency(0) = %d, want max %d", got, proxyProbeConcurrency)
	}
	if got := reaperStaleThreshold(currentPressure()); got != reaperStaleCalm {
		t.Fatalf("reaperStaleThreshold(0) = %v, want baseline %v", got, reaperStaleCalm)
	}

	// it keeps zeroing: a score that reappears while the loop runs is cleared
	// again on a later tick, not just once at startup
	setPressure(0.95)
	setPressureNoCPU(0.95)
	waitFor("the loop to publish zero again", func() bool {
		return currentPressure() == 0 && currentPressureNoCPU() == 0
	})
}
