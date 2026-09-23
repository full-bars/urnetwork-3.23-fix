//go:build unix

package urnettools

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withVerifyStubs overrides the package-level verify*Fn seams used by
// reconcileUnitTypeAndRestart / verifyPlainRestart and restores them on
// cleanup.
func withVerifyStubs(t *testing.T, version string, discover func() []Provider, restart func(Provider) error) {
	t.Helper()
	origVersion, origDiscover, origSleep, origRestart := verifyProviderVersionFn, verifyDiscoverFn, verifySleepFn, restartForUpdate
	t.Cleanup(func() {
		verifyProviderVersionFn, verifyDiscoverFn, verifySleepFn, restartForUpdate = origVersion, origDiscover, origSleep, origRestart
	})
	verifyProviderVersionFn = func(string) string { return version }
	verifySleepFn = func(time.Duration) {} // no real sleeping in tests
	if discover != nil {
		verifyDiscoverFn = discover
	}
	if restart != nil {
		restartForUpdate = restart
	}
}

func TestReconcileUnitTypeAndRestart_MigratesAndRestartsRunningProvider(t *testing.T) {
	p, _, reloads := fakeUnit(t, simpleUnit)
	p.StateDir = "/home/provider/.urnetwork"
	p.PID = 111
	p.Running = true

	restarted := false
	newPID := 222
	withVerifyStubs(t, "v3.23.0-fix.32.4",
		func() []Provider {
			if !restarted {
				return []Provider{{StateDir: p.StateDir, PID: p.PID}}
			}
			return []Provider{{StateDir: p.StateDir, PID: newPID}}
		},
		func(Provider) error { restarted = true; return nil },
	)

	migrated, err := reconcileUnitTypeAndRestart(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true for a hotswap-supported binary on a Type=simple unit")
	}
	if !restarted {
		t.Fatal("expected a restart to hand the running process NOTIFY_SOCKET")
	}
	if *reloads != 1 {
		t.Errorf("daemon-reload ran %d times, want 1", *reloads)
	}
}

