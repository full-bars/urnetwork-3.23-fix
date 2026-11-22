package main

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect"
)

func TestSelectUDPProbeDueBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		status proxyUDPStatus
		ttl    time.Duration
	}{
		{"ok", proxyUDPOK, 24 * time.Hour},
		{"no-relay", proxyUDPNoRelay, 72 * time.Hour},
		{"mismatch", proxyUDPExitMismatch, 24 * time.Hour},
		{"unknown", proxyUDPUnknown, time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			candidates := []udpProbeCandidate{
				{key: "before", online: true, status: tt.status, lastChecked: now.Add(-tt.ttl/2 + time.Nanosecond)},
				{key: "at", online: true, status: tt.status, lastChecked: now.Add(-tt.ttl / 2)},
				{key: "after", online: true, status: tt.status, lastChecked: now.Add(-tt.ttl/2 - time.Nanosecond)},
				{key: "offline", status: tt.status},
			}
			got := selectUDPProbeBatch(now, candidates, time.Minute, time.Minute, rand.New(rand.NewSource(1)))
			if len(got) != 2 || got[0].key != "after" || got[1].key != "at" {
				t.Fatalf("due candidates = %v, want after, at", got)
			}
		})
	}
}

func TestSelectUDPProbeBatchSizes(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct{ online, want int }{{1, 1}, {7, 1}, {4750, 4}} {
		t.Run(fmt.Sprint(tt.online), func(t *testing.T) {
			candidates := make([]udpProbeCandidate, tt.online)
			for i := range candidates {
				candidates[i] = udpProbeCandidate{key: fmt.Sprint(i), online: true}
			}
			got := selectUDPProbeBatch(now, candidates, 24*time.Hour, time.Minute, rand.New(rand.NewSource(42)))
			if len(got) != tt.want {
				t.Fatalf("batch size = %d, want %d", len(got), tt.want)
			}
		})
	}
}

func TestSelectUDPProbeJitter(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	candidates := make([]udpProbeCandidate, 150)
	for i := range candidates {
		candidates[i] = udpProbeCandidate{key: fmt.Sprint(i), online: true}
	}
	seenLow, seenHigh := false, false
	for seed := int64(0); seed < 1000; seed++ {
		got := selectUDPProbeBatch(now, candidates, 3*time.Minute, time.Minute, rand.New(rand.NewSource(seed)))
		if len(got) < 40 || len(got) > 60 {
			t.Fatalf("seed %d: size %d outside [40,60]", seed, len(got))
		}
		seenLow = seenLow || len(got) < 50
		seenHigh = seenHigh || len(got) > 50
	}
	if !seenLow || !seenHigh {
		t.Fatal("jitter did not vary on both sides of the base size")
	}
	// A tick longer than the window still uses the uncapped formula before
	// jitter; 100 * 11/10 gives a base of 110, with a lower bound of 88.
	seenBelowCap := false
	for seed := int64(0); seed < 1000; seed++ {
		got := selectUDPProbeBatch(now, candidates[:100], 10*time.Minute, 11*time.Minute, rand.New(rand.NewSource(seed)))
		if len(got) < 88 || len(got) > 100 {
			t.Fatalf("long tick seed %d: size %d outside [88,100]", seed, len(got))
		}
		seenBelowCap = seenBelowCap || len(got) < 100
	}
	if !seenBelowCap {
		t.Fatal("long tick jitter never dropped below the due cap")
	}
}

func TestSelectUDPProbeOrdering(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	candidates := []udpProbeCandidate{
		{key: "tie-b", online: true, lastChecked: now.Add(-48 * time.Hour)},
		{key: "new-b", online: true},
		{key: "oldest", online: true, lastChecked: now.Add(-72 * time.Hour)},
		{key: "tie-a", online: true, lastChecked: now.Add(-48 * time.Hour)},
		{key: "new-a", online: true},
	}
	original := append([]udpProbeCandidate(nil), candidates...)
	got := selectUDPProbeBatch(now, candidates, time.Minute, time.Minute, nil)
	var keys []string
	for _, candidate := range got {
		keys = append(keys, candidate.key)
	}
	want := []string{"new-a", "new-b", "oldest", "tie-a", "tie-b"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("order = %v, want %v", keys, want)
	}
	if !reflect.DeepEqual(candidates, original) {
		t.Fatal("selection modified the input")
	}
}

