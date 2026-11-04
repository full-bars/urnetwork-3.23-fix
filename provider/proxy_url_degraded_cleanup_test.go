package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func noteLiveProxyHealth(key, health string, downSince time.Time) {
	connect.NoteLiveProxyHealth(key, health, downSince)
}

func noteLiveProxyHealthUp(key string) {
	connect.NoteLiveProxyHealth(key, "up", time.Time{})
}

func noteLiveProxyHealthFailure(key string) {
	connect.NoteLiveProxyHealth(key, "recently_offline", time.Now())
}

func resetProxyHealthReportStateForTest() {
	connect.ResetProxyHealthForTesting()
}

// TestDegradedCleanup_UnsetThresholdRetiresNothing verifies that when
// DegradedCleanupThreshold is unset or empty, degraded eviction is opt-in:
// with no threshold configured, a member that has been degraded for longer
// than any plausible window is still kept. Deleting proxy capacity must
// never be the default behaviour.
func TestDegradedCleanup_UnsetThresholdRetiresNothing(t *testing.T) {
	withTempHome(t)

	oldAddr := "1.1.1.1:1080"
	youngAddr := "2.2.2.2:1080"

	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{
			oldAddr:   {},
			youngAddr: {},
		},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	state := &ProxyState{
		StartedAt: now.Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			oldAddr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: now.Add(-7 * time.Hour).UTC().Format(time.RFC3339),
			},
			youngAddr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: now.Add(-5 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("removed = %d with no threshold configured, want 0", removed)
	}

	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[oldAddr]; !ok {
		t.Errorf("expected %s (degraded 7h) to be KEPT when eviction is off", oldAddr)
	}
	if _, ok := urlState.Cache[youngAddr]; !ok {
		t.Errorf("expected %s to be kept when eviction is off", youngAddr)
	}
}

