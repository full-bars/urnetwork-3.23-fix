package main

import "testing"

// Coverage-gap tests (Sonnet round-3 review): proxyTableProbeConfig.probeWidth
// was 0% covered — nothing drove it directly, only indirectly through
// resolveProxyTableProbeConfig's own clamping. probeWidth is the "wider of
// SampleWidth and MaxSampleWidth" helper the adaptive path uses to size the
// host pool sampled for a pass.

// TestProbeWidth_MaxWiderThanSample: when MaxSampleWidth exceeds SampleWidth
// (the normal adaptive-overflow case), probeWidth must return MaxSampleWidth
// so the adaptive path always has room to grow into.
func TestProbeWidth_MaxWiderThanSample(t *testing.T) {
	cfg := proxyTableProbeConfig{SampleWidth: 12, MaxSampleWidth: 36}
	if got := cfg.probeWidth(); got != 36 {
		t.Fatalf("probeWidth() = %d, want 36 (MaxSampleWidth)", got)
	}
}

// TestProbeWidth_MaxAtOrBelowSample: when MaxSampleWidth is at or below
// SampleWidth (adaptive overflow disabled, or an operator misconfiguration),
// probeWidth must fall back to SampleWidth — never to a smaller number than
// the base pass actually dials.
func TestProbeWidth_MaxAtOrBelowSample(t *testing.T) {
	cases := []struct {
		name           string
		sample, maxwid int
		want           int
	}{
		{"max equals sample", 12, 12, 12},
		{"max below sample", 12, 4, 12},
		{"max zero (disabled)", 12, 0, 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := proxyTableProbeConfig{SampleWidth: c.sample, MaxSampleWidth: c.maxwid}
			if got := cfg.probeWidth(); got != c.want {
				t.Fatalf("probeWidth() = %d, want %d", got, c.want)
			}
		})
	}
}

// TestProbeWidth_DefaultConfig pins the real production default: SampleWidth
// 12, MaxSampleWidth 36 — probeWidth() must report the wider adaptive
// ceiling for the config actually shipped, not just synthetic values.
func TestProbeWidth_DefaultConfig(t *testing.T) {
	cfg := defaultProxyTableProbeConfig()
	if got := cfg.probeWidth(); got != cfg.MaxSampleWidth {
		t.Fatalf("probeWidth() on the default config = %d, want MaxSampleWidth %d", got, cfg.MaxSampleWidth)
	}
}
