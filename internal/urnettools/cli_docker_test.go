package urnettools

import (
	"testing"
)

// mockContainerProbes installs in-memory mocks for the three container
// inspection hooks used by waitForLiveVersion and restores the originals on
// cleanup. This is the same var-injection pattern used for discoverDockerFn
// in namespace_guard_test.go.
func mockContainerProbes(t *testing.T, running, alive bool, liveFn func() string) {
	t.Helper()
	origRunning := containerRunning
	origAlive := containerProviderAlive
	origLive := containerLiveVersion
	t.Cleanup(func() {
		containerRunning = origRunning
		containerProviderAlive = origAlive
		containerLiveVersion = origLive
	})
	containerRunning = func(string) bool { return running }
	containerProviderAlive = func(string) bool { return alive }
	containerLiveVersion = func(string) string { return liveFn() }
}

// TestWaitForLiveVersion_NoOpUpdateIsNotFalseSuccess is the CR #1 regression:
// a no-op in-place update leaves the live binary version UNCHANGED on a real
// container. The pre-fix code compared the live version against the docker
// image tag (p.Version), which is fixed at container-creation time — so a
// no-op update "trivially equals the tag" and reported a false success. The
// fixed code compares against the PRE-update live reading, so a no-op
// (live == prev) is correctly reported as failure.
//
// This test FAILS against the pre-fix logic (which returns ok=true here) and
// PASSES after the fix.
func TestWaitForLiveVersion_NoOpUpdateIsNotFalseSuccess(t *testing.T) {
	mockContainerProbes(t, true, true, func() string { return "urnetwork 26.5.0 stable" })
	if _, ok := waitForLiveVersion("c", "urnetwork 26.5.0 stable", 1); ok {
		t.Fatal("no-op update (live == prev) must NOT report success")
	}
}

// TestWaitForLiveVersion_ChangedVersionSucceeds covers the happy path: when
// the live version differs from the pre-update reading, verification passes
// and returns the observed version.
func TestWaitForLiveVersion_ChangedVersionSucceeds(t *testing.T) {
	mockContainerProbes(t, true, true, func() string { return "urnetwork 26.6.0 stable" })
	v, ok := waitForLiveVersion("c", "urnetwork 26.5.0 stable", 1)
	if !ok {
		t.Fatal("update that changed the live version should report success")
	}
	if v != "urnetwork 26.6.0 stable" {
		t.Fatalf("unexpected observed version: %q", v)
	}
}

// TestWaitForLiveVersion_WaitsForChangeWithRetry covers the realistic case
// where the live version has not yet swapped on the first poll but does on a
// later poll (the binary swap takes a few seconds to land). The loop must
// keep polling until the version differs, not bail early on the stale
// reading.
func TestWaitForLiveVersion_WaitsForChangeWithRetry(t *testing.T) {
	calls := 0
	prev := "urnetwork 26.5.0 stable"
	mockContainerProbes(t, true, true, func() string {
		calls++
		if calls < 2 {
			return prev // stale for the first poll
		}
		return "urnetwork 26.6.0 stable"
	})
	v, ok := waitForLiveVersion("c", prev, 5)
	if !ok {
		t.Fatal("verification should succeed once the version changes")
	}
	if v != "urnetwork 26.6.0 stable" {
		t.Fatalf("unexpected observed version: %q", v)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 polls while waiting for the swap, got %d", calls)
	}
}

// TestWaitForLiveVersion_NotRunningDoesNotSucceed ensures that when the
// container or provider is no longer up, verification never reports success
// (only the timeout failure path).
func TestWaitForLiveVersion_NotRunningDoesNotSucceed(t *testing.T) {
	mockContainerProbes(t, false, false, func() string { return "urnetwork 26.6.0 stable" })
	if _, ok := waitForLiveVersion("c", "urnetwork 26.5.0 stable", 1); ok {
		t.Fatal("verification must not succeed when the container/provider is down")
	}
}
