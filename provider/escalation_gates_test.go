//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Golden table for escalationGates. Every expected Action, Code and Reason
// was captured from the pre-extraction thrashEscalate over the same inputs,
// or derived and hand-verified against it; the extraction must not change
// one character of them. The capture was re-run against base 529e666cb
// during review and every captured row reproduced byte-identically; the
// handshake rows are new by construction. Restarts24h is the
// cap ladder's 24h count (0 whenever the ladder was not reached), which the
// pre-extraction runs expose as esc.Restarts-1 on restarts and as the ladder
// return in the rearm/cap-reached reasons.
func TestEscalationGates(t *testing.T) {
	now := thrashT0
	rearmState := thrashCapState{
		Cap:         300,
		SetUnix:     now.Add(-time.Minute).Unix(),
		ExpiresUnix: now.Add(23 * time.Hour).Unix(),
		Restarts:    []int64{now.Add(-time.Minute).Unix()},
	}
	reachedState := thrashCapState{
		Cap:         300,
		SetUnix:     now.Add(-3 * time.Hour).Unix(),
		ExpiresUnix: now.Add(23 * time.Hour).Unix(),
		Restarts:    []int64{now.Add(-3 * time.Hour).Unix(), now.Add(-2 * time.Hour).Unix(), now.Add(-time.Hour).Unix()},
	}
	pastWindowState := thrashCapState{
		Cap:         300,
		SetUnix:     now.Add(-31 * time.Minute).Unix(),
		ExpiresUnix: now.Add(23 * time.Hour).Unix(),
		Restarts:    []int64{now.Add(-31 * time.Minute).Unix()},
	}

	base := escalationInputs{
		SelfHeal:        true,
		Attr:            "unit",
		UnitSwapMiB:     3000,
		HostSwapUsedMiB: 3300,
		Share:           0.9,
		SupervisorKind:  "systemd",
		RestartsOn75:    true,
		Now:             now,
	}
	in := func(f func(*escalationInputs)) escalationInputs {
		got := base
		f(&got)
		return got
	}

	tests := []struct {
		name string
		in   escalationInputs
		want escalationVerdict
	}{
		{
			"self-heal-off",
			in(func(i *escalationInputs) { i.SelfHeal = false }),
			escalationVerdict{Action: "alert", Code: "self-heal-off", Reason: "self-heal is off (URNETWORK_SELF_HEAL / proxy_self_heal), so no automatic restart runs; the operator must act"},
		},
		{
			"hotswap-busy",
			in(func(i *escalationInputs) { i.HotSwapBusy = true }),
			escalationVerdict{Action: "alert", Code: "hotswap", Reason: "a hot-swap is draining or mid-handoff in this process; not restarting"},
		},
		{
			"attr-other",
			in(func(i *escalationInputs) {
				i.Attr = "other"
				i.UnitSwapMiB = 100
				i.HostSwapUsedMiB = 3000
				i.Share = 0.0333
			}),
			escalationVerdict{Action: "alert", Code: "attributed-other", Reason: "the swap belongs to another process: this provider holds 100 MB of 2.9 GB swapped (3%); never restarting someone else's thrash"},
		},
		{
			"attr-unknown",
			in(func(i *escalationInputs) { i.Attr = "unknown" }),
			escalationVerdict{Action: "alert", Code: "attributed-unknown", Reason: "the swap cannot be attributed to this provider and no per-unit PSI is readable; not restarting blind"},
		},
		{
			"no-supervisor",
			in(func(i *escalationInputs) { i.SupervisorKind = "none"; i.RestartsOn75 = false }),
			escalationVerdict{Action: "alert", Code: "no-supervisor", Reason: "not running under a service supervisor (systemd, OpenRC, or a container whose start script restarts on exit 75), so a self-exit would not be restarted; not restarting"},
		},
		{
			// The docker state-dir probe is the one gate whose result only the
			// caller can obtain (it writes a temp file): unprobed reports the
			// handshake instead of deciding.
			"docker-probe-needed",
			in(func(i *escalationInputs) { i.SupervisorKind = "docker" }),
			escalationVerdict{Action: "probe-capdir"},
		},
		{
			// Probe needed even when the cap ladder would deny: the probe runs
			// before the ladder in the shipping code, so the handshake must
			// happen first and the ladder is re-checked afterwards.
			"docker-probe-needed-beats-cap-denied",
			in(func(i *escalationInputs) { i.SupervisorKind = "docker"; i.CapState = reachedState }),
			escalationVerdict{Action: "probe-capdir"},
		},
		{
			"docker-probe-ok-restart",
			in(func(i *escalationInputs) { i.SupervisorKind = "docker"; i.CapDirProbed = true }),
			escalationVerdict{Action: "restart", Code: "restart"},
		},
		{
			// A clean probe with the ladder in its re-arm window: the probe
			// gate passes and the ladder denies.
			"docker-probed-rearm",
			in(func(i *escalationInputs) { i.SupervisorKind = "docker"; i.CapDirProbed = true; i.CapState = rearmState }),
			escalationVerdict{Action: "alert", Code: "rearm", Reason: "re-arming: last restart was 1m ago, next allowed in 29m", Restarts24h: 1},
		},
		{
			"docker-probe-fail",
			in(func(i *escalationInputs) {
				i.SupervisorKind = "docker"
				i.CapDirProbed = true
				i.CapDirErr = errors.New("p2 probe boom")
			}),
			escalationVerdict{Action: "alert", Code: "persist-failed", Reason: "state directory holding thrash_cap.json is not writable (p2 probe boom); not restarting, to avoid an unthrottled restart loop"},
		},
		{
			// Captured precedence: the probe gate sits above the cap ladder,
			// so persist-failed beats cap-reached.
			"docker-probe-fail-beats-cap-denied",
			in(func(i *escalationInputs) {
				i.SupervisorKind = "docker"
				i.CapDirProbed = true
				i.CapDirErr = errors.New("p2 probe boom")
				i.CapState = reachedState
			}),
			escalationVerdict{Action: "alert", Code: "persist-failed", Reason: "state directory holding thrash_cap.json is not writable (p2 probe boom); not restarting, to avoid an unthrottled restart loop"},
		},
		{
			"cap-rearm",
			in(func(i *escalationInputs) { i.CapState = rearmState }),
			escalationVerdict{Action: "alert", Code: "rearm", Reason: "re-arming: last restart was 1m ago, next allowed in 29m", Restarts24h: 1},
		},
		{
			"cap-reached",
			in(func(i *escalationInputs) { i.CapState = reachedState }),
			escalationVerdict{Action: "alert", Code: "cap-reached", Reason: "restart cap reached (3 of max 3 restarts in 24h); not restarting again today", Restarts24h: 3},
		},
		{
			"restart",
			in(func(i *escalationInputs) {}),
			escalationVerdict{Action: "restart", Code: "restart"},
		},
		{
			"restart-after-window",
			in(func(i *escalationInputs) { i.CapState = pastWindowState }),
			escalationVerdict{Action: "restart", Code: "restart", Restarts24h: 1},
		},
		{
			// Only the docker kind probes; every other supervised kind passes
			// straight to the ladder.
			"openrc-restart",
			in(func(i *escalationInputs) { i.SupervisorKind = "openrc" }),
			escalationVerdict{Action: "restart", Code: "restart"},
		},
		{
			// Multi-fault rows pin the gate ORDER: single-fault rows cannot
			// (each trips only its own gate).
			"order-self-heal-beats-all",
			in(func(i *escalationInputs) {
				i.SelfHeal = false
				i.HotSwapBusy = true
				i.Attr = "other"
				i.SupervisorKind = "none"
				i.RestartsOn75 = false
			}),
			escalationVerdict{Action: "alert", Code: "self-heal-off", Reason: "self-heal is off (URNETWORK_SELF_HEAL / proxy_self_heal), so no automatic restart runs; the operator must act"},
		},
		{
			"order-hotswap-beats-attr-and-supervisor",
			in(func(i *escalationInputs) {
				i.HotSwapBusy = true
				i.Attr = "other"
				i.RestartsOn75 = false
			}),
			escalationVerdict{Action: "alert", Code: "hotswap", Reason: "a hot-swap is draining or mid-handoff in this process; not restarting"},
		},
		{
			"order-attr-beats-supervisor",
			in(func(i *escalationInputs) {
				i.Attr = "unknown"
				i.RestartsOn75 = false
			}),
			escalationVerdict{Action: "alert", Code: "attributed-unknown", Reason: "the swap cannot be attributed to this provider and no per-unit PSI is readable; not restarting blind"},
		},
		{
			"order-supervisor-beats-probe",
			in(func(i *escalationInputs) {
				i.SupervisorKind = "docker"
				i.RestartsOn75 = false
				i.CapState = reachedState
			}),
			escalationVerdict{Action: "alert", Code: "no-supervisor", Reason: "not running under a service supervisor (systemd, OpenRC, or a container whose start script restarts on exit 75), so a self-exit would not be restarted; not restarting"},
		},
		{
			"order-no-probe-without-contract",
			in(func(i *escalationInputs) {
				i.SupervisorKind = "docker"
				i.RestartsOn75 = false
			}),
			escalationVerdict{Action: "alert", Code: "no-supervisor", Reason: "not running under a service supervisor (systemd, OpenRC, or a container whose start script restarts on exit 75), so a self-exit would not be restarted; not restarting"},
		},
	}

	for _, tt := range tests {
		got := escalationGates(tt.in)
		if got.Action != tt.want.Action || got.Code != tt.want.Code || got.Reason != tt.want.Reason || got.Restarts24h != tt.want.Restarts24h {
			t.Errorf("%s: escalationGates() = (%q, %q, %q, %d), want (%q, %q, %q, %d)",
				tt.name, got.Action, got.Code, got.Reason, got.Restarts24h,
				tt.want.Action, tt.want.Code, tt.want.Reason, tt.want.Restarts24h)
		}
	}
}