func TestSelectUDPProbeEmptyAndLimits(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	fresh := []udpProbeCandidate{{key: "fresh", online: true, status: proxyUDPOK, lastChecked: now}}
	for _, tt := range []struct {
		name         string
		candidates   []udpProbeCandidate
		window, tick time.Duration
	}{
		{"empty", nil, time.Hour, time.Minute},
		{"not due", fresh, time.Hour, time.Minute},
		{"zero window", []udpProbeCandidate{{online: true}}, 0, time.Minute},
		{"zero tick", []udpProbeCandidate{{online: true}}, time.Hour, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectUDPProbeBatch(now, tt.candidates, tt.window, tt.tick, rand.New(rand.NewSource(1))); len(got) != 0 {
				t.Fatalf("got %v, want no candidates", got)
			}
		})
	}
	candidates := make([]udpProbeCandidate, 100)
	for i := range candidates {
		candidates[i] = udpProbeCandidate{online: true, status: proxyUDPOK, lastChecked: now}
	}
	candidates[0].lastChecked = time.Time{}
	if got := selectUDPProbeBatch(now, candidates, time.Minute, time.Minute, rand.New(rand.NewSource(2))); len(got) != 1 {
		t.Fatalf("due cap = %d, want 1", len(got))
	}
}

// The selector orders by the earlier due time, so neither kind of due time has a
// standing priority: the oldest outstanding due goes first.
func TestSelectUDPProbeBatchOrdersByTheEarliestDueTime(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

	ipv6Earliest := []udpProbeCandidate{
		{key: "ipv6-earlier", online: true, status: proxyUDPOK, lastChecked: now.Add(-time.Minute), ipv6Due: true, ipv6DueAt: now.Add(-30 * time.Minute)},
		{key: "udp-later", online: true, status: proxyUDPOK, lastChecked: now.Add(-11 * time.Hour)},
	}
	batch := selectUDPProbeBatch(now, ipv6Earliest, 24*time.Hour, time.Minute, nil)
	if len(batch) != 1 || batch[0].key != "ipv6-earlier" {
		t.Fatalf("batch = %v, want the identity whose ipv6 retry is the oldest due", batch)
	}

	udpEarliest := []udpProbeCandidate{
		{key: "udp-earlier", online: true, status: proxyUDPOK, lastChecked: now.Add(-20 * time.Hour)},
		{key: "ipv6-later", online: true, status: proxyUDPOK, lastChecked: now.Add(-time.Minute), ipv6Due: true, ipv6DueAt: now.Add(-10 * time.Minute)},
	}
	batch = selectUDPProbeBatch(now, udpEarliest, 24*time.Hour, time.Minute, nil)
	if len(batch) != 1 || batch[0].key != "udp-earlier" {
		t.Fatalf("batch = %v, want the identity whose udp recheck is the oldest due", batch)
	}
}

// Sol's trace: four identities hold the batch with IPv6 retries that are only
// due soon, and one identity's udp evidence expired hours ago. The expired one
// is the earliest due, so it takes the single slot.
func TestSelectUDPProbeBatchSelectsTheOldestDueOverIpv6Retries(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	var candidates []udpProbeCandidate
	for i := 0; i < 4; i++ {
		candidates = append(candidates, udpProbeCandidate{
			key:         fmt.Sprintf("retry-%d", i),
			online:      true,
			status:      proxyUDPNoRelay,
			lastChecked: now.Add(-time.Minute),
			ipv6Due:     true,
			// the stage stopped fifteen minutes before this tick
			ipv6DueAt: now.Add(15 * time.Minute),
		})
	}
	candidates = append(candidates, udpProbeCandidate{
		key:         "expired-udp",
		online:      true,
		status:      proxyUDPOK,
		lastChecked: now.Add(-25 * time.Hour),
	})
	batch := selectUDPProbeBatch(now, candidates, 24*time.Hour, 5*time.Minute, nil)
	if len(batch) != 1 {
		t.Fatalf("batch size = %d, want the budget of 1", len(batch))
	}
	if batch[0].key != "expired-udp" {
		t.Fatalf("first candidate = %q, want the identity whose udp evidence expired first", batch[0].key)
	}
}

