package connect

import (
	"strings"
	"testing"
	"time"
)

// Tests for human-readable extender activation logging and attempt counters.

func waitForLogLines(t *testing.T, log *testActivateLogger, sub string, minCount int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		lines := log.linesContaining(sub)
		if len(lines) >= minCount {
			return lines
		}
		time.Sleep(5 * time.Millisecond)
	}
	return log.linesContaining(sub)
}

func TestExtenderActivatorCountersAdvance(t *testing.T) {
	log := &testActivateLogger{}
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(4, "the platform did not answer in time")
		},
		func(settings *ExtenderActivatorSettings) {
			settings.Log = log
			settings.ApiUrlV6 = ""
		},
	)

	version, _ := fixture.activator.ChangeMonitor().Get()
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitOutcome(version)

	counters := fixture.activator.FamilyCounters(4)
	if counters.Attempts != 1 {
		t.Fatalf("attempts = %d, expected 1", counters.Attempts)
	}
	if counters.Worked != 0 {
		t.Fatalf("worked = %d, expected 0", counters.Worked)
	}
	if counters.Failed != 1 {
		t.Fatalf("failed = %d, expected 1", counters.Failed)
	}
	if counters.ConsecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures = %d, expected 1", counters.ConsecutiveFailures)
	}
	if counters.LastFailure != fixture.clock.Now() {
		t.Fatalf("lastFailure = %v, expected %v", counters.LastFailure, fixture.clock.Now())
	}
	if counters.LastFailureReason != "the platform did not answer in time" {
		t.Fatalf("lastFailureReason = %q, expected %q", counters.LastFailureReason, "the platform did not answer in time")
	}

	// Now the operator accepts the activation.
	version, _ = fixture.activator.ChangeMonitor().Get()
	fixture.operator.setFamilyRefusal(4, "")
	fixture.step(10 * time.Minute)
	fixture.waitPost()
	fixture.waitActivation()
	fixture.waitOutcome(version)

	counters = fixture.activator.FamilyCounters(4)
	if counters.Attempts != 2 {
		t.Fatalf("attempts = %d, expected 2", counters.Attempts)
	}
	if counters.Worked != 1 {
		t.Fatalf("worked = %d, expected 1", counters.Worked)
	}
	if counters.Failed != 1 {
		t.Fatalf("failed = %d, expected 1", counters.Failed)
	}
	if counters.ConsecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d, expected 0", counters.ConsecutiveFailures)
	}
	if counters.LastSuccess != fixture.clock.Now() {
		t.Fatalf("lastSuccess = %v, expected %v", counters.LastSuccess, fixture.clock.Now())
	}

	// Verify try lines in the log.
	tryLines := waitForLogLines(t, log, "[extender] IPv4 activation try ", 2)
	if len(tryLines) != 2 {
		t.Fatalf("tryLines = %v, expected 2 lines", tryLines)
	}
	if !strings.Contains(tryLines[0], "try 1 failed after") || !strings.Contains(tryLines[0], "the platform did not answer in time") {
		t.Errorf("try line 0 = %q", tryLines[0])
	}
	if !strings.Contains(tryLines[1], "try 2 worked in") {
		t.Errorf("try line 1 = %q", tryLines[1])
	}
}

func TestExtenderActivatorPeriodicLineCarriesDetails(t *testing.T) {
	log := &testActivateLogger{}
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(4, "the platform did not answer in time")
		},
		func(settings *ExtenderActivatorSettings) {
			settings.Log = log
			settings.ApiUrlV6 = ""
		},
	)

	version, _ := fixture.activator.ChangeMonitor().Get()
	t0 := fixture.clock.Now()
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitOutcome(version)

	// In pass 1 (at t0), attempt 1 failed. Next try is held 10m to t0 + 10m.
	expectedNextTime := formatNextTryTime(t0.Add(10*time.Minute), t0)
	periodicLines := waitForLogLines(t, log, "IPv4 not activated:", 1)
	if len(periodicLines) != 1 {
		t.Fatalf("periodicLines = %v, expected 1 line", periodicLines)
	}
	expectedFirst := "IPv4 not activated: 1 tries, 0 worked, 1 failed; next try in 10m at " + expectedNextTime + "; last failure 0s ago: the platform did not answer in time\n"
	if !strings.Contains(periodicLines[0], expectedFirst) {
		t.Fatalf("periodic line 1 = %q, expected %q", periodicLines[0], expectedFirst)
	}

	// Advance clock by 6 minutes. 6m is outside the 5 minute window, so the pass should log.
	// Next try is still at t0 + 10m, which is 4m away (10m - 6m).
	// Last failure was at t0, which is 6m ago.
	t1 := t0.Add(6 * time.Minute)
	expectedNextTime1 := formatNextTryTime(t0.Add(10*time.Minute), t1)
	fixture.step(6 * time.Minute)

	periodicLines = waitForLogLines(t, log, "IPv4 not activated:", 2)
	if len(periodicLines) != 2 {
		t.Fatalf("periodicLines = %v, expected 2 lines", periodicLines)
	}
	expectedSecond := "IPv4 not activated: 1 tries, 0 worked, 1 failed; next try in 4m at " + expectedNextTime1 + "; last failure 6m ago: the platform did not answer in time\n"
	if !strings.Contains(periodicLines[1], expectedSecond) {
		t.Fatalf("periodic line 2 = %q, expected %q", periodicLines[1], expectedSecond)
	}
}

