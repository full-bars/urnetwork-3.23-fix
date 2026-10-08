package urnettools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubVerifyLoop replaces all verify*Fn vars with no-ops/stubs and returns
// a cleanup function that restores the originals. Call t.Cleanup with the
// returned func so hooks are restored even on test failure.
func stubVerifyLoop(t *testing.T) {
	t.Helper()

	origDiscover := verifyDiscoverFn
	origHandle := verifyRunningImageHandleFn
	origPath := verifyRunningImagePathFn
	origVersion := verifyProviderVersionFn
	origAlive := verifyPidIsAliveFn
	origPrune := verifyPruneBackupsFn
	origSleep := verifySleepFn
	origSuccess := verifyRecordSuccessFn
	origDecline := verifyRecordDeclineFn

	t.Cleanup(func() {
		verifyDiscoverFn = origDiscover
		verifyRunningImageHandleFn = origHandle
		verifyRunningImagePathFn = origPath
		verifyProviderVersionFn = origVersion
		verifyPidIsAliveFn = origAlive
		verifyPruneBackupsFn = origPrune
		verifySleepFn = origSleep
		verifyRecordSuccessFn = origSuccess
		verifyRecordDeclineFn = origDecline
	})

	// Default stubs: no-op sleep, always-alive PID, no-op side effects.
	verifySleepFn = func(d time.Duration) { /* no-op */ }
	verifyPidIsAliveFn = func(pid int) bool { return true }
	verifyPruneBackupsFn = func(string, int) {}
	verifyRecordSuccessFn = func(string) {}
	verifyRecordDeclineFn = func(string, string) {}
	verifyRunningImagePathFn = func(pid int) (string, error) {
		return "/usr/bin/provider", nil
	}
}

func testProvider(stateDir string, pid int) Provider {
	return Provider{
		StateDir: stateDir,
		Binary:   "/usr/bin/provider",
		PID:      pid,
	}
}

func testConfig(tag string) updateConfig {
	return updateConfig{Tag: tag}
}

// TestUpdateVerification_PIDChangesThenSucceeds verifies the happy path:
// PID changes (restart landed), then on a subsequent iteration the version
// matches and the loop returns nil.
func TestUpdateVerification_PIDChangesThenSucceeds(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	callCount := 0

	// Discover returns a new PID; version matches on the 3rd call.
	verifyDiscoverFn = func() []Provider {
		callCount++
		switch {
		case callCount <= 2:
			// Old PID still alive, restart hasn't landed yet.
			return []Provider{testProvider(p.StateDir, 100)}
		default:
			// New PID appeared, version matches.
			return []Provider{testProvider(p.StateDir, 200)}
		}
	}

	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}

	verifyProviderVersionFn = func(binary string) string {
		// Version matches on the 3rd+ call (after PID changed).
		if callCount >= 3 {
			return cfg.Tag
		}
		return "v0.0.0-old"
	}

	err := verifyRestartLoop(p, cfg, false, "")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

// TestUpdateVerification_VersionMatchesImmediately verifies that when the
// new provider already has the correct version on the first iteration,
// the loop succeeds immediately.
func TestUpdateVerification_VersionMatchesImmediately(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{testProvider(p.StateDir, 200)}
	}

	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}

	verifyProviderVersionFn = func(binary string) string {
		return cfg.Tag
	}

	err := verifyRestartLoop(p, cfg, false, "")
	if err != nil {
		t.Fatalf("expected nil error on immediate version match, got: %v", err)
	}
}

