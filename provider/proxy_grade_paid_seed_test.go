package main

import (
	"context"
	"testing"
)

// The paid grader must rotate the sampled host block every tick. The pass
// counter used to advance only inside the URL fetch cycle, so a box with a
// --proxy_file and no URL sources re-dialed the SAME block on every sweep and
// a second "independent" F sighting proved nothing new.
func TestRunPaidProxyGradeOnce_AdvancesProbePassCounter(t *testing.T) {
	withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	before := tableProbePassCounter.Load()
	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)
	after := tableProbePassCounter.Load()

	if after != before+1 {
		t.Fatalf("expected the paid tick to advance the probe pass counter by exactly 1, got %d -> %d", before, after)
	}
}

// Kill switch (proxy_probe.json enabled=false) is a full skip for paid
// grading: no probes, so no rotation either.
func TestRunPaidProxyGradeOnce_KillSwitchDoesNotAdvanceProbePassCounter(t *testing.T) {
	withTempHome(t)
	writePaidGradeProbeOverride(t, false)

	before := tableProbePassCounter.Load()
	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)
	after := tableProbePassCounter.Load()

	if after != before {
		t.Fatalf("expected the disabled paid grader not to touch the probe pass counter, got %d -> %d", before, after)
	}
}