func TestExtenderActivatorPeriodicLineFiveMinuteWindow(t *testing.T) {
	log := &testActivateLogger{}
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(4, "the platform did not answer in time")
		},
		func(settings *ExtenderActivatorSettings) {
			settings.Log = log
			settings.ApiUrlV6 = ""
		},
	)

	version, _ := fixture.activator.ChangeMonitor().Get()
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitOutcome(version)

	periodicLines := waitForLogLines(t, log, "IPv4 not activated:", 1)
	if len(periodicLines) != 1 {
		t.Fatalf("initial periodicLines count = %d, expected 1", len(periodicLines))
	}

	// Second pass inside the 5 minute window (at 2 minutes): prints no periodic line.
	fixture.step(2 * time.Minute)
	// Give loop a moment to finish pass.
	time.Sleep(50 * time.Millisecond)
	periodicLines = log.linesContaining("IPv4 not activated:")
	if len(periodicLines) != 1 {
		t.Fatalf("periodicLines count after 2m = %d, expected 1 (no new periodic line inside 5m window)", len(periodicLines))
	}

	// Another pass at 3 minutes (total 3m from start): still inside 5 minute window.
	fixture.step(1 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	periodicLines = log.linesContaining("IPv4 not activated:")
	if len(periodicLines) != 1 {
		t.Fatalf("periodicLines count after 3m = %d, expected 1", len(periodicLines))
	}

	// Now step by 3 more minutes (total 6m from start >= 5m): prints periodic line.
	fixture.step(3 * time.Minute)
	periodicLines = waitForLogLines(t, log, "IPv4 not activated:", 2)
	if len(periodicLines) != 2 {
		t.Fatalf("periodicLines count after 6m = %d, expected 2", len(periodicLines))
	}
}

func TestExtenderActivatorActivationLineOncePerTransition(t *testing.T) {
	log := &testActivateLogger{}
	fixture := newTestActivatorFixture(
		t,
		func(settings *ExtenderActivatorSettings) {
			settings.Log = log
			settings.ApiUrlV6 = ""
		},
	)

	fixture.waitPass()
	fixture.waitPost()
	fixture.waitActivation()

	// Line 3 prints once when transitioning to activated.
	activatedLines := waitForLogLines(t, log, "[extender] IPv4 is now activated on try ", 1)
	if len(activatedLines) != 1 {
		t.Fatalf("activatedLines = %v, expected 1 line", activatedLines)
	}
	expected := "[extender] IPv4 is now activated on try 1. The platform can reach this host over IPv4.\n"
	if !strings.Contains(activatedLines[0], expected) {
		t.Fatalf("activatedLine = %q, expected %q", activatedLines[0], expected)
	}

	// Step passes after it: activation line does NOT print again.
	fixture.step(1 * time.Minute)
	fixture.step(2 * time.Minute)
	fixture.step(5 * time.Minute)
	time.Sleep(50 * time.Millisecond)

	activatedLines = log.linesContaining("[extender] IPv4 is now activated on try ")
	if len(activatedLines) != 1 {
		t.Fatalf("activatedLines count after subsequent passes = %d, expected 1", len(activatedLines))
	}

	// Now transition away from activated via refusal.
	fixture.operator.setFamilyRefusal(4, "temporary failure")
	status := fixture.forceActivation()
	// the record the family holds is valid for a day yet, and a refused renewal does
	// not clear it: the family stays activated and says what the renewal met
	if !status.Family(4).Activated || !status.Family(4).LastRefused {
		t.Fatal("v4 status expected activated with the refusal after a refused renewal")
	}

	// Now transition back to activated, once the refused renewal's hold is over.
	fixture.clock.advance(10 * time.Minute)
	fixture.operator.setFamilyRefusal(4, "")
	status = fixture.forceActivation()
	if !status.Family(4).Activated {
		t.Fatal("v4 status expected activated")
	}

	// The refused renewal never left the activated state, so there is no second
	// transition to print: the line stays at one.
	activatedLines = log.linesContaining("[extender] IPv4 is now activated on try ")
	if len(activatedLines) != 1 {
		t.Fatalf("activatedLines count after re-activation = %d, expected 1", len(activatedLines))
	}
}