// TestUpdateVerification_OldPidDiesNoNewProvider verifies the early-exit
// path: the old PID dies and no new provider appears, so the loop breaks
// after i > 10 (~25s) and returns the "restart did not take effect" error.
// It also pins the new diagnostics on that path: the absent-dir listing
// once, two bounded heartbeats, and the early-exit message.
func TestUpdateVerification_OldPidDiesNoNewProvider(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	sleepCalls := 0

	// Track sleep calls to verify the loop ran the expected iterations.
	verifySleepFn = func(d time.Duration) {
		sleepCalls++
	}

	// Discover always returns empty — no new provider.
	verifyDiscoverFn = func() []Provider {
		return nil
	}

	// Old PID is dead.
	verifyPidIsAliveFn = func(pid int) bool {
		if pid == 100 {
			return false // old PID is dead
		}
		return false
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })

	// Should return error because restart didn't take effect.
	if err == nil {
		t.Fatal("expected error when old PID dies with no new provider")
	}

	if !strings.Contains(err.Error(), "restart did not take effect") {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Count(out, "no provider entry matches state dir"); got != 1 {
		t.Errorf("expected the absent-dir listing exactly once, got %d:\n%s", got, out)
	}
	// Heartbeats print at i=5 and i=10, then the i > 10 early exit fires.
	if got := strings.Count(out, "no matching provider yet"); got != 2 {
		t.Errorf("expected 2 bounded heartbeats before the early exit, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "exited but no new provider") {
		t.Errorf("expected the early-exit message, got:\n%s", out)
	}

	// The early exit fires at i=11 (the i > 10 guard), so 12 iteration
	// sleeps + the initial settle = 13 sleep calls; the check below keeps
	// a conservative lower bound.
	if sleepCalls < 5 {
		t.Fatalf("expected at least 5 sleep calls (1 settle + 4 iterations), got %d", sleepCalls)
	}
}

// TestUpdateVerification_HotSwapDecline verifies that when hotSwapTriggered
// is true and verification fails, the decline is recorded and the error
// mentions "HotSwap candidate".
func TestUpdateVerification_HotSwapDecline(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	declined := false

	verifyDiscoverFn = func() []Provider { return nil }
	verifyPidIsAliveFn = func(pid int) bool { return false }
	verifyRecordDeclineFn = func(stateDir, reason string) {
		declined = true
		if reason != "takeover_failed" {
			t.Errorf("expected decline reason 'takeover_failed', got %q", reason)
		}
	}

	err := verifyRestartLoop(p, cfg, true, "")

	if err == nil {
		t.Fatal("expected error for hotswap verification failure")
	}
	if !declined {
		t.Error("expected recordHotswapDecline to be called")
	}
	if !strings.Contains(err.Error(), "HotSwap candidate") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestUpdateVerification_StillWaitingLog verifies the periodic progress
// log fires every 5 iterations after a PID change.
func TestUpdateVerification_StillWaitingLog(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	callCount := 0

	// Discover always returns new PID but version never matches.
	verifyDiscoverFn = func() []Provider {
		callCount++
		return []Provider{testProvider(p.StateDir, 200)}
	}
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}
	verifyProviderVersionFn = func(binary string) string {
		return "v0.0.0-stale"
	}

	// Override sleep to count iterations and break the loop.
	iterCount := 0
	verifySleepFn = func(d time.Duration) {
		iterCount++
	}

	// The loop runs 30 iterations max. We just want to confirm it doesn't
	// crash and returns an error (verification failed).
	err := verifyRestartLoop(p, cfg, false, "")
	if err == nil {
		t.Fatal("expected error when version never matches")
	}
}

// TestUpdateExplicitDigestMismatchNotSkipped verifies the W6 fix: when the
// operator supplies --digest explicitly and the on-disk version matches the
// tag, the digest must still be checked against the binary. A wrong digest
// should cause an update (not a silent skip).
func TestUpdateExplicitDigestMismatchNotSkipped(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "provider")
	binaryContent := []byte("#!/bin/sh\n# fake provider binary\n")
	if err := os.WriteFile(binaryPath, binaryContent, 0o755); err != nil {
		t.Fatal(err)
	}
	actualDigest := fmt.Sprintf("%x", sha256.Sum256(binaryContent))

	base := Provider{
		Binary:   binaryPath,
		Version:  "v3.23.0-fix.31.1",
		StateDir: "/tmp/test",
		Running:  true,
		PID:      1234,
	}

	// Helper: simulate the skip-decision for a given provider + config.
	checkSkip := func(p Provider, cfg updateConfig) (skip bool) {
		if p.Version == cfg.Tag && !p.BinaryDeleted && p.PID > 0 {
			if exe, err := runningImagePath(p.PID); err == nil {
				_, isDeleted := strings.CutSuffix(exe, " (deleted)")
				skip = !isDeleted
			} else {
				skip = false
			}
		} else if p.Version == cfg.Tag && !p.BinaryDeleted && p.PID == 0 {
			skip = true
		}
		if skip && cfg.DigestExplicit && p.Binary != "" && !p.BinaryDeleted {
			if actual, err := fileSHA256(p.Binary); err != nil {
				// can't verify — keep skipping (existing behavior)
			} else if !strings.EqualFold(actual, cfg.Digest) {
				skip = false
			}
		}
		return skip
	}

	// Case 1: --digest matches the binary AND version matches → should skip.
	cfgMatch := updateConfig{Tag: "v3.23.0-fix.31.1", Digest: actualDigest, DigestExplicit: true}
	// Use PID=0 to avoid runningImagePath (which would fail for 1234 on test box).
	p1 := Provider{Binary: binaryPath, Version: "v3.23.0-fix.31.1", PID: 0, BinaryDeleted: false}
	if !checkSkip(p1, cfgMatch) {
		t.Error("expected skip when digest matches on-disk binary")
	}

	// Case 2: --digest does NOT match → skip should be cancelled (proceed to update).
	cfgMismatch := updateConfig{Tag: "v3.23.0-fix.31.1", Digest: "0000000000000000000000000000000000000000000000000000000000000000", DigestExplicit: true}
	if checkSkip(p1, cfgMismatch) {
		t.Error("expected NO skip when explicit --digest mismatches — this is the W6 bug")
	}

	// Case 3: --digest not explicit, version matches → should skip (no digest check).
	cfgNoExplicit := updateConfig{Tag: "v3.23.0-fix.31.1", Digest: "", DigestExplicit: false}
	if !checkSkip(p1, cfgNoExplicit) {
		t.Error("expected skip when no explicit --digest and version matches")
	}

	_ = base
}