// Equal due times fall back to the older check, then the key.
func TestSelectUDPProbeBatchBreaksDueTimeTies(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	same := now.Add(-20 * time.Hour)
	candidates := []udpProbeCandidate{
		{key: "b", online: true, status: proxyUDPOK, lastChecked: same},
		{key: "a", online: true, status: proxyUDPOK, lastChecked: same},
		{key: "older", online: true, status: proxyUDPOK, lastChecked: now.Add(-21 * time.Hour)},
	}
	batch := selectUDPProbeBatch(now, candidates, 24*time.Hour, 5*time.Minute, nil)
	if len(batch) != 1 || batch[0].key != "older" {
		t.Fatalf("batch = %v, want the older check first", batch)
	}
	two := selectUDPProbeBatch(now, candidates, 24*time.Hour, 12*time.Hour, nil)
	if len(two) < 2 || two[1].key != "a" {
		t.Fatalf("batch = %v, want the key order to break a due time tie", two)
	}
}

// An identity that was never checked must get a measurement even while IPv6
// retries are due. IPv6-due candidates sorted first, so with a small budget they
// can fill every batch and an unchecked identity never runs.
func TestSelectUDPProbeBatchDoesNotStarveAnUncheckedIdentity(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	var candidates []udpProbeCandidate
	for i := 0; i < 20; i++ {
		// measured a minute ago, so only the IPv6 retry makes it due, and it
		// stays un-held: this is the state a stage that ran and learned nothing
		// leaves behind
		candidates = append(candidates, udpProbeCandidate{
			key:         fmt.Sprintf("v6-%02d", i),
			online:      true,
			status:      proxyUDPOK,
			lastChecked: now.Add(-time.Minute),
			ipv6Due:     true,
		})
	}
	candidates = append(candidates, udpProbeCandidate{key: "unchecked", online: true, status: proxyUDPUnknown})

	seen := map[string]bool{}
	for tick := 0; tick < 10; tick++ {
		at := now.Add(time.Duration(tick) * time.Minute)
		batch := selectUDPProbeBatch(at, candidates, 24*time.Hour, time.Minute, nil)
		if len(batch) != 1 {
			t.Fatalf("tick %d selected %d candidates, want the budget of 1", tick, len(batch))
		}
		for _, candidate := range batch {
			seen[candidate.key] = true
		}
	}
	if !seen["unchecked"] {
		t.Fatal("an unchecked identity never reached a batch while the ipv6 retries were due")
	}
}

// A first check the tick deadline cancels publishes nothing and leaves no
// entry, so nothing records that the identity had its turn. The selector keeps
// giving it the only slot, and the identity beside it never runs.
func TestCancelledFirstCheckReleasesTheOnlySlot(t *testing.T) {
	pinSyntheticStageClock(t)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	a := &connect.ProxySettings{Network: "tcp", Address: "a.example.test:1080"}
	b := &connect.ProxySettings{Network: "tcp", Address: "b.example.test:1080"}
	online := []*connect.ProxySettings{a, b}
	store := newProxyUDPCheckStore(nil)
	var cancel context.CancelFunc
	probed := []string{}
	store.probe = func(_ context.Context, settings *connect.ProxySettings) proxyUDPResult {
		probed = append(probed, settings.Address)
		if settings.Key() == a.Key() {
			// the tick deadline cancels the check before it can publish
			cancel()
			return proxyUDPResult{Status: proxyUDPUnknown}
		}
		return proxyUDPResult{Status: proxyUDPOK}
	}
	for tick := 0; tick < 2; tick++ {
		at := now.Add(time.Duration(tick) * 5 * time.Minute)
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		runUDPProbeTick(ctx, store, online, at, udpProbeRecheckWindow, 5*time.Minute, 0, true)
		cancel()
		if store.size() != 0 && tick == 0 {
			t.Fatalf("a cancelled first check left %d entries behind", store.size())
		}
	}
	if len(probed) != 2 {
		t.Fatalf("two ticks measured %v, want one identity each", probed)
	}
	if probed[0] != a.Address || probed[1] != b.Address {
		t.Fatalf("the two ticks measured %v, want %q and then %q: the identity beside a cancelled first check never reached the only slot", probed, a.Address, b.Address)
	}
}

