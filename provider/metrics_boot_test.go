//go:build linux

package main

import (
	"os"
	"testing"
)

// TestPersistedMetricsOnStartsListenerAtBoot: with URNETWORK_METRICS unset,
// main.go never starts the listener, so a persisted `set metrics on` has to
// be applied by applyPersistedRuntimeTuning or it does not survive a restart.
func TestPersistedMetricsOnStartsListenerAtBoot(t *testing.T) {
	orig, had := os.LookupEnv("URNETWORK_METRICS")
	os.Unsetenv("URNETWORK_METRICS")
	t.Cleanup(func() {
		if had {
			os.Setenv("URNETWORK_METRICS", orig)
		}
	})
	if metricsServer != nil {
		t.Fatal("metrics listener already running before the test")
	}
	setPersistedControlValue(t, "metrics", "on")
	t.Cleanup(func() { _ = applyMetricsLive("off") })

	applyPersistedRuntimeTuning(globalControlState)

	if metricsServer == nil {
		t.Fatal("persisted metrics=on did not start the listener at boot")
	}
}
