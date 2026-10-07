package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// After a pressure cut the pool must not regrow past a ceiling just under the
// level where pressure appeared, or it sawtooths: grow, hit the same wall,
// shed earners, repeat.
func TestAimdCeilingCapsRegrowthAfterCut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(400, now) // pressure hit at a pool of 400, then the pool was cut to 280

	ceiling := c.effective(now)
	if ceiling != 360 {
		t.Fatalf("ceiling = %d, want 360 (0.9 x 400)", ceiling)
	}
	target := 280
	for i := 0; i < 20; i++ {
		target = aimdStep(target, 10_000, 0.05, ceiling)
	}
	if target != 360 {
		t.Fatalf("regrowth settled at %d, want it held at the ceiling 360", target)
	}
}

// The ceiling is a memory of where trouble was, not a permanent cap: it
// relaxes toward the level pressure hit and is released after 24h so a box
// whose capacity has changed can rediscover it.
func TestAimdCeilingRelaxesAndReleases(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(400, now)

	if got := c.effective(now.Add(12 * time.Hour)); got != 380 {
		t.Fatalf("ceiling at 12h = %d, want 380 (halfway from 360 to 400)", got)
	}
	if got := c.effective(now.Add(24 * time.Hour)); got != 0 {
		t.Fatalf("ceiling at 24h = %d, want 0 (released)", got)
	}
	if got := c.effective(now.Add(48 * time.Hour)); got != 0 {
		t.Fatalf("ceiling at 48h = %d, want 0 (released)", got)
	}
}

func TestAimdCeilingNeverBelowFloor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(aimdFloor-10, now)
	if got := c.effective(now); got < aimdFloor {
		t.Fatalf("ceiling = %d, want at least the floor %d", got, aimdFloor)
	}
}

func TestAimdCeilingUnsetMeansNoCeiling(t *testing.T) {
	var c aimdCeilingState
	if got := c.effective(time.Unix(1_700_000_000, 0)); got != 0 {
		t.Fatalf("zero-value ceiling = %d, want 0 (none)", got)
	}
}

func TestCombineCeilingsTakesTightestPositive(t *testing.T) {
	cases := []struct{ configured, learned, want int }{
		{0, 0, 0},
		{500, 0, 500},
		{0, 360, 360},
		{500, 360, 360},
		{300, 360, 300},
	}
	for _, tc := range cases {
		if got := combineCeilings(tc.configured, tc.learned); got != tc.want {
			t.Errorf("combineCeilings(%d, %d) = %d, want %d", tc.configured, tc.learned, got, tc.want)
		}
	}
}

// Right after a restart the lifetime traffic map is empty, so every proxy
// looks idle. The persisted earnings score must still protect the earner.
func TestSelectURLProxiesToShed_EarningsProtectEarnerAfterRestart(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
		"3.3.3.3:1080": {Health: "up", Source: "url"},
	}}
	earnings := map[string]float64{"1.1.1.1:1080": 5e9, "2.2.2.2:1080": 0, "3.3.3.3:1080": 1e6}
	got := selectURLProxiesToShed(state, nil, earnings, 2)
	want := []string{"2.2.2.2:1080", "3.3.3.3:1080"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v (earner 1.1.1.1 must be shed last)", got, want)
	}
}

// Health tier still outranks earnings: a dead proxy goes before a healthy
// non-earner.
func TestSelectURLProxiesToShed_HealthBeatsEarnings(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "dead", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
	}}
	earnings := map[string]float64{"1.1.1.1:1080": 9e9, "2.2.2.2:1080": 0}
	got := selectURLProxiesToShed(state, nil, earnings, 1)
	if !slices.Equal(got, []string{"1.1.1.1:1080"}) {
		t.Fatalf("got %v, want the dead proxy first", got)
	}
}

// Missing earnings data falls back to the lifetime-traffic order.
func TestSelectURLProxiesToShed_NoEarningsFallsBackToTraffic(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.1.1.1:1080": {Health: "up", Source: "url"},
		"2.2.2.2:1080": {Health: "up", Source: "url"},
	}}
	traffic := map[string]uint64{"1.1.1.1:1080": 1000, "2.2.2.2:1080": 5}
	got := selectURLProxiesToShed(state, traffic, nil, 1)
	if !slices.Equal(got, []string{"2.2.2.2:1080"}) {
		t.Fatalf("got %v, want lowest-traffic first", got)
	}
}

