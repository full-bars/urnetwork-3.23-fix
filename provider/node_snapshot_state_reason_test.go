package main

import (
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

// A direct-only node has no proxies by design. Once the provider publishes its
// (zero) configured count, startup is settled and it must not read "starting".
func TestStartupPhaseSettlesForADirectOnlyNode(t *testing.T) {
	resetProxyResolutionStatus()
	proxiesConfiguredSet.Store(false)
	t.Cleanup(func() { proxiesConfiguredSet.Store(false); resetProxyResolutionStatus() })

	if got := proxyStartupPhase(); got != startupResolving {
		t.Fatalf("before the configured count is published: phase = %q, want %q", got, startupResolving)
	}
	setConfiguredProxyCount(0)
	if got := proxyStartupPhase(); got != "" {
		t.Fatalf("after publishing a zero count (direct-only): phase = %q, want settled", got)
	}
}

func TestStartupPhaseReportsSourceFailureOnlyWithNoProxies(t *testing.T) {
	t.Cleanup(func() { proxiesConfiguredSet.Store(false); proxiesConfigured.Store(0); resetProxyResolutionStatus() })

	setConfiguredProxyCount(0)
	setProxyResolutionStatus(proxyResolutionFailed, "boom")
	if got := proxyStartupPhase(); got != startupSourceUnreachable {
		t.Fatalf("no proxies + failed source: phase = %q", got)
	}
	setProxyResolutionStatus(proxyResolutionEmpty, "")
	if got := proxyStartupPhase(); got != startupSourceEmpty {
		t.Fatalf("no proxies + empty source: phase = %q", got)
	}
	// With proxies running, a failed refresh is not a startup problem.
	setConfiguredProxyCount(50)
	if got := proxyStartupPhase(); got != "" {
		t.Fatalf("proxies configured: phase = %q, want settled", got)
	}
}