// Every check below is cancelled by its tick deadline, so no identity ever
// publishes. The oldest attempt goes first, so the single slot rotates through
// all of them instead of staying with the identity that sorts first.
func TestCancelledAttemptsRotateTheOnlySlot(t *testing.T) {
	pinSyntheticStageClock(t)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	const identities = 9
	online := make([]*connect.ProxySettings, 0, identities)
	for i := 0; i < identities; i++ {
		online = append(online, &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("%02d.example.test:1080", i)})
	}
	store := newProxyUDPCheckStore(nil)
	var cancel context.CancelFunc
	var order []string
	store.probe = func(_ context.Context, settings *connect.ProxySettings) proxyUDPResult {
		order = append(order, settings.Address)
		cancel()
		return proxyUDPResult{Status: proxyUDPUnknown}
	}
	for tick := 0; tick < 3*identities; tick++ {
		at := now.Add(time.Duration(tick) * 5 * time.Minute)
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		runUDPProbeTick(ctx, store, online, at, udpProbeRecheckWindow, 5*time.Minute, 0, true)
		cancel()
		if store.size() != 0 {
			t.Fatalf("tick %d left %d entries after a cancelled first check", tick, store.size())
		}
	}
	if len(order) != 3*identities {
		t.Fatalf("three rounds measured %d identities, want %d", len(order), 3*identities)
	}
	for i := range order {
		if order[i] != order[i%identities] {
			t.Fatalf("selection sequence %v does not rotate by attempt time", order)
		}
	}
	seen := map[string]bool{}
	for _, address := range order {
		seen[address] = true
	}
	if len(seen) != identities {
		t.Fatalf("%d of %d identities never reached the only slot: %v", identities-len(seen), identities, order)
	}
}

// The freshness test that makes a UDP recheck due reads the entry's own stamp,
// monotonic reading included; only the sort keys take the strip. Rounding the
// stamp to the wall clock before the test drops that reading, so after a backward
// wall step an entry that is seven hours old on the elapsed clock reads five
// hours old and is not due at the six hour threshold.
func TestSweepEligibilityReadsTheLiveStamp(t *testing.T) {
	pinSyntheticStageClock(t)
	now := time.Now()
	half := proxyUDPTTL(proxyUDPOK) / 2
	// wall age half minus an hour, elapsed age half plus an hour: the wall clock
	// stepped back two hours before the entry was stamped
	stamp := mixedClock(now.Add(-half+time.Hour), now.Add(-half-time.Hour))
	store := newProxyUDPCheckStore(nil)
	store.entries["tracked"] = &proxyUDPCheckEntry{at: stamp, result: proxyUDPResult{Status: proxyUDPOK}}
	candidate := store.probeCandidateLocked("tracked", now)
	if strings.Contains(candidate.lastChecked.String(), " m=") {
		t.Fatalf("a sort key carries a monotonic reading: %s", candidate.lastChecked)
	}
	if got := selectUDPProbeBatch(now, []udpProbeCandidate{candidate}, udpProbeRecheckWindow, time.Minute, nil); len(got) != 1 {
		t.Fatalf("the entry is %s old on the elapsed clock and %s old on the wall clock, and the batch left it out: the threshold is %s", now.Sub(stamp), now.Sub(stamp.Round(0)), half)
	}
	// the selector agrees with the entry's own stamp, which is the shape it read
	// before the sort keys were normalized
	live := candidate
	live.lastChecked = stamp
	if got := selectUDPProbeBatch(now, []udpProbeCandidate{live}, udpProbeRecheckWindow, time.Minute, nil); len(got) != 1 {
		t.Fatal("the selector left out an entry stamped by the store itself")
	}
}

// A recheck the tick deadline cancels leaves the identity with the result it had
// before, so a cancelled attempt of an identity that already has one is not
// recorded; the identity beside it, whose older result is due as well, then keeps
// the only slot on every tick. A cancelled check records its attempt for an
// identity with a previous result too, so the oldest attempt goes first and the
// slot rotates.
func TestCancelledRecheckRotatesTheOnlySlot(t *testing.T) {
	pinSyntheticStageClock(t)
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a := &connect.ProxySettings{Network: "tcp", Address: "a.example.test:1080"}
	b := &connect.ProxySettings{Network: "tcp", Address: "b.example.test:1080"}
	online := []*connect.ProxySettings{a, b}
	store := newProxyUDPCheckStore(nil)
	var cancel context.CancelFunc
	var probed []string
	store.probe = func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		probed = append(probed, settings.Address)
		// the tick deadline cancels the check before it can publish
		cancel()
		return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
	}
	// b holds an unknown result from a check that was not cancelled
	store.entries[b.Key()] = &proxyUDPCheckEntry{
		at:     t0.Add(-40 * time.Minute),
		result: proxyUDPResult{Status: proxyUDPUnknown},
	}
	// a's first check is cancelled before it publishes anything at all
	var ctx context.Context
	ctx, cancel = context.WithCancel(context.Background())
	store.Check(ctx, a, t0)
	for tick := 1; tick <= 8; tick++ {
		ctx, cancel = context.WithCancel(context.Background())
		runUDPProbeTick(ctx, store, online, t0.Add(time.Duration(tick)*5*time.Minute), udpProbeRecheckWindow, 5*time.Minute, 0, true)
		cancel()
	}
	// the first cancelled check, then the two identities take turns
	want := []string{a.Address, b.Address, a.Address, b.Address, a.Address, b.Address, a.Address, b.Address, a.Address}
	if strings.Join(probed, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatches %v, want %v: the slot did not rotate", probed, want)
	}
}