// The delegating caller keeps the shipping behaviors around the pure stack:
// the docker state-dir probe runs exactly when (and only when) the gate stack
// reaches it, and a restart verdict keeps producing the same thrashEscalation
// the pre-extraction code returned (captured values).
func TestThrashEscalateDelegatesAndProbesOnlyWhenReached(t *testing.T) {
	resetGlobalControlStateForTest()
	t.Cleanup(resetGlobalControlStateForTest)
	ResetHotSwapStateForTest()
	t.Cleanup(ResetHotSwapStateForTest)

	oldFileExists := thrashFileExistsFn
	oldProbe := thrashCheckCapDirWritableFn
	oldRunning := lastRunningProxyCount.Load()
	var probeCalls atomic.Int32
	t.Cleanup(func() {
		thrashFileExistsFn = oldFileExists
		thrashCheckCapDirWritableFn = oldProbe
		lastRunningProxyCount.Store(oldRunning)
	})

	now := thrashT0
	rt := thrashRates{fullFrac: 0.5, fullOK: true, swapOK: true, swapInPS: 500, swapOutPS: 500}
	rdUnit := thrashRead{unitSwapOK: true, unitSwapMiB: 3000, hostSwapOK: true, hostSwapUsedMiB: 3300, hostSwapTotalMiB: 3300}

	newTempState := func(t *testing.T, reached bool) {
		t.Helper()
		withTempHome(t)
		oldEnvFn := thrashEnvFn
		thrashEnvFn = os.Getenv
		t.Cleanup(func() { thrashEnvFn = oldEnvFn })
		t.Setenv("INVOCATION_ID", "")
		t.Setenv("NOTIFY_SOCKET", "")
		t.Setenv("URNETWORK_INIT", "")
		t.Setenv("URNETWORK_CONTAINER", "1")
		thrashFileExistsFn = func(string) bool { return false }
		probeCalls.Store(0)
		thrashCheckCapDirWritableFn = func() error { probeCalls.Add(1); return nil }
		if reached {
			// One recent restart puts the ladder in its re-arm window.
			writeThrashCapStateForTest(t, thrashCapState{
				Cap:         300,
				SetUnix:     now.Add(-time.Minute).Unix(),
				ExpiresUnix: now.Add(23 * time.Hour).Unix(),
				Restarts:    []int64{now.Add(-time.Minute).Unix()},
			})
		}
	}

	// Blocked before the docker gate: the probe must not run.
	blocked := []struct {
		name     string
		selfHeal bool
		attr     string
		setup    func(t *testing.T)
		code     string
	}{
		{"self-heal-off", false, "unit", func(t *testing.T) {}, "self-heal-off"},
		{"hotswap-busy", true, "unit", func(t *testing.T) { isHotSwapDraining.Store(true) }, "hotswap"},
		{"attr-other", true, "other", func(t *testing.T) {}, "attributed-other"},
		{"attr-unknown", true, "unknown", func(t *testing.T) {}, "attributed-unknown"},
		{"no-supervisor", true, "unit", func(t *testing.T) { t.Setenv("URNETWORK_CONTAINER", "") }, "no-supervisor"},
	}
	for _, tc := range blocked {
		t.Run(tc.name, func(t *testing.T) {
			newTempState(t, false)
			isHotSwapDraining.Store(false)
			t.Cleanup(func() { isHotSwapDraining.Store(false) })
			tc.setup(t)
			got := thrashEscalate(now, rt, rdUnit, tc.selfHeal, tc.attr, 0.9, true)
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Code, tc.code)
			}
			if probeCalls.Load() != 0 {
				t.Fatalf("state-dir probe calls = %d, want 0 before the docker gate", probeCalls.Load())
			}
		})
	}

	t.Run("probe-runs-then-rearm-denies", func(t *testing.T) {
		newTempState(t, true)
		got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true)
		if got.Action != "alert" || got.Code != "rearm" {
			t.Fatalf("got (%s, %s), want (alert, rearm)", got.Action, got.Code)
		}
		if probeCalls.Load() != 1 {
			t.Fatalf("state-dir probe calls = %d, want exactly 1 once the stack reached it", probeCalls.Load())
		}
	})

	t.Run("probe-runs-then-restart", func(t *testing.T) {
		newTempState(t, false)
		lastRunningProxyCount.Store(500)
		defer lastRunningProxyCount.Store(0)
		got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true)
		if probeCalls.Load() != 1 {
			t.Fatalf("state-dir probe calls = %d, want exactly 1", probeCalls.Load())
		}
		if got.Action != "restart" || got.Restarts != 1 || got.Cap != 300 || got.Running != 500 {
			t.Fatalf("got (action=%s restarts=%d cap=%d running=%d), want (restart, 1, 300, 500)",
				got.Action, got.Restarts, got.Cap, got.Running)
		}
		wantMsg := "🚨 [memory] This will not recover on its own. Restarting the provider now (restart 1 of max 3 per day) with the proxy cap reduced 500 -> 300 so the next run fits in RAM."
		if got.Msg != wantMsg {
			t.Fatalf("msg = %q, want %q", got.Msg, wantMsg)
		}
	})

	t.Run("probe-error-becomes-persist-failed", func(t *testing.T) {
		newTempState(t, false)
		thrashCheckCapDirWritableFn = func() error { probeCalls.Add(1); return errors.New("boom") }
		got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true)
		if got.Code != "persist-failed" {
			t.Fatalf("code = %q, want persist-failed", got.Code)
		}
		if probeCalls.Load() != 1 {
			t.Fatalf("state-dir probe calls = %d, want exactly 1", probeCalls.Load())
		}
		if !strings.Contains(got.Reason, "boom") {
			t.Fatalf("reason = %q, want it to carry the probe error", got.Reason)
		}
	})

	t.Run("systemd-restart-never-probes", func(t *testing.T) {
		newTempState(t, false)
		t.Setenv("INVOCATION_ID", "1")
		got := thrashEscalate(now, rt, rdUnit, true, "unit", 0.9, true)
		if got.Action != "restart" {
			t.Fatalf("action = %q, want restart", got.Action)
		}
		if probeCalls.Load() != 0 {
			t.Fatalf("state-dir probe calls = %d, want 0 for a non-docker kind", probeCalls.Load())
		}
	})
}

// writeThrashCapStateForTest persists a cap state under the temp HOME.
func writeThrashCapStateForTest(t *testing.T, st thrashCapState) {
	t.Helper()
	path, err := thrashCapPath()
	if err != nil {
		t.Fatalf("thrashCapPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for cap state: %v", err)
	}
	if err := oomWriteJSON(path, st); err != nil {
		t.Fatalf("write cap state: %v", err)
	}
}
