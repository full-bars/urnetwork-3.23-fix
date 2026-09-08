package main

import (
	"os"
	"runtime/debug"
	"testing"

	"github.com/urnetwork/connect"
)

// setPersistedControlValue sets one control-socket key directly in
// globalControlState for a test, under the same locking every other access
// uses, and restores the state on cleanup.
func setPersistedControlValue(t *testing.T, key, value string) {
	t.Helper()
	resetGlobalControlStateForTest()
	globalControlState.txMu.Lock()
	if err := globalControlState.set(key, value); err != nil {
		globalControlState.txMu.Unlock()
		t.Fatalf("set %s=%s: %v", key, value, err)
	}
	globalControlState.txMu.Unlock()
	t.Cleanup(resetGlobalControlStateForTest)
}

// withCleanGOMEMLIMITEnv clears GOMEMLIMIT for the duration of the test and
// restores whatever was there afterward, so applyTurboMemoryLimit/
// applyEcoSettings's env-var precedence check doesn't accidentally win over
// the persisted value this test is trying to isolate.
func withCleanGOMEMLIMITEnv(t *testing.T) {
	t.Helper()
	orig, had := os.LookupEnv("GOMEMLIMIT")
	os.Unsetenv("GOMEMLIMIT")
	t.Cleanup(func() {
		if had {
			os.Setenv("GOMEMLIMIT", orig)
		} else {
			os.Unsetenv("GOMEMLIMIT")
		}
	})
}

func withCleanGOGCEnv(t *testing.T) {
	t.Helper()
	orig, had := os.LookupEnv("GOGC")
	os.Unsetenv("GOGC")
	t.Cleanup(func() {
		if had {
			os.Setenv("GOGC", orig)
		} else {
			os.Unsetenv("GOGC")
		}
	})
}

// TestApplyTurboMemoryLimit_PersistedGomemlimitWins pins the precedence bug:
// applyTurboMemoryLimit used to check only the GOMEMLIMIT env var before
// overwriting the runtime memory limit with the turbo default (80% of RAM)
// on every provideWithProxy call — silently clobbering an operator's
// persisted `urnet-tools set gomemlimit` back to the profile default the
// next time a proxy connected. A persisted value must win over the turbo
// default.
func TestApplyTurboMemoryLimit_PersistedGomemlimitWins(t *testing.T) {
	withCleanGOMEMLIMITEnv(t)
	setPersistedControlValue(t, "gomemlimit", "123MiB")

	const persisted = int64(123 * 1024 * 1024)
	debug.SetMemoryLimit(persisted)
	t.Cleanup(func() { debug.SetMemoryLimit(-1) })

	applyTurboMemoryLimit("turbo-v4", 0)

	if got := debug.SetMemoryLimit(-1); got != persisted {
		t.Fatalf("GOMEMLIMIT after applyTurboMemoryLimit = %d, want persisted value %d (turbo default must not clobber it)", got, persisted)
	}
}

// TestApplyEcoSettings_PersistedGomemlimitWins mirrors the turbo case for
// the eco profile's GOMEMLIMIT default (75% of RAM).
func TestApplyEcoSettings_PersistedGomemlimitWins(t *testing.T) {
	withCleanGOMEMLIMITEnv(t)
	setPersistedControlValue(t, "gomemlimit", "77MiB")

	origProfile, hadProfile := os.LookupEnv("URNETWORK_PROFILE")
	os.Setenv("URNETWORK_PROFILE", "eco")
	t.Cleanup(func() {
		if hadProfile {
			os.Setenv("URNETWORK_PROFILE", origProfile)
		} else {
			os.Unsetenv("URNETWORK_PROFILE")
		}
	})

	const persisted = int64(77 * 1024 * 1024)
	debug.SetMemoryLimit(persisted)
	t.Cleanup(func() { debug.SetMemoryLimit(-1) })

	applyEcoSettings(0)

	if got := debug.SetMemoryLimit(-1); got != persisted {
		t.Fatalf("GOMEMLIMIT after applyEcoSettings = %d, want persisted value %d (eco default must not clobber it)", got, persisted)
	}
}

// TestApplyTurboSettings_PersistedGogcWins: applyTurboSettings must not
// reset GOGC to its 200% default when the operator persisted an explicit
// gogc via the control socket.
func TestApplyTurboSettings_PersistedGogcWins(t *testing.T) {
	withCleanGOGCEnv(t)
	setPersistedControlValue(t, "gogc", "33")

	origProfile, hadProfile := os.LookupEnv("URNETWORK_PROFILE")
	os.Setenv("URNETWORK_PROFILE", "turbo-v4")
	t.Cleanup(func() {
		if hadProfile {
			os.Setenv("URNETWORK_PROFILE", origProfile)
		} else {
			os.Unsetenv("URNETWORK_PROFILE")
		}
	})

	origGOGC := debug.SetGCPercent(33)
	t.Cleanup(func() { debug.SetGCPercent(origGOGC) })

	clientSettings := connect.DefaultClientSettings()
	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	applyTurboSettings(clientSettings, localUserNatSettings)

	if got := debug.SetGCPercent(33); got != 33 {
		t.Fatalf("GOGC after applyTurboSettings = %d, want persisted value 33 (turbo default must not clobber it)", got)
	}
}

// TestApplyEcoSettings_PersistedGogcWins mirrors the turbo case for eco's
// GOGC default (50%).
func TestApplyEcoSettings_PersistedGogcWins(t *testing.T) {
	withCleanGOGCEnv(t)
	setPersistedControlValue(t, "gogc", "44")

	origProfile, hadProfile := os.LookupEnv("URNETWORK_PROFILE")
	os.Setenv("URNETWORK_PROFILE", "eco")
	t.Cleanup(func() {
		if hadProfile {
			os.Setenv("URNETWORK_PROFILE", origProfile)
		} else {
			os.Unsetenv("URNETWORK_PROFILE")
		}
	})

	origGOGC := debug.SetGCPercent(44)
	t.Cleanup(func() { debug.SetGCPercent(origGOGC) })

	applyEcoSettings(0)

	if got := debug.SetGCPercent(44); got != 44 {
		t.Fatalf("GOGC after applyEcoSettings = %d, want persisted value 44 (eco default must not clobber it)", got)
	}
}