// Without a cancelled attempt there is nothing to rank: a check that publishes
// keeps the identity out of the due set for its own TTL and the next
// never-checked identity takes the slot, as before.
func TestPublishedChecksKeepTheExistingOrder(t *testing.T) {
	pinSyntheticStageClock(t)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	online := make([]*connect.ProxySettings, 0, 4)
	for i := 0; i < 4; i++ {
		online = append(online, &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("%02d.example.test:1080", i)})
	}
	store := newProxyUDPCheckStore(nil)
	var order []string
	store.probe = func(_ context.Context, settings *connect.ProxySettings) proxyUDPResult {
		order = append(order, settings.Address)
		return proxyUDPResult{Status: proxyUDPOK}
	}
	for tick := 0; tick < len(online); tick++ {
		at := now.Add(time.Duration(tick) * 5 * time.Minute)
		runUDPProbeTick(context.Background(), store, online, at, udpProbeRecheckWindow, 5*time.Minute, 0, true)
	}
	if len(order) != len(online) {
		t.Fatalf("measured %v, want each identity once", order)
	}
	for i, address := range order {
		if want := online[i].Address; address != want {
			t.Fatalf("measured %v, want the unchanged key order with %q at position %d", order, want, i)
		}
	}
}

// The trace: five identities share a single sweep slot on five-minute ticks.
// Four of them hold the slot with IPv6 retries that come due again and again,
// and the fifth has an expired UDP result and no IPv6 record at all, so its
// IPv6 due time is the zero time. It has been IPv6-due since its last UDP
// check, so that check is when it became due: falling back to its UDP half-TTL
// due time puts it a whole refresh window out and it never runs.
func TestUDPProbeSweepMeasuresANeverMeasuredIpv6Identity(t *testing.T) {
	pinSyntheticStageClock(t)
	h := newIPv6CheckHarness(t)
	names := map[string]string{}
	// the identity each selection touched
	hits := map[string]bool{}
	// a proxy without a UDP relay: its half-TTL due time is 36 hours out, which
	// is as far as the fallback can push an identity
	h.udpResult = proxyUDPResult{Status: proxyUDPNoRelay}
	probe := h.store.probe
	h.store.probe = func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		hits[names[settings.Key()]] = true
		return probe(ctx, settings)
	}
	// every stage stops on its own, so each measurement leaves a retry hold
	h.ipv6OK = false
	stage := h.store.ipv6Probe
	h.store.ipv6Probe = func(ctx context.Context, settings *connect.ProxySettings, previous proxyIPv6Result, now time.Time, dials proxyIPv6Dials) (proxyIPv6Result, bool) {
		hits[names[settings.Key()]] = true
		return stage(ctx, settings, previous, now, dials)
	}

	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	const (
		tickDuration = 5 * time.Minute
		ticks        = 864
	)
	var online []*connect.ProxySettings
	h.store.mu.Lock()
	for i := 0; i < 5; i++ {
		settings := &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1)}
		online = append(online, settings)
		entry := &proxyUDPCheckEntry{
			result:      proxyUDPResult{Status: proxyUDPNoRelay},
			at:          start.Add(-time.Minute),
			completedAt: start.Add(-time.Minute),
		}
		if i == 4 {
			// its UDP result expired an hour ago and no IPv6 was ever measured
			names[settings.Key()] = "never-measured"
			entry.at = start.Add(-time.Hour)
			entry.completedAt = entry.at
		} else {
			// cached no-relay evidence, an IPv6 record that is due, and a retry
			// hold that comes round every few ticks
			names[settings.Key()] = fmt.Sprintf("retry-%d", i)
			entry.ipv6 = proxyIPv6Result{At: start.Add(-7 * time.Hour), Interval: 6 * time.Hour, Status: proxyIPv6No, Source: "measured"}
			entry.ipv6RetryAt = start.Add(-time.Minute + time.Duration(i)*tickDuration)
		}
		h.store.entries[settings.Key()] = entry
	}
	h.store.mu.Unlock()

	seen := map[string]bool{}
	firstTick := ""
	for tick := 0; tick < ticks; tick++ {
		now := start.Add(time.Duration(tick) * tickDuration)
		hits = map[string]bool{}
		runUDPProbeTick(context.Background(), h.store, online, now, udpProbeRecheckWindow, tickDuration, 0, true)
		if len(hits) != 1 {
			t.Fatalf("tick %d measured %d identities, want the single sweep slot", tick, len(hits))
		}
		for name := range hits {
			if tick == 0 {
				firstTick = name
			}
			seen[name] = true
		}
	}
	if firstTick != "never-measured" {
		t.Fatalf("the first tick measured %q, want the identity whose IPv6 was never measured", firstTick)
	}
	if len(seen) != 5 {
		t.Fatalf("the trace measured %d identities, want all 5: %v", len(seen), seen)
	}
}

