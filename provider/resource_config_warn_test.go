package main

import (
	"strings"
	"testing"
)

const testMiB = int64(1) << 20

// Two 1 GB boxes ran 1,300 proxies under a hand-pinned GOMEMLIMIT=400MiB,
// GOGC=50 and a 450M cgroup MemoryHigh: the live heap (about 470 MiB) sat above
// its own soft limit, so the runtime spent its CPU on GC and the cgroup
// throttled the rest. Nothing said so. At startup the provider now compares the
// limits it was given with a conservative estimate of what the pool needs, and
// says so once, with what to change.
func TestResourceConfigWarnings(t *testing.T) {
	cases := []struct {
		name string
		in   resourceConfigInput
		want []string // substrings, one per expected warning, in order
	}{
		{
			name: "1 GB box: 1300 proxies under a 400 MiB limit and a 450M cgroup ceiling",
			in:   resourceConfigInput{Proxies: 1300, GoMemLimit: 400 * testMiB, CgroupCeiling: 450 * testMiB},
			want: []string{"GOMEMLIMIT=400 MiB", "cgroup memory ceiling 450 MiB"},
		},
		{
			name: "a limit that fits the pool is silent",
			in:   resourceConfigInput{Proxies: 1300, GoMemLimit: 1600 * testMiB, CgroupCeiling: 1900 * testMiB},
			want: nil,
		},
		{
			name: "no limits at all is silent",
			in:   resourceConfigInput{Proxies: 4000},
			want: nil,
		},
		{
			name: "a small pool under a small limit is fine",
			in:   resourceConfigInput{Proxies: 100, GoMemLimit: 256 * testMiB},
			want: nil,
		},
		{
			name: "a pinned GOGC disables the adaptive governor under the auto profile",
			in:   resourceConfigInput{Proxies: 500, GOGCEnv: true, AutoProfile: true},
			want: []string{"GOGC is pinned"},
		},
		{
			name: "a pinned GOGC without the auto profile is the operator's plain choice",
			in:   resourceConfigInput{Proxies: 500, GOGCEnv: true},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resourceConfigWarnings(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %d warnings %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			for i, w := range c.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("warning %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// The estimate covers the proxies AND a peak-load allowance for connected
// clients, which are not known at startup but drive memory (about 0.8 MiB each;
// A 2 GiB box peaked at ~540 clients on 2,001 proxies, heap 415 -> 791 MiB). Pin its
// shape against the two measured ends: an idle fresh process and that peak.
func TestEstimatedPoolMemoryIsMonotonicAndCoversPeakClientLoad(t *testing.T) {
	if estimatedPoolMemory(0) != resourceBaseOverhead {
		t.Fatalf("an empty pool still costs the fixed overhead")
	}
	prev := estimatedPoolMemory(0)
	for _, n := range []int{100, 500, 1000, 2000, 4000} {
		cur := estimatedPoolMemory(n)
		if cur <= prev {
			t.Fatalf("estimate must grow with the pool: %d proxies -> %d, previous %d", n, cur, prev)
		}
		prev = cur
	}
	// 2 GiB box: 415 MiB idle-fresh, 791 MiB at the observed client peak (2,001 proxies).
	// The estimate must cover the peak but not overshoot it by much.
	got := estimatedPoolMemory(2001)
	if got < 791*testMiB {
		t.Fatalf("estimate for 2001 proxies = %d MiB does not cover the measured 791 MiB peak-client heap", got/testMiB)
	}
	if got > 950*testMiB {
		t.Fatalf("estimate for 2001 proxies = %d MiB overshoots the measured peak (791 MiB) by too much and would cry wolf", got/testMiB)
	}
}

// The client allowance must not turn boxes that are known to run fine into
// warnings: these were healthy on the day the estimate was calibrated.
func TestResourceConfigDoesNotWarnOnKnownHealthyBoxes(t *testing.T) {
	healthy := []struct {
		name string
		in   resourceConfigInput
	}{
		{"1 GB box after the fix: 1458 proxies, 818 MiB limit", resourceConfigInput{Proxies: 1458, GoMemLimit: 818 * testMiB}},
		{"1 GB box after the fix: 1029 proxies, 818 MiB limit", resourceConfigInput{Proxies: 1029, GoMemLimit: 818 * testMiB}},
		{"2 GiB box: 2001 proxies, 1400 MiB limit", resourceConfigInput{Proxies: 2001, GoMemLimit: 1400 * testMiB}},
		{"2 GiB box: 1067 proxies, 1.7 GiB limit", resourceConfigInput{Proxies: 1067, GoMemLimit: 1700 * testMiB}},
		{"1 GB box: 596 proxies, 818 MiB limit", resourceConfigInput{Proxies: 596, GoMemLimit: 818 * testMiB}},
	}
	for _, c := range healthy {
		if got := resourceConfigWarnings(c.in); len(got) != 0 {
			t.Errorf("%s: unexpected warnings %q", c.name, got)
		}
	}
}