// TestDegradedCleanup_ExplicitThresholdWins verifies that an explicitly configured
// threshold wins over the 6-hour default, both when shorter and when longer.
func TestDegradedCleanup_ExplicitThresholdWins(t *testing.T) {
	withTempHome(t)

	now := time.Now()

	// Subtest 1: Shorter explicit threshold (1h) evicts a member degraded for 2h
	// which would have been kept by the 6h default.
	t.Run("shorter_wins", func(t *testing.T) {
		addr := "10.0.0.1:1080"
		if err := writeProxyURLState(&ProxyURLState{
			DegradedCleanupThreshold: "1h",
			Cache: map[string]ProxyURLEntry{
				addr: {},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeProxyState(&ProxyState{
			StartedAt: now.Add(-70 * time.Minute),
			Proxies: map[string]ProxyEntry{
				addr: {
					Health:    "offline",
					Source:    "url",
					DownSince: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		removed := runProxyURLCleanupOnce("url")
		if removed != 1 {
			t.Fatalf("removed = %d, want 1 with 1h explicit threshold", removed)
		}
		urlState, _ := readProxyURLState()
		if _, ok := urlState.Cache[addr]; ok {
			t.Errorf("expected %s to be evicted with 1h threshold", addr)
		}
	})

	// Subtest 2: Longer explicit threshold (12h) preserves a member degraded for 8h
	// which would have been evicted by the 6h default, but removes one degraded for 14h.
	t.Run("longer_wins", func(t *testing.T) {
		eightHourAddr := "10.0.0.8:1080"
		fourteenHourAddr := "10.0.0.14:1080"
		if err := writeProxyURLState(&ProxyURLState{
			DegradedCleanupThreshold: "12h",
			Cache: map[string]ProxyURLEntry{
				eightHourAddr:    {},
				fourteenHourAddr: {},
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeProxyState(&ProxyState{
			StartedAt: now.Add(-70 * time.Minute),
			Proxies: map[string]ProxyEntry{
				eightHourAddr: {
					Health:    "long_offline",
					Source:    "url",
					DownSince: now.Add(-8 * time.Hour).UTC().Format(time.RFC3339),
				},
				fourteenHourAddr: {
					Health:    "long_offline",
					Source:    "url",
					DownSince: now.Add(-14 * time.Hour).UTC().Format(time.RFC3339),
				},
			},
		}); err != nil {
			t.Fatal(err)
		}

		removed := runProxyURLCleanupOnce("url")
		if removed != 1 {
			t.Fatalf("removed = %d, want 1 with 12h explicit threshold", removed)
		}
		urlState, _ := readProxyURLState()
		if _, ok := urlState.Cache[eightHourAddr]; !ok {
			t.Errorf("expected %s (8h down) to be kept with 12h threshold", eightHourAddr)
		}
		if _, ok := urlState.Cache[fourteenHourAddr]; ok {
			t.Errorf("expected %s (14h down) to be evicted with 12h threshold", fourteenHourAddr)
		}
	})
}

// TestDegradedCleanup_HiccupNotRetired verifies that a member graded seven hours
// ago that blipped or degraded recently (e.g. 10m ago or legacy without DownSince)
// is NOT retired based on LastGraded. LastGraded reflects when a proxy was alive
// and working, not when it went down.
func TestDegradedCleanup_HiccupNotRetired(t *testing.T) {
	withTempHome(t)

	addr := "20.0.0.1:1080"
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{addr: {}},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	// Member was last graded 7h ago while alive and working, and has empty DownSince on disk (legacy).
	// It must NOT be backfilled from LastGraded and instantly deleted.
	if err := writeProxyState(&ProxyState{
		StartedAt: now.Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			addr: {
				Health:     "recently_offline",
				Source:     "url",
				LastGraded: now.Add(-7 * time.Hour),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("hiccup member was retired: removed = %d, want 0", removed)
	}

	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[addr]; !ok {
		t.Fatalf("hiccup member %s was evicted from cache", addr)
	}
}

// TestDegradedCleanup_LegacyDownSinceStampedOncePerPass verifies that the pass,
// not the read, stamps legacy degraded members: every one gets the same single
// timestamp, it reaches the disk, and a later pass keeps it rather than
// generating a fresh one.
func TestDegradedCleanup_LegacyDownSinceStampedOncePerPass(t *testing.T) {
	withTempHome(t)

	a, b := "20.0.0.2:1080", "20.0.0.3:1080"
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{
			a: {Health: "recently_offline", Source: "url"},
			b: {Health: "offline", Source: "url"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	runProxyURLCleanupOnce("url")
	first, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	stamp := first.Proxies[a].DownSince
	if stamp == "" {
		t.Fatalf("%s DownSince was not stamped by the pass", a)
	}
	if got := first.Proxies[b].DownSince; got != stamp {
		t.Fatalf("members stamped from different clocks: %q vs %q", got, stamp)
	}

	runProxyURLCleanupOnce("url")
	second, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Proxies[a].DownSince; got != stamp {
		t.Fatalf("second pass re-stamped %s: %q, want persisted %q", a, got, stamp)
	}
}

// TestProxyState_ReadLeavesBytesUnchanged verifies a read never writes: a file
// holding a degraded entry with no stamp is byte-identical after the read.
func TestProxyState_ReadLeavesBytesUnchanged(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{
			"20.0.0.4:1080": {Health: "recently_offline", Source: "url"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	path, err := proxyStatePath()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Proxies["20.0.0.4:1080"].DownSince; got != "" {
		t.Fatalf("read stamped DownSince = %q, want it left empty", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("read changed proxy.state:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestProxyState_ReaderCannotClobberLockedWriter races readers against a writer
// that holds the state locks and commits a marker (NextID) each trial. The file
// the writer commits always carries unstamped degraded entries, which is what
// made the old read path write back its stale snapshot. A pure read can never
// lose the writer's value. The interleaving is probabilistic with the old
// code (a reader must land between the writer's commit and its own write) so
// the file is large to widen that window; with the fix it cannot fail.
func TestProxyState_ReaderCannotClobberLockedWriter(t *testing.T) {
	withTempHome(t)

	members := make(map[string]ProxyEntry, 3000)
	for i := 0; i < 3000; i++ {
		members[fmt.Sprintf("10.%d.%d.1:1080", i/250, i%250)] = ProxyEntry{Health: "recently_offline", Source: "url"}
	}
	for trial := 1; trial <= 100; trial++ {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var reads atomic.Int64
		for r := 0; r < 4; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						readProxyState()
						reads.Add(1)
					}
				}
			}()
		}
		// Commit only once the readers are in flight, so some read is
		// concurrent with the commit.
		for reads.Load() < 4 {
			runtime.Gosched()
		}
		// Several commits back to back, each carrying unstamped entries, so a
		// reader that read one is likely to write after the next.
		var err error
		for k := 0; k < 5 && err == nil; k++ {
			proxyStateMu.Lock()
			err = writeProxyState(&ProxyState{NextID: trial*10 + k, Proxies: members})
			proxyStateMu.Unlock()
		}
		close(stop)
		wg.Wait()
		if err != nil {
			t.Fatal(err)
		}
		st, err := readProxyState()
		if err != nil {
			t.Fatal(err)
		}
		if want := trial*10 + 4; st.NextID != want {
			t.Fatalf("trial %d: writer's last NextID=%d was overwritten by a reader, file has %d", trial, want, st.NextID)
		}
	}
}

// TestDegradedCleanup_FailedStampWriteReportedOnce verifies a failed state
// write is reported once for the pass, is not retried within it, and leaves the
// freshly stamped members in place instead of retiring them.
func TestDegradedCleanup_FailedStampWriteReportedOnce(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{
		DegradedCleanupThreshold: "1h",
		Cache:                    map[string]ProxyURLEntry{"20.0.0.5:1080": {}, "20.0.0.6:1080": {}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{
			"20.0.0.5:1080": {Health: "recently_offline", Source: "url"},
			"20.0.0.6:1080": {Health: "offline", Source: "url"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	writes := 0
	prev := cleanupWriteState
	cleanupWriteState = func(*ProxyState) error {
		writes++
		return errors.New("disk full")
	}
	defer func() { cleanupWriteState = prev }()

	var removed int
	out := captureTlog(t, func() { removed = runProxyURLCleanupOnce("url") })
	if writes != 1 {
		t.Fatalf("state writes = %d, want exactly 1 per pass", writes)
	}
	if n := strings.Count(out, "could not persist down_since"); n != 1 {
		t.Fatalf("warning logged %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "disk full") {
		t.Fatalf("warning does not carry the reason:\n%s", out)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0: members stamped this pass are not old", removed)
	}
}

// TestDegradedCleanup_TrimHeldClockRestartsNotFrozen is the held-member case:
// a degraded member the trim cap holds back is not running, so it accrues no
// downtime. Its clock restarts each pass, and raising the cap does not collect
// it on the age it had when it was held.
func TestDegradedCleanup_TrimHeldClockRestartsNotFrozen(t *testing.T) {
	withTempHome(t)

	upAddr, heldAddr := "20.0.0.10:1080", "20.0.0.11:1080"
	if err := writeProxyURLState(&ProxyURLState{
		DegradedCleanupThreshold: "6h",
		Cache:                    map[string]ProxyURLEntry{upAddr: {}, heldAddr: {}},
	}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{
			upAddr:   {Health: "up", Source: "url"},
			heldAddr: {Health: "recently_offline", Source: "url", DownSince: old},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}

	if removed := runProxyURLCleanupOnce("url"); removed != 0 {
		t.Fatalf("held member retired under cap=1: removed = %d, want 0", removed)
	}
	st, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	ds, err := time.Parse(time.RFC3339, st.Proxies[heldAddr].DownSince)
	if err != nil || time.Since(ds) > time.Minute {
		t.Fatalf("held member DownSince = %q, want it restarted to now (was %q)", st.Proxies[heldAddr].DownSince, old)
	}

	// Raise the cap without the member ever having run.
	if err := writeTrimTarget(2); err != nil {
		t.Fatal(err)
	}
	if removed := runProxyURLCleanupOnce("url"); removed != 0 {
		t.Fatalf("raising the cap collected a never-relaunched member on stale age: removed = %d, want 0", removed)
	}
	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[heldAddr]; !ok {
		t.Fatalf("member %s evicted from the cache after the cap was raised", heldAddr)
	}
}

// TestDegradedCleanup_TrimHeldDeadStillRetired verifies the hold covers only
// the degraded branch: a dead member the cap holds back is still collected.
func TestDegradedCleanup_TrimHeldDeadStillRetired(t *testing.T) {
	withTempHome(t)

	upAddr, deadAddr := "20.0.0.12:1080", "20.0.0.13:1080"
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{upAddr: {}, deadAddr: {}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{
			upAddr:   {Health: "up", Source: "url"},
			deadAddr: {Health: "dead", Source: "url"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeTrimTarget(1); err != nil {
		t.Fatal(err)
	}
	if removed := runProxyURLCleanupOnce("url"); removed != 1 {
		t.Fatalf("removed = %d, want the held dead member collected", removed)
	}
}

// TestDegradedCleanup_HeldSetMatchesAdmission verifies cleanup holds exactly
// what admission holds when the live health overlay disagrees with the disk:
// the disk says A is degraded and B up, the live registry says the reverse.
func TestDegradedCleanup_HeldSetMatchesAdmission(t *testing.T) {
	withTempHome(t)
	resetProxyHealthReportStateForTest()

	a, b := "20.0.0.20:1080", "20.0.0.21:1080"
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		a: {Health: "recently_offline", Source: "url", DownSince: time.Now().UTC().Format(time.RFC3339)},
		b: {Health: "up", Source: "url"},
	}}
	noteLiveProxyHealthUp(a)
	noteLiveProxyHealthFailure(b)

	// Admission's inputs: the overlay applied to the state it ranks.
	admitted := map[string]ProxyEntry{}
	for k, e := range state.Proxies {
		admitted[k] = e
	}
	overlayLiveProxyHealth(admitted)
	all := []*connect.ProxySettings{
		{Network: "tcp", Address: a},
		{Network: "tcp", Address: b},
	}
	_, held := startupTrimSelection(all, 1, admitted, nil, nil)
	if len(held) != 1 || held[0].Key() != b {
		t.Fatalf("test premise broken: admission holds %v, want [%s]", held, b)
	}

	got := cleanupTrimHeld(state, map[string]bool{a: true, b: true}, 1, time.Now())
	if len(got) != 1 || !got[b] {
		t.Fatalf("cleanup holds %v, admission holds %s", got, b)
	}
	if state.Proxies[a].Health != "recently_offline" {
		t.Fatalf("the overlay leaked into the persisted copy: %+v", state.Proxies[a])
	}
}

// TestDegradedCleanup_UnparseableThresholdWarns verifies that when degraded_cleanup_threshold
// cannot be parsed or is negative/non-positive, a warning naming the value and fallback window
// is logged, and degraded eviction stays off.
func TestDegradedCleanup_UnparseableThresholdWarns(t *testing.T) {
	withTempHome(t)

	for _, bad := range []string{"not-a-duration", "-5m", "0s"} {
		if err := writeProxyURLState(&ProxyURLState{
			DegradedCleanupThreshold: bad,
		}); err != nil {
			t.Fatal(err)
		}

		out := captureTlog(t, func() {
			runProxyURLCleanupOnce("url")
		})
		if !strings.Contains(out, bad) {
			t.Fatalf("expected warning for bad threshold %q, got:\n%s", bad, out)
		}
		if !strings.Contains(out, "leaving degraded eviction off") {
			t.Fatalf("expected warning to say eviction stays off, got:\n%s", out)
		}
	}
}

// TestDegradedCleanup_FreshlyDegradedUntouched verifies that a degraded member
// whose DownSince is well inside the window is untouched.
func TestDegradedCleanup_FreshlyDegradedUntouched(t *testing.T) {
	withTempHome(t)

	addr := "30.0.0.1:1080"
	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{addr: {}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			addr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("freshly degraded member removed: %d, want 0", removed)
	}
	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[addr]; !ok {
		t.Fatalf("freshly degraded member %s was unexpectedly removed", addr)
	}
}

// TestDegradedCleanup_InternalSourceNeverRemoved verifies that internal-source
// members in degraded state are never removed by URL cleanup, even when degraded
// for days.
func TestDegradedCleanup_InternalSourceNeverRemoved(t *testing.T) {
	withTempHome(t)

	internalAddr := "40.0.0.1:1080"
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-70 * time.Minute),
		Proxies: map[string]ProxyEntry{
			internalAddr: {
				Health:    "recently_offline",
				Source:    "internal",
				DownSince: time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("internal-source degraded member removed: %d, want 0", removed)
	}
	st, _ := readProxyState()
	if _, ok := st.Proxies[internalAddr]; !ok {
		t.Fatalf("internal member %s was removed from proxy.state", internalAddr)
	}
}

// TestDegradedCleanup_UptimeGuardHolds verifies that the 65-minute uptime guard
// prevents removal of degraded members during provider warmup.
func TestDegradedCleanup_UptimeGuardHolds(t *testing.T) {
	withTempHome(t)

	addr := "50.0.0.1:1080"
	// A window is configured on purpose: without one, eviction is off and this
	// test would pass without ever exercising the warmup guard it exists for.
	if err := writeProxyURLState(&ProxyURLState{
		Cache:                    map[string]ProxyURLEntry{addr: {}},
		DegradedCleanupThreshold: "1h",
	}); err != nil {
		t.Fatal(err)
	}
	// Warmup: StartedAt is 10 minutes ago (< 65m minUptime).
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-10 * time.Minute),
		Proxies: map[string]ProxyEntry{
			addr: {
				Health:    "recently_offline",
				Source:    "url",
				DownSince: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	removed := runProxyURLCleanupOnce("url")
	if removed != 0 {
		t.Fatalf("warmup cleanup removed %d proxies, want 0", removed)
	}
	urlState, _ := readProxyURLState()
	if _, ok := urlState.Cache[addr]; !ok {
		t.Fatalf("degraded member %s was evicted during warmup", addr)
	}

	// Settled: StartedAt is 70 minutes ago (> 65m minUptime).
	st, _ := readProxyState()
	st.StartedAt = time.Now().Add(-70 * time.Minute)
	if err := writeProxyState(st); err != nil {
		t.Fatal(err)
	}

	removedSettled := runProxyURLCleanupOnce("url")
	if removedSettled != 1 {
		t.Fatalf("settled cleanup removed %d proxies, want 1", removedSettled)
	}
	urlState2, _ := readProxyURLState()
	if _, ok := urlState2.Cache[addr]; ok {
		t.Fatalf("degraded member %s was not evicted after warmup elapsed", addr)
	}
}

// TestDegradedCleanup_FrozenPoolDoesNotAgeTheClock: a degraded member that
// ages past the window while the reaper is frozen has not been able to recover,
// so releasing the freeze must not collect it on that age. It starts a fresh
// window and is collected only once that window expires unfrozen.
func TestDegradedCleanup_FrozenPoolDoesNotAgeTheClock(t *testing.T) {
	withTempHome(t)

	addr := "21.0.0.10:1080"
	if err := writeProxyURLState(&ProxyURLState{
		DegradedCleanupThreshold: "6h",
		Cache:                    map[string]ProxyURLEntry{addr: {}},
	}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{addr: {Health: "recently_offline", Source: "url", DownSince: old}},
	}); err != nil {
		t.Fatal(err)
	}

	// The live registry carries the same old clock; a later health write would
	// copy it back over the restarted stamp if the freeze left it alone.
	noteLiveProxyHealth(addr, "recently_offline", time.Now().Add(-48*time.Hour))
	t.Cleanup(func() {
		connect.UnregisterProxyByKey(addr)
	})

	prev := degradedReaperFrozen
	t.Cleanup(func() { degradedReaperFrozen = prev })
	degradedReaperFrozen = func() bool { return true }
	if removed := runProxyURLCleanupOnce("url"); removed != 0 {
		t.Fatalf("frozen pass removed %d, want 0", removed)
	}
	liveDown := connect.ProxyHealthByKey()[addr].DownSince
	if time.Since(liveDown) > time.Minute {
		t.Fatalf("live registry DownSince = %v after a frozen pass, want it restarted to now", liveDown)
	}
	st, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	ds, err := time.Parse(time.RFC3339, st.Proxies[addr].DownSince)
	if err != nil || time.Since(ds) > time.Minute {
		t.Fatalf("DownSince after a frozen pass = %q, want it restarted to now (was %q)", st.Proxies[addr].DownSince, old)
	}

	degradedReaperFrozen = func() bool { return false }
	if removed := runProxyURLCleanupOnce("url"); removed != 0 {
		t.Fatalf("release collected a member on age it accrued while frozen: removed = %d, want 0", removed)
	}

	// The fresh window expires with the reaper unfrozen.
	st, err = readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	e := st.Proxies[addr]
	e.DownSince = time.Now().Add(-7 * time.Hour).UTC().Format(time.RFC3339)
	st.Proxies[addr] = e
	if err := writeProxyState(st); err != nil {
		t.Fatal(err)
	}
	if removed := runProxyURLCleanupOnce("url"); removed != 1 {
		t.Fatalf("member past its fresh window removed = %d, want 1", removed)
	}
}