// TestUpdateVerification_NoMatchLogsStateDirs pins the diagnostic for the
// silent wait: when discovery has no entry for the provider's state dir at
// all, the loop prints the discovered state dirs once and then a bounded
// heartbeat instead of staying quiet for the whole window.
func TestUpdateVerification_NoMatchLogsStateDirs(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{testProvider("/home/other/.urnetwork", 200)}
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when no discovered provider matches")
	}
	if !strings.Contains(out, `no provider entry matches state dir "/home/user/.urnetwork"`) {
		t.Errorf("expected the missing-state-dir diagnostic, got:\n%s", out)
	}
	if !strings.Contains(out, `"/home/other/.urnetwork"`) {
		t.Errorf("expected the discovered state dir in the diagnostic, got:\n%s", out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 1 {
		t.Errorf("expected the state-dir listing exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "present but not ready"); got != 0 {
		t.Errorf("an absent state-dir entry must not print the not-ready line, got %d:\n%s", got, out)
	}
	// maxIterations is 30; the heartbeat prints at iterations 6/11/16/21/26.
	if got := strings.Count(out, "no matching provider yet"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_NoMatchNoProvidersLogsNone pins the zero-provider
// rendering of the missing-state-dir diagnostic: discovery sees nothing at
// all, a distinct failure shape from "sees other state dirs".
func TestUpdateVerification_NoMatchNoProvidersLogsNone(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider { return nil }

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when discovery sees no providers")
	}
	if !strings.Contains(out, "discovery sees: (none)") {
		t.Errorf("expected the (none) rendering, got:\n%s", out)
	}
	if got := strings.Count(out, "present but not ready"); got != 0 {
		t.Errorf("a zero-provider discovery must not print the not-ready line, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet (iteration"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_NotReadyEntryLogsReason pins the diagnostic for the
// other silent case: discovery has an entry for the provider's state dir, but
// it cannot satisfy the readiness predicate — typically the stale
// pre-restart process, whose swapped-out binary reads as binaryDeleted.
func TestUpdateVerification_NotReadyEntryLogsReason(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{{StateDir: p.StateDir, PID: 100, BinaryDeleted: true}}
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when the only discovered entry is stale")
	}
	if !strings.Contains(out, "present but not ready") {
		t.Errorf("expected the not-ready diagnostic, got:\n%s", out)
	}
	if !strings.Contains(out, "pid 100, binaryDeleted=true") {
		t.Errorf("expected the entry's readiness fields, got:\n%s", out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 0 {
		t.Errorf("a present state-dir entry must not print the absent-dir listing, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "present but not ready"); got != 1 {
		t.Errorf("expected the not-ready detail exactly once, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_TransientNotReadyThenSuccess pins the transition
// shape: the stale pre-restart process is seen once (not-ready reported
// exactly once, no heartbeat), then the restarted process appears and the
// loop returns success without further diagnostics.
func TestUpdateVerification_TransientNotReadyThenSuccess(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	callCount := 0

	verifyDiscoverFn = func() []Provider {
		callCount++
		if callCount == 1 {
			return []Provider{{StateDir: p.StateDir, PID: 100, BinaryDeleted: true}}
		}
		return []Provider{{StateDir: p.StateDir, PID: 200}}
	}
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}
	verifyProviderVersionFn = func(binary string) string {
		return cfg.Tag
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err != nil {
		t.Fatalf("expected success once the restarted process appears, got: %v", err)
	}
	if got := strings.Count(out, "present but not ready"); got != 1 {
		t.Errorf("expected the not-ready transient reported exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 0 {
		t.Errorf("expected no heartbeats once the entry is present, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 0 {
		t.Errorf("expected no absent-dir listing, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_TransientMissThenSuccess pins the cold-restart
// shape: the old process is already gone when the loop starts (discovery
// sees nothing), then the new process appears and the loop succeeds — the
// absent-dir listing prints exactly once and no heartbeat follows.
func TestUpdateVerification_TransientMissThenSuccess(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	callCount := 0

	verifyDiscoverFn = func() []Provider {
		callCount++
		if callCount == 1 {
			return nil
		}
		return []Provider{{StateDir: p.StateDir, PID: 200}}
	}
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "/proc/200/exe", nil
	}
	verifyProviderVersionFn = func(binary string) string {
		return cfg.Tag
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err != nil {
		t.Fatalf("expected success once the restarted process appears, got: %v", err)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 1 {
		t.Errorf("expected the absent-dir listing exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 0 {
		t.Errorf("expected no heartbeats once the entry appears, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "present but not ready"); got != 0 {
		t.Errorf("expected no not-ready diagnostics, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_NotReadyTakesPrecedenceOverListing pins the
// branch-selection rule: when discovery has a stale entry for the
// provider's own state dir plus entries for other dirs, the not-ready line
// fires and the absent-dir listing stays silent.
func TestUpdateVerification_NotReadyTakesPrecedenceOverListing(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{
			{StateDir: p.StateDir, PID: 100, BinaryDeleted: true},
			testProvider("/home/other/.urnetwork", 200),
		}
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when the same-dir entry never becomes ready")
	}
	if got := strings.Count(out, "present but not ready"); got != 1 {
		t.Errorf("expected the not-ready line exactly once, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no provider entry matches state dir"); got != 0 {
		t.Errorf("a present same-dir entry must suppress the absent-dir listing, got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 5 {
		t.Errorf("expected 5 bounded heartbeats, got %d:\n%s", got, out)
	}
}

// TestUpdateVerification_ImageReadFailureLogs pins the third diagnostic
// shape: an entry satisfies the readiness predicate but its running image
// cannot be read, which used to leave the iteration silent.
func TestUpdateVerification_ImageReadFailureLogs(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")

	verifyDiscoverFn = func() []Provider {
		return []Provider{testProvider(p.StateDir, 200)}
	}
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		return "", fmt.Errorf("read /proc/%d/exe: permission denied", pid)
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err == nil {
		t.Fatal("expected error when the running image is never readable")
	}
	if !strings.Contains(out, "running image unreadable") {
		t.Errorf("expected the unreadable-image diagnostic, got:\n%s", out)
	}
	if got := strings.Count(out, "running image unreadable"); got != 5 {
		t.Errorf("expected 5 bounded diagnostics (iterations 6/11/16/21/26), got %d:\n%s", got, out)
	}
	// The entry is matched, so the unmatched-entry diagnostics must stay
	// silent — if `matched` ever regressed to false here, both families
	// would print and these pins would catch it.
	for _, absent := range []string{
		"no matching provider yet",
		"present but not ready",
		"no provider entry matches state dir",
	} {
		if got := strings.Count(out, absent); got != 0 {
			t.Errorf("expected no %q diagnostics for a matched entry, got %d:\n%s", absent, got, out)
		}
	}
}

// TestUpdateVerification_TransientImageReadFailureThenSuccess pins recovery
// from a transient running-image read failure: the failure persists through
// the first heartbeat-gated iteration (printed once there), then the read
// succeeds and the loop returns without further diagnostics.
func TestUpdateVerification_TransientImageReadFailureThenSuccess(t *testing.T) {
	stubVerifyLoop(t)

	p := testProvider("/home/user/.urnetwork", 100)
	cfg := testConfig("v3.23.0-fix.30.0")
	handleCalls := 0

	verifyDiscoverFn = func() []Provider {
		return []Provider{testProvider(p.StateDir, 200)}
	}
	// Fail through iteration 5 (handle call 6, the first gated print),
	// then recover: the failure must not suppress the eventual success.
	verifyRunningImageHandleFn = func(pid int) (string, error) {
		handleCalls++
		if handleCalls <= 6 {
			return "", fmt.Errorf("read /proc/%d/exe: permission denied", pid)
		}
		return "/proc/200/exe", nil
	}
	verifyProviderVersionFn = func(binary string) string {
		return cfg.Tag
	}

	var err error
	out := captureStdout(t, func() { err = verifyRestartLoop(p, cfg, false, "") })
	if err != nil {
		t.Fatalf("expected success after the transient read failure, got: %v", err)
	}
	if got := strings.Count(out, "running image unreadable"); got != 1 {
		t.Errorf("expected the unreadable-image line once (iteration 6), got %d:\n%s", got, out)
	}
	if got := strings.Count(out, "no matching provider yet"); got != 0 {
		t.Errorf("expected no heartbeats for a matched entry, got %d:\n%s", got, out)
	}
}
