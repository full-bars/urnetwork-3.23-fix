package main

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// registerTrafficProxy registers a live proxy with separate total and billable
// byte counts, the way a real provider sees them: most of the total is
// non-billable platform traffic.
func registerTrafficProxy(index int, key string, total uint64, billable uint64) {
	connect.RegisterProxy(index, key, key)
	bw := connect.RegisterProxyBandwidth(index)
	bw.TotalRx.Store(total)
	bw.BillableRx.Store(billable)
}

// The shed rankings must read what a proxy EARNED. On a measured provider the
// total byte count was about 17x the billable count, so a proxy that moved a
// lot of non-billable traffic and earned nothing ranked as a protected earner,
// and a small real earner could be shed ahead of it.
func TestRunningProxyEarnings_UsesBillableNotTotalBytes(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	restore := withGlobalEarningsStore(t)
	defer restore()

	registerTrafficProxy(1, "10.0.0.1:1080", 50<<30, 0)    // busy, earned nothing
	registerTrafficProxy(2, "10.0.0.2:1080", 1<<20, 1<<20) // small, all billable

	earnings := runningProxyEarnings()
	if earnings["10.0.0.1:1080"] != 0 {
		t.Fatalf("a proxy with 50 GiB total and no billable bytes reads as earning %d", earnings["10.0.0.1:1080"])
	}
	if earnings["10.0.0.2:1080"] != 1<<20 {
		t.Fatalf("billable earner reads %d, want %d", earnings["10.0.0.2:1080"], 1<<20)
	}

	state := map[string]ProxyEntry{"10.0.0.1:1080": {Health: "up"}, "10.0.0.2:1080": {Health: "up"}}
	shed := selectWorstRunningProxies(state, nil, earnings, []string{"10.0.0.2:1080", "10.0.0.1:1080"}, 1)
	if len(shed) != 1 || shed[0] != "10.0.0.1:1080" {
		t.Fatalf("shed %v, want the busy non-earner and not the small real earner", shed)
	}
}

// A proxy that earned heavily before this process started, and has not moved a
// billable byte since, is still a proven earner. Ranking on this run alone
// shed it first, which is the case the startup trim already handled.
func TestRunningProxyEarnings_DecayedScoreProtectsAnIdleSinceRestartEarner(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	restore := withGlobalEarningsStore(t)
	defer restore()

	registerTrafficProxy(1, "10.0.0.1:1080", 1<<30, 0) // earned last week, idle this run
	registerTrafficProxy(2, "10.0.0.2:1080", 1<<30, 0) // never earned
	creditEarningsAt(globalProxyEarningsStore, "10.0.0.1:1080", 5<<30, time.Now().Add(-24*time.Hour))

	earnings := runningProxyEarnings()
	if earnings["10.0.0.1:1080"] == 0 {
		t.Fatalf("a proxy with a 5 GiB decayed earnings score reads as idle")
	}
	if earnings["10.0.0.2:1080"] != 0 {
		t.Fatalf("a proxy that never earned reads as %d", earnings["10.0.0.2:1080"])
	}
	state := map[string]ProxyEntry{"10.0.0.1:1080": {Health: "up"}, "10.0.0.2:1080": {Health: "up"}}
	// the earner sorts first by address, so only the earnings can save it
	shed := selectWorstRunningProxies(state, nil, earnings, []string{"10.0.0.1:1080", "10.0.0.2:1080"}, 1)
	if len(shed) != 1 || shed[0] != "10.0.0.2:1080" {
		t.Fatalf("shed %v, want the never-earner; the proven earner must be kept", shed)
	}
}

// This run's billable bytes count even before the store has credited them.
func TestRunningProxyEarnings_ThisRunsBillableCountsBeforeTheStoreHasIt(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	restore := withGlobalEarningsStore(t)
	defer restore()

	registerTrafficProxy(1, "10.0.0.1:1080", 10<<20, 4<<20)
	if got := runningProxyEarnings()["10.0.0.1:1080"]; got != 4<<20 {
		t.Fatalf("earnings = %d, want this run's 4 MiB billable", got)
	}
}

// The larger of the run's billable bytes and the decayed score wins, so neither
// signal can lower the other.
func TestRunningProxyEarnings_TakesTheLargerOfRunAndDecayedScore(t *testing.T) {
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	restore := withGlobalEarningsStore(t)
	defer restore()

	registerTrafficProxy(1, "10.0.0.1:1080", 100<<20, 40<<20)
	creditEarningsAt(globalProxyEarningsStore, "10.0.0.1:1080", 10<<20, time.Now())
	if got := runningProxyEarnings()["10.0.0.1:1080"]; got != 40<<20 {
		t.Fatalf("earnings = %d, want the larger run billable %d", got, 40<<20)
	}
	registerTrafficProxy(2, "10.0.0.2:1080", 100<<20, 1<<20)
	creditEarningsAt(globalProxyEarningsStore, "10.0.0.2:1080", 30<<20, time.Now())
	if got := runningProxyEarnings()["10.0.0.2:1080"]; got < 29<<20 {
		t.Fatalf("earnings = %d, want about the larger decayed score %d", got, 30<<20)
	}
}
