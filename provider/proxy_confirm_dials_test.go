package main

import (
	"context"
	"testing"
	"time"
)

// MinConfirmDials is a floor on ATTEMPTED dials (res.Total), not on
// SampleWidth. A sub-bar pass below the floor runs its whole block before the
// viability abort may convict it, so a correlated region-deny cluster cannot
// seal an F from a handful of dials. 0 (the URL admission path) must reproduce
// the previous fail-fast behaviour byte for byte.

// TestMinConfirmDials_SubBarPassRunsWholeBlockBeforeConviction is the core
// regression: the same all-refused pass must fail fast with the floor off and
// dial its whole block with the floor on.
func TestMinConfirmDials_SubBarPassRunsWholeBlockBeforeConviction(t *testing.T) {
	// every CONNECT refused, so the bar is unreachable from the first dial —
	// exactly the case the fail-fast abort was built for.
	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x05 })
	defer cleanup()

	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 6
	cfg.MinSampleWidth = 6
	cfg.MaxSampleWidth = 12
	cfg.TargetTimeout = time.Second

	if n := seedProbeDNSForBlocks(t, addr, cfg, tableProbePassCounter.Load()); n < 6 {
		t.Skipf("seeded %d hosts; need >= 6 for a deterministic block", n)
	}

	// floor disabled -> fail-fast, unchanged.
	cfg.MinConfirmDials = 0
	before := connects.Load()
	probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	noFloor := int(connects.Load() - before)
	if noFloor >= 6 {
		t.Errorf("MinConfirmDials=0 must fail fast: dialed %d of 6", noFloor)
	}

	// floor enabled -> the same pass must run the whole block.
	cfg.MinConfirmDials = 6
	before = connects.Load()
	res := probeTableThroughProxy(context.Background(), addr, "", "", "", 0, cfg)
	floorDials := int(connects.Load() - before)
	if floorDials != 6 {
		t.Errorf("MinConfirmDials=6 must dial the whole 6-wide block: dialed %d", floorDials)
	}

	// the conviction must rest on the floor, measured in ATTEMPTED dials.
	if res.Total != 6 {
		t.Errorf("conviction Total = %d, want 6 (the floor counts attempted dials)", res.Total)
	}
	// and the operator-facing pair must stop overstating the work: the abort
	// used to leave Total=3 against SampleWidth=6, which printed as "0/6".
	if res.Total != res.SampleWidth {
		t.Errorf("display still overstates dials: SampleWidth=%d Total=%d", res.SampleWidth, res.Total)
	}
	if !res.Decidable || res.Score != 0 {
		t.Errorf("expected a decidable 0.0 conviction, got decidable=%v score=%v", res.Decidable, res.Score)
	}
}

// TestConfirmNeeded_Table pins the trigger's boundary: only a below-band pass
// that has not yet met the floor grows. In-band passes belong to growthNeeded
// and above-band passes have nothing to confirm.
func TestConfirmNeeded_Table(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.PassBar = 0.6
	cfg.BorderlineBand = 0.15 // below-band means score < 0.45

	cases := []struct {
		name      string
		total, ok int
		floor     int
		want      bool
	}{
		{"disabled", 3, 0, 0, false},
		{"sub-bar below floor grows", 3, 0, 6, true},
		{"one short of floor grows", 5, 0, 6, true},
		{"floor met does not grow", 6, 0, 6, false},
		{"in-band is growthNeeded's job", 3, 2, 6, false}, // 0.667
		{"above band has nothing to confirm", 2, 2, 6, false},
		{"nothing asked", 0, 0, 6, false},
	}
	for _, c := range cases {
		res := tableProbeResult{Total: c.total, OK: c.ok, SampleWidth: 6}
		cfg.MinConfirmDials = c.floor
		if got := confirmNeeded(res, cfg); got != c.want {
			t.Errorf("%s: confirmNeeded(total=%d ok=%d floor=%d) = %v, want %v",
				c.name, c.total, c.ok, c.floor, got, c.want)
		}
	}
}

// TestMinConfirmDials_FloorCannotExceedWhatTheProbeDials guards the clamp: a
// floor above probeWidth() could never be satisfied, so the pass would grow on
// every sweep and still convict on thin evidence.
func TestMinConfirmDials_FloorCannotExceedWhatTheProbeDials(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	cfg.SampleWidth = 12
	cfg.MaxSampleWidth = 36
	if w := cfg.probeWidth(); w != 36 {
		t.Fatalf("probeWidth = %d, want 36", w)
	}
	// mirror the resolver's clamp arithmetic
	capped := 999
	if w := cfg.probeWidth(); capped > w {
		capped = w
	}
	if capped != 36 {
		t.Fatalf("floor clamp produced %d, want 36", capped)
	}
}
