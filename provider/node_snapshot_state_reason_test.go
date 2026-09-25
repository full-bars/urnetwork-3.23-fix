package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The provider knows it is still "resolving proxies" (systemd STATUS= says so),
// but the snapshot used to look only at uptime. A node whose startup stalled read
// IDLE / "no proxies configured" after two minutes while systemd still said
// "starting". The state and its reason must follow the real startup phase.
func TestDeriveSnapshotStateFollowsStartupPhase(t *testing.T) {
	healthy := SnapshotProxies{Up: 5}
	cases := []struct {
		name       string
		in         stateInputs
		wantState  string
		wantReason string
	}{
		{"normal warmup has no reason", stateInputs{uptime: 30 * time.Second, proxies: healthy}, "starting", ""},
		{"still resolving past 2 min stays starting", stateInputs{uptime: 3 * time.Minute, startup: startupResolving}, "starting", "resolving proxies (3 min)"},
		{"still resolving at 4 min", stateInputs{uptime: 4*time.Minute + 59*time.Second, startup: startupResolving}, "starting", "resolving proxies (4 min)"},
		{"stuck resolving at 5 min is degraded", stateInputs{uptime: 5 * time.Minute, startup: startupResolving}, "degraded", "startup stuck: resolving proxies for 5 min"},
		{"source unreachable is degraded once startup ends", stateInputs{uptime: 3 * time.Minute, startup: startupSourceUnreachable}, "degraded", "proxy source unreachable, retrying"},
		{"empty source is degraded once startup ends", stateInputs{uptime: 3 * time.Minute, startup: startupSourceEmpty}, "degraded", "proxy source returned no usable proxies, retrying"},
		{"settled startup falls through to normal states", stateInputs{uptime: 3 * time.Minute, proxies: healthy, avg1m: 1e9}, "flowing", ""},

		{"degraded by dead proxies says how many", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Up: 4, Dead: 4, Degraded: 2}}, "degraded", "6 of 10 proxies dead or degraded"},
		{"degraded by pressure says the level", stateInputs{uptime: time.Hour, proxies: healthy, pressure: 0.85}, "degraded", "resource pressure 0.85"},
		{"degraded by both says both", stateInputs{uptime: time.Hour, proxies: SnapshotProxies{Up: 2, Dead: 8}, pressure: 0.9}, "degraded", "8 of 10 proxies dead or degraded; resource pressure 0.90"},
		{"idle and flowing carry no state reason", stateInputs{uptime: time.Hour, proxies: healthy, avg1m: 0}, "idle", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveSnapshotState(tc.in); got != tc.wantState {
				t.Fatalf("state = %q, want %q", got, tc.wantState)
			}
			if got := deriveStateReason(tc.in); got != tc.wantReason {
				t.Fatalf("reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

func TestCollectorReportsStartupStuckAndRecovery(t *testing.T) {
	env := &fakeSnapshotEnv{now: snapT0.Add(3 * time.Minute), proxies: SnapshotProxies{}, startup: startupResolving}
	c := newNodeSnapshotCollector(env.sources())
	if snap := c.Get(); snap.State != "starting" || snap.StateReason != "resolving proxies (3 min)" || snap.IdleHint != "" {
		t.Fatalf("state=%q reason=%q hint=%q", snap.State, snap.StateReason, snap.IdleHint)
	}

	env.now = snapT0.Add(6 * time.Minute)
	if snap := c.Get(); snap.State != "degraded" || snap.StateReason != "startup stuck: resolving proxies for 6 min" {
		t.Fatalf("state=%q reason=%q", snap.State, snap.StateReason)
	}

	// Startup finishes: the reason clears and the normal states take over.
	env.startup = ""
	env.proxies = SnapshotProxies{Up: 3}
	env.now = env.now.Add(2 * time.Second)
	if snap := c.Get(); snap.State != "idle" || snap.StateReason != "" {
		t.Fatalf("state=%q reason=%q, want idle with no state reason", snap.State, snap.StateReason)
	}
}

// The snapshot's startup phase and the systemd STATUS= line must never
// disagree. The phase is defined from the same three inputs the line's
// zero-proxy branch reads, so every combination is checked against the line.
func TestStartupPhaseNeverDisagreesWithTheSystemdLine(t *testing.T) {
	t.Cleanup(func() { proxiesConfigured.Store(0); resetProxyResolutionStatus() })
	statuses := map[string]int32{"pending": proxyResolutionPending, "failed": proxyResolutionFailed, "empty": proxyResolutionEmpty, "ok": proxyResolutionOK, "zero-valid": proxyResolutionZeroValid, "no-source": proxyResolutionNoSource}
	for _, count := range []int{0, 1, 5} {
		for name, status := range statuses {
			t.Run(fmt.Sprintf("count=%d/%s", count, name), func(t *testing.T) {
				proxiesConfigured.Store(int64(count))
				proxyResolutionStatus.Store(status)
				phase, line := proxyStartupPhase(), systemdStatusLine()
				switch {
				case count > 0:
					if phase != "" {
						t.Fatalf("proxies are configured but the phase is %q (line %q)", phase, line)
					}
				case phase == startupResolving && !strings.HasPrefix(line, "starting:"):
					t.Fatalf("phase %q but the line says %q", phase, line)
				case (phase == startupSourceUnreachable || phase == startupSourceEmpty) && !strings.HasPrefix(line, "degraded:"):
					t.Fatalf("phase %q but the line says %q", phase, line)
				case phase == "" && (strings.HasPrefix(line, "starting:") || (strings.Contains(line, "source") && !strings.Contains(line, "direct/local"))):
					t.Fatalf("no phase but the line still says %q", line)
				}
			})
		}
	}
}

// The first reload always resolves the status (empty, failed or ok), so a
// zero-proxy node leaves "resolving" within moments and then reads degraded, the
// same as systemd, rather than sitting in "starting".
func TestStartupPhaseAfterTheFirstReloadResolves(t *testing.T) {
	t.Cleanup(func() { proxiesConfigured.Store(0); resetProxyResolutionStatus() })
	resetProxyResolutionStatus()
	proxiesConfigured.Store(0)
	if got := proxyStartupPhase(); got != startupResolving {
		t.Fatalf("before the first reload: %q, want %q", got, startupResolving)
	}
	setProxyResolutionStatus(proxyResolutionEmpty, "source returned no usable proxies")
	if got := proxyStartupPhase(); got != startupSourceEmpty {
		t.Fatalf("a zero-proxy node after its first reload: %q, want %q", got, startupSourceEmpty)
	}
	setConfiguredProxyCount(50)
	setProxyResolutionOK()
	if got := proxyStartupPhase(); got != "" {
		t.Fatalf("resolved with proxies: %q, want settled", got)
	}
}

// A proxy source that fails during the first two minutes is degraded at once,
// like the systemd line, not "starting" with no reason until warmup ends.
func TestSourceFailureIsDegradedEvenDuringWarmup(t *testing.T) {
	for _, phase := range []string{startupSourceUnreachable, startupSourceEmpty} {
		in := stateInputs{uptime: 30 * time.Second, startup: phase}
		if got := deriveSnapshotState(in); got != "degraded" {
			t.Fatalf("%q at 30s: state %q, want degraded", phase, got)
		}
		if got := deriveStateReason(in); got != phase+", retrying" {
			t.Fatalf("%q at 30s: reason %q", phase, got)
		}
	}
	// Still resolving during warmup is ordinary startup.
	if got := deriveSnapshotState(stateInputs{uptime: 30 * time.Second, startup: startupResolving}); got != "starting" {
		t.Fatalf("resolving at 30s: state %q, want starting", got)
	}
}