// The trim/OOM cap bounds ALL running proxies, so the URL target may only use
// what paid proxies leave over.
func TestCapURLTargetLeavesRoomForPaid(t *testing.T) {
	cases := []struct{ next, cap, paid, want int }{
		{500, 0, 40, 500},   // no cap: untouched
		{500, 300, 40, 260}, // cap minus paid
		{200, 300, 40, 200}, // already under
		{500, 300, 400, 1},  // paid exceeds the cap: keep a nonzero target
	}
	for _, tc := range cases {
		if got := capURLTarget(tc.next, tc.cap, tc.paid); got != tc.want {
			t.Errorf("capURLTarget(%d, cap=%d, paid=%d) = %d, want %d", tc.next, tc.cap, tc.paid, got, tc.want)
		}
	}
}

// Pressure is held for many minutes, and each sustained-high sample cuts again
// at 0.7x. The ceiling must remember where pressure FIRST hit, not the last,
// lowest cut of the cascade, or a transient host event pins the pool near the
// floor for a day.
func TestAimdCeilingCascadeKeepsTheFirstHit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(1000, now)
	c.record(700, now.Add(10*time.Minute))
	c.record(490, now.Add(20*time.Minute))
	if got := c.effective(now.Add(20 * time.Minute)); got != 900 {
		t.Fatalf("ceiling after a cascade = %d, want 900 (0.9 x the first hit of 1000)", got)
	}
}

// Once the ceiling has been released, a new episode starts fresh at its own level.
func TestAimdCeilingNewEpisodeAfterReleaseStartsFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var c aimdCeilingState
	c.record(1000, now)
	later := now.Add(25 * time.Hour)
	c.record(300, later)
	if got := c.effective(later); got != 270 {
		t.Fatalf("ceiling for a fresh episode = %d, want 270", got)
	}
}

// The recorded hit is the real pool, not a target the cache never reached.
func TestCeilingHitUsesTheSmallerOfTargetAndPool(t *testing.T) {
	cases := []struct{ target, pool, want int }{
		{1000, 600, 600},
		{500, 900, 500},
		{500, 0, 500}, // pool size unknown
	}
	for _, tc := range cases {
		if got := ceilingHit(tc.target, tc.pool); got != tc.want {
			t.Errorf("ceilingHit(%d, %d) = %d, want %d", tc.target, tc.pool, got, tc.want)
		}
	}
}

// The trim cap counts RUNNING non-direct proxies. proxy.state also holds dead,
// backed-off and trim-held entries; counting those drove cap - paid to nothing
// and pinned the URL pool at 1.
func TestPaidRunningCountCountsOnlyRunningPaid(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"paid-up:1":    {Health: "up", Source: "file"},
		"paid-run2:1":  {Health: "up", Source: ""},
		"paid-dead:1":  {Health: "dead", Source: "file"},
		"url-up:1":     {Health: "up", Source: "url"},
		directProxyKey: {Health: "up", Source: "file"},
	}}
	running := []string{"paid-up:1", "paid-run2:1", "url-up:1", directProxyKey, "unknown:1"}
	if got := paidRunningCount(state, running); got != 2 {
		t.Fatalf("paidRunningCount = %d, want 2 (running non-URL, non-direct, known to the state)", got)
	}
	if got := paidRunningCount(state, nil); got != 0 {
		t.Fatalf("paidRunningCount with nothing running = %d, want 0", got)
	}
}

// The lowered-target line must not blame a cap change for a cache-tracking
// cut: aimdStep's grow branch catches the target down to cacheSize plus the
// increment when the pool lags far behind, and blaming a cap sends an
// operator chasing a cause that never existed.
func TestAimdMoveMessageAttributesCause(t *testing.T) {
	msg := aimdMoveMessage(1000, 100+aimdIncrement, 100, 0.1)
	if !strings.Contains(msg, "only 100 proxies are cached") || !strings.Contains(msg, "follows the live pool") {
		t.Fatalf("a cache-tracking cut must say so: %q", msg)
	}
	if strings.Contains(msg, "cap or ceiling") {
		t.Fatalf("a cache-tracking cut must not blame a cap or ceiling: %q", msg)
	}
	// A genuine cap or ceiling cut keeps its original attribution.
	if plain := aimdMoveMessage(1000, 700, 900, 0.5); !strings.Contains(plain, "cap or ceiling lowered") {
		t.Fatalf("a cap cut keeps its wording: %q", plain)
	}
	// A pressure shrink keeps its wording too.
	if shrink := aimdMoveMessage(1000, 500, 1000, 0.9); !strings.Contains(shrink, "pressure has been high") {
		t.Fatalf("a pressure shrink keeps its wording: %q", shrink)
	}
	// And a low-pressure raise keeps its wording.
	if grow := aimdMoveMessage(100, 100+aimdIncrement, 100, 0.1); !strings.Contains(grow, "pressure is low") {
		t.Fatalf("a low-pressure raise keeps its wording: %q", grow)
	}
}