func TestReconcileUnitTypeAndRestart_DemotionNeverRestarts(t *testing.T) {
	// Unit is already notify (e.g. from a prior migration); binary on disk
	// is a downgrade that cannot send sd_notify.
	p, _, _ := fakeUnit(t, strings.ReplaceAll(simpleUnit, "Type=simple", "Type=notify"))
	p.PID = 111
	p.Running = true

	restartCalled := false
	withVerifyStubs(t, "v3.23.0-fix.30.9", nil, func(Provider) error {
		restartCalled = true
		return nil
	})

	migrated, err := reconcileUnitTypeAndRestart(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if migrated {
		t.Fatal("a demotion must never report migrated=true")
	}
	if restartCalled {
		t.Fatal("demotion to Type=simple must not restart the already-running process")
	}
}

func TestReconcileUnitTypeAndRestart_NoRestartWhenNotRunning(t *testing.T) {
	p, _, _ := fakeUnit(t, simpleUnit)
	p.Running = false
	p.PID = 0

	restartCalled := false
	withVerifyStubs(t, "v3.23.0-fix.32.4", nil, func(Provider) error {
		restartCalled = true
		return nil
	})

	migrated, err := reconcileUnitTypeAndRestart(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !migrated {
		t.Fatal("expected migrated=true even though nothing needed restarting")
	}
	if restartCalled {
		t.Fatal("must not restart a provider that was not observed running")
	}
}

func TestReconcileUnitTypeAndRestart_RestartFailurePropagates(t *testing.T) {
	p, _, _ := fakeUnit(t, simpleUnit)
	p.StateDir = "/home/provider/.urnetwork"
	p.PID = 111
	p.Running = true

	wantErr := errors.New("systemctl restart: permission denied")
	withVerifyStubs(t, "v3.23.0-fix.32.4", nil, func(Provider) error { return wantErr })

	migrated, err := reconcileUnitTypeAndRestart(p)
	if !migrated {
		t.Error("the unit WAS migrated even though the restart failed — migrated should still report true")
	}
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped restart error, got %v", err)
	}
}

// TestReportAlreadyCurrent_WiresIntoSelfHeal guards the actual call site
// cmdUpdate's skip branch delegates to. Unlike the reconcileUnitTypeAndRestart
// tests above, this catches a regression where a future edit removes the
// call from reportAlreadyCurrent (or cmdUpdate stops calling
// reportAlreadyCurrent) without anyone touching reconcileUnitTypeAndRestart
// itself — the gap that let the installer silently strand HotSwap in the
// first place.
func TestReportAlreadyCurrent_WiresIntoSelfHeal(t *testing.T) {
	p, _, _ := fakeUnit(t, simpleUnit)
	p.StateDir = "/home/provider/.urnetwork"
	p.PID = 111
	p.Running = true
	cfg := updateConfig{Tag: "v3.23.0-fix.32.4"}

	restarted := false
	newPID := 222
	withVerifyStubs(t, "v3.23.0-fix.32.4",
		func() []Provider {
			if !restarted {
				return []Provider{{StateDir: p.StateDir, PID: p.PID}}
			}
			return []Provider{{StateDir: p.StateDir, PID: newPID}}
		},
		func(Provider) error { restarted = true; return nil },
	)

	out := captureStdout(t, func() { reportAlreadyCurrent(p, cfg) })

	if !restarted {
		t.Fatal("reportAlreadyCurrent did not reconcile the clobbered unit — self-heal is not wired into the skip path")
	}
	if !strings.Contains(out, "reconciled for HotSwap") {
		t.Errorf("expected the reconciled-unit message on stdout, got: %q", out)
	}
}

// TestReportAlreadyCurrent_NoOpStaysQuiet is the control case: a unit that
// needs no reconciliation must not restart the provider or print the
// reconciled-unit message — the self-heal must not fire on every update.
func TestReportAlreadyCurrent_NoOpStaysQuiet(t *testing.T) {
	p, _, _ := fakeUnit(t, strings.ReplaceAll(simpleUnit, "Type=simple", "Type=notify"))
	p.PID = 111
	p.Running = true
	cfg := updateConfig{Tag: "v3.23.0-fix.32.4"}

	restarted := false
	withVerifyStubs(t, "v3.23.0-fix.32.4", nil, func(Provider) error {
		restarted = true
		return nil
	})

	out := captureStdout(t, func() { reportAlreadyCurrent(p, cfg) })

	if restarted {
		t.Fatal("reportAlreadyCurrent restarted a provider whose unit needed no reconciliation")
	}
	if strings.Contains(out, "reconciled for HotSwap") {
		t.Errorf("expected the plain already-current message, got: %q", out)
	}
	if !strings.Contains(out, "already on") {
		t.Errorf("expected the standard already-current message, got: %q", out)
	}
}

func TestReconcileUnitTypeAndRestart_VerifyTimeoutPropagates(t *testing.T) {
	p, _, _ := fakeUnit(t, simpleUnit)
	p.StateDir = "/home/provider/.urnetwork"
	p.PID = 111
	p.Running = true

	// PID never changes across the poll loop -> verifyPlainRestart times out.
	withVerifyStubs(t, "v3.23.0-fix.32.4",
		func() []Provider { return []Provider{{StateDir: p.StateDir, PID: p.PID}} },
		func(Provider) error { return nil },
	)

	migrated, err := reconcileUnitTypeAndRestart(p)
	if !migrated {
		t.Error("migrated should still report true even when verification times out")
	}
	if err == nil {
		t.Fatal("expected an error when the restarted process never shows a new PID")
	}
	if !strings.Contains(err.Error(), "did not take effect") {
		t.Errorf("expected a 'did not take effect' error, got: %v", err)
	}
}
