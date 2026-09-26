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

// The estimate is calibrated to the high side of the measured fleet (roughly
// 0.2-0.55 MiB of heap per proxy, 0.25 used here plus a fixed 100 MiB), so a
// warning means "this limit is very likely short for this pool". Pin its shape.
func TestEstimatedPoolMemoryIsMonotonicAndBounded(t *testing.T) {
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
	// The incident box (2,001 proxies, heap ~410-470 MiB fresh): the estimate must
	// not exceed what it really needed by a wide margin.
	if got := estimatedPoolMemory(2001); got < 350*testMiB || got > 650*testMiB {
		t.Fatalf("estimate for 2001 proxies = %d MiB, want within the 350-650 MiB band around the measured 410-470 MiB", got/testMiB)
	}
}