// The trace: five identities share a single sweep slot on five-minute ticks.
// Four of them hold the slot with an IPv6 retry that comes due again fifteen
// minutes after each stage stops. The fifth had its UDP check cancelled before
// it could publish. A cancelled attempt ranks behind every identity without one,
// and the four retries always have one, so the fifth never reaches the slot
// again however long the run lasts.
func TestUDPProbeSweepDoesNotStarveACancelledIdentity(t *testing.T) {
	pinSyntheticStageClock(t)
	h := newIPv6CheckHarness(t)
	// every stage stops on its own, so each measurement leaves a retry hold
	h.ipv6OK = false
	// a proxy without a UDP relay: only the IPv6 retry makes these due
	h.udpResult = proxyUDPResult{Status: proxyUDPNoRelay}

	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	const tickDuration = 5 * time.Minute
	names := map[string]string{}
	hits := map[string]bool{}
	var online []*connect.ProxySettings
	for i := 0; i < 4; i++ {
		settings := &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1)}
		online = append(online, settings)
		names[settings.Key()] = fmt.Sprintf("retry-%d", i)
		h.store.entries[settings.Key()] = &proxyUDPCheckEntry{
			result:      proxyUDPResult{Status: proxyUDPNoRelay},
			at:          start.Add(-30 * proxyIPv6StopBackoff),
			completedAt: start.Add(-30 * proxyIPv6StopBackoff),
			// an IPv6 record that is due, with a retry hold that has passed
			ipv6:        proxyIPv6Result{At: start.Add(-7 * time.Hour), Interval: 6 * time.Hour, Status: proxyIPv6No, Source: "measured"},
			ipv6RetryAt: start.Add(-time.Duration(i+1) * time.Minute),
		}
	}
	waiting := &connect.ProxySettings{Network: "tcp", Address: "192.0.2.5:1080"}
	online = append(online, waiting)
	names[waiting.Key()] = "cancelled"

	probe := h.store.probe
	// the fifth identity's only check is cancelled before it publishes
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstCancel := true
	h.store.probe = func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		hits[names[settings.Key()]] = true
		if settings.Key() == waiting.Key() && firstCancel {
			firstCancel = false
			cancelFirst()
			return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}
		return probe(ctx, settings)
	}
	stage := h.store.ipv6Probe
	h.store.ipv6Probe = func(ctx context.Context, settings *connect.ProxySettings, previous proxyIPv6Result, now time.Time, dials proxyIPv6Dials) (proxyIPv6Result, bool) {
		hits[names[settings.Key()]] = true
		return stage(ctx, settings, previous, now, dials)
	}

	h.store.Check(firstCtx, waiting, start)
	if h.store.attempts[waiting.Key()].IsZero() {
		t.Fatal("the cancelled check must leave an attempt stamp")
	}

	const ticks = 48
	dispatched := -1
	for tick := 0; tick < ticks; tick++ {
		now := start.Add(time.Duration(tick) * tickDuration)
		hits = map[string]bool{}
		runUDPProbeTick(context.Background(), h.store, online, now, udpProbeRecheckWindow, tickDuration, 0, true)
		if len(hits) != 1 {
			t.Fatalf("tick %d measured %d identities, want the single sweep slot", tick, len(hits))
		}
		if hits["cancelled"] && dispatched < 0 {
			dispatched = tick
		}
	}
	// one slot per tick over five identities: no identity can be held longer
	// than a handful of ticks, whatever the other four are retrying
	if dispatched < 0 || dispatched > 8 {
		t.Fatalf("the identity whose check was cancelled reached the slot at tick %d of %d, want within 8", dispatched, ticks)
	}
}