func TestExtenderActivatorFirstFailureHintPrintsOnce(t *testing.T) {
	log := &testActivateLogger{}
	fixture := newTestActivatorFixtureWithOperator(
		t,
		func(operator *testActivateOperator) {
			operator.setFamilyRefusal(4, "the platform did not answer in time")
			operator.setFamilyRefusal(6, "the platform did not answer in time")
		},
		func(settings *ExtenderActivatorSettings) {
			settings.Log = log
		},
	)

	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()

	// First failure hint prints for IPv4 and IPv6.
	hintLinesV4 := waitForLogLines(t, log, "[extender] IPv4 failed for the first time.", 1)
	if len(hintLinesV4) != 1 {
		t.Fatalf("hintLinesV4 = %v, expected 1 line", hintLinesV4)
	}
	expectedHintV4 := "[extender] IPv4 failed for the first time. Check that inbound TCP 443 reaches this host and that it has a public IPv4 address.\n"
	if !strings.Contains(hintLinesV4[0], expectedHintV4) {
		t.Fatalf("hint line v4 = %q, expected %q", hintLinesV4[0], expectedHintV4)
	}

	hintLinesV6 := waitForLogLines(t, log, "[extender] IPv6 failed for the first time.", 1)
	if len(hintLinesV6) != 1 {
		t.Fatalf("hintLinesV6 = %v, expected 1 line", hintLinesV6)
	}
	expectedHintV6 := "[extender] IPv6 failed for the first time. Check that inbound TCP 443 reaches this host and that it has a public IPv6 address.\n"
	if !strings.Contains(hintLinesV6[0], expectedHintV6) {
		t.Fatalf("hint line v6 = %q, expected %q", hintLinesV6[0], expectedHintV6)
	}

	// Advance clock by 10 minutes to trigger second failure.
	fixture.step(10 * time.Minute)
	fixture.waitPost()
	fixture.waitPost()

	// Verify try lines show try 2 failed.
	v4TryLines := waitForLogLines(t, log, "[extender] IPv4 activation try ", 2)
	if len(v4TryLines) != 2 {
		t.Fatalf("v4TryLines count = %d, expected 2", len(v4TryLines))
	}

	// Hint line must NOT be printed again!
	hintLinesV4 = log.linesContaining("[extender] IPv4 failed for the first time.")
	if len(hintLinesV4) != 1 {
		t.Fatalf("hintLinesV4 count after second failure = %d, expected 1", len(hintLinesV4))
	}
	hintLinesV6 = log.linesContaining("[extender] IPv6 failed for the first time.")
	if len(hintLinesV6) != 1 {
		t.Fatalf("hintLinesV6 count after second failure = %d, expected 1", len(hintLinesV6))
	}

	// Advance clock by 20 minutes to trigger third failure.
	fixture.step(20 * time.Minute)
	fixture.waitPost()
	fixture.waitPost()

	// Verify try lines show try 3 failed.
	v4TryLines = waitForLogLines(t, log, "[extender] IPv4 activation try ", 3)
	if len(v4TryLines) != 3 {
		t.Fatalf("v4TryLines count = %d, expected 3", len(v4TryLines))
	}

	// Still only 1 hint line each!
	hintLinesV4 = log.linesContaining("[extender] IPv4 failed for the first time.")
	if len(hintLinesV4) != 1 {
		t.Fatalf("hintLinesV4 count after third failure = %d, expected 1", len(hintLinesV4))
	}
	hintLinesV6 = log.linesContaining("[extender] IPv6 failed for the first time.")
	if len(hintLinesV6) != 1 {
		t.Fatalf("hintLinesV6 count after third failure = %d, expected 1", len(hintLinesV6))
	}
}