// A long run over identities that all stay due: one slot per five-minute tick
// and six identities. Every one of them, including the identity whose checks
// are cancelled and the one whose only attempt is long past, must reach the slot
// within a bounded number of ticks for the whole run.
func TestUDPProbeSweepIsFairOverALongRun(t *testing.T) {
	pinSyntheticStageClock(t)
	h := newIPv6CheckHarness(t)
	// every stage stops on its own, so each measurement leaves a retry hold
	h.ipv6OK = false
	h.udpResult = proxyUDPResult{Status: proxyUDPNoRelay}

	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	const tickDuration = 5 * time.Minute
	const identityCount = 6
	names := map[string]string{}
	var online []*connect.ProxySettings
	for i := 0; i < 4; i++ {
		settings := &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1)}
		online = append(online, settings)
		names[settings.Key()] = fmt.Sprintf("retry-%d", i)
		h.store.entries[settings.Key()] = &proxyUDPCheckEntry{
			result:      proxyUDPResult{Status: proxyUDPNoRelay},
			at:          start.Add(-30 * proxyIPv6StopBackoff),
			completedAt: start.Add(-30 * proxyIPv6StopBackoff),
			ipv6:        proxyIPv6Result{At: start.Add(-7 * time.Hour), Interval: 6 * time.Hour, Status: proxyIPv6No, Source: "measured"},
			ipv6RetryAt: start.Add(-time.Duration(i+1) * time.Minute),
		}
	}
	// an identity whose checks are cancelled every time it is dispatched, and
	// one whose only attempt is long past: neither ever publishes, so both stay
	// due for the whole run
	for i, attempt := range []time.Time{start.Add(-6 * 24 * time.Hour), start.Add(-30 * time.Minute)} {
		settings := &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+5)}
		online = append(online, settings)
		names[settings.Key()] = fmt.Sprintf("cancelled-%d", i)
		h.store.attempts[settings.Key()] = attempt
	}

	probe := h.store.probe
	stage := h.store.ipv6Probe
	var cancel context.CancelFunc
	hits := map[string]bool{}
	h.store.probe = func(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
		hits[names[settings.Key()]] = true
		if strings.HasPrefix(names[settings.Key()], "cancelled") {
			// the tick deadline cancels the check before it can publish
			cancel()
			return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}
		return probe(ctx, settings)
	}
	h.store.ipv6Probe = func(ctx context.Context, settings *connect.ProxySettings, previous proxyIPv6Result, now time.Time, dials proxyIPv6Dials) (proxyIPv6Result, bool) {
		hits[names[settings.Key()]] = true
		return stage(ctx, settings, previous, now, dials)
	}

	const ticks = 288
	last := map[string]int{}
	skipped := map[string]int{}
	worst, worstName := 0, ""
	for tick := 0; tick < ticks; tick++ {
		now := start.Add(time.Duration(tick) * tickDuration)
		hits = map[string]bool{}
		h.store.mu.Lock()
		candidates := make([]udpProbeCandidate, 0, len(online))
		for _, settings := range online {
			candidates = append(candidates, h.store.probeCandidateLocked(settings.Key(), now))
		}
		h.store.mu.Unlock()
		batch := selectUDPProbeBatch(now, candidates, udpProbeRecheckWindow, tickDuration, nil)
		if len(batch) != 1 {
			t.Fatalf("tick %d selected %d identities, want the single sweep slot", tick, len(batch))
		}
		selected := batch[0].key
		// a due identity the tick left out is waiting; the longest such run is
		// how long some identity waited while others took the slot
		for _, candidate := range candidates {
			if candidate.key == selected {
				skipped[candidate.key] = 0
				continue
			}
			if len(selectUDPProbeBatch(now, []udpProbeCandidate{candidate}, udpProbeRecheckWindow, tickDuration, nil)) != 1 {
				skipped[candidate.key] = 0
				continue
			}
			skipped[candidate.key]++
			if skipped[candidate.key] > worst {
				worst, worstName = skipped[candidate.key], names[candidate.key]
			}
		}
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		runUDPProbeTick(ctx, h.store, online, now, udpProbeRecheckWindow, tickDuration, 0, true)
		cancel()
		if !hits[names[selected]] {
			t.Fatalf("tick %d selected %s and measured %v", tick, names[selected], hits)
		}
		last[selected] = tick
	}
	if len(last) != identityCount {
		t.Fatalf("%d of %d identities reached the slot over %d ticks: %v", len(last), identityCount, ticks, last)
	}
	t.Logf("%d identities over %d ticks, longest wait for a due identity: %s, %d ticks", identityCount, ticks, worstName, worst)
	// one slot per tick over six identities: a fair order serves a due identity
	// within a few ticks, and none of them waits anywhere near the run length
	if worst > 2*identityCount {
		t.Fatalf("%s waited %d ticks while due, want at most %d", worstName, worst, 2*identityCount)
	}
}

// mixedClock returns a stamp whose wall clock comes from wallLive and whose
// monotonic reading comes from elapsed, the pair a live stamp carries after the
// wall clock stepped between two reads. A test needs it because the public
// clock cannot produce two stamps whose wall order and elapsed order disagree
// while the wall clock stands still.
func mixedClock(wallLive, elapsed time.Time) time.Time {
	type timeRepr struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	t := wallLive
	tRepr := (*timeRepr)(unsafe.Pointer(&t))
	eRepr := (*timeRepr)(unsafe.Pointer(&elapsed))
	tRepr.ext = eRepr.ext
	return t
}

// Sol's trace: after a backward wall step two live stamps compare by elapsed
// time while each compares with a restored stamp by the wall clock, so the
// comparator runs in a cycle. Every permutation of the triple must come out in
// wall order.
func TestSelectUDPProbeBatchOrdersMixedClocks(t *testing.T) {
	base := time.Now()
	// wall order is restored, live-early, live-late; the live pair's elapsed
	// order runs the other way, as it does when the wall clock steps back
	// between their stamps
	liveLate := udpProbeCandidate{key: "live-late", online: true, status: proxyUDPOK, lastChecked: mixedClock(base.Add(-20*time.Hour), base.Add(-48*time.Hour))}
	liveEarly := udpProbeCandidate{key: "live-early", online: true, status: proxyUDPOK, lastChecked: mixedClock(base.Add(-21*time.Hour), base.Add(-40*time.Hour))}
	restored := udpProbeCandidate{key: "restored", online: true, status: proxyUDPOK, lastChecked: base.Add(-20*time.Hour - 30*time.Minute).Round(0)}
	triple := []udpProbeCandidate{liveLate, liveEarly, restored}
	want := []string{"live-early", "restored", "live-late"}
	permutations := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, permutation := range permutations {
		input := []udpProbeCandidate{triple[permutation[0]], triple[permutation[1]], triple[permutation[2]]}
		batch := selectUDPProbeBatch(base, input, 24*time.Hour, 24*time.Hour, nil)
		var got []string
		for _, candidate := range batch {
			got = append(got, candidate.key)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %v ordered %v, want the wall order %v", permutation, got, want)
		}
	}
}

// A comparator that mixes the two clocks can put a later due time before an
// earlier one, so sorting is not a total order. Every candidate here carries a
// random pair of a wall clock and an elapsed reading, and the batch must still
// come out in wall order.
func TestSelectUDPProbeBatchComparatorIsATotalOrder(t *testing.T) {
	base := time.Now()
	window := proxyUDPTTL(proxyUDPOK) / 2
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		candidates := make([]udpProbeCandidate, 0, 12)
		for i := 0; i < 12; i++ {
			// all due, with wall stamps far enough apart that the window cannot
			// reorder them
			wall := base.Add(-time.Duration(13+rng.Intn(23)) * time.Hour)
			lastChecked := wall.Round(0)
			if rng.Intn(2) == 0 {
				// an elapsed reading of its own, far enough back that the
				// window has run out on either clock
				lastChecked = mixedClock(wall, base.Add(-time.Duration(13+rng.Intn(48))*time.Hour))
			}
			candidates = append(candidates, udpProbeCandidate{
				key:         fmt.Sprintf("k%02d", i),
				online:      true,
				status:      proxyUDPOK,
				lastChecked: lastChecked,
			})
		}
		batch := selectUDPProbeBatch(base, candidates, udpProbeRecheckWindow, udpProbeRecheckWindow, nil)
		if len(batch) != len(candidates) {
			t.Fatalf("trial %d selected %d of %d due candidates", trial, len(batch), len(candidates))
		}
		for i := 1; i < len(batch); i++ {
			previous := batch[i-1]
			current := batch[i]
			previousDue := previous.lastChecked.Add(window).Round(0)
			currentDue := current.lastChecked.Add(window).Round(0)
			if currentDue.Before(previousDue) {
				t.Fatalf("trial %d put %q (due %v) before %q (due %v)", trial, current.key, currentDue, previous.key, previousDue)
			}
			if currentDue.Equal(previousDue) && current.key <= previous.key {
				t.Fatalf("trial %d broke the key order on equal due times: %q then %q", trial, previous.key, current.key)
			}
		}
	}
}
