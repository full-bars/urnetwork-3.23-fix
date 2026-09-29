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
			in:   resourceConfigInput{Proxies: 1300, GoMemLimit: 1600 * testMiB, CgroupCeiling: 2000 * testMiB},
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
// clients, which are not known at startup but drive memory. It is calibrated
// against a sample of the running fleet after days of uptime, not fresh
// restarts: heap per proxy there had a median near 0.65 MiB and an upper
// quartile near 1, and the whole process footprint per proxy had a median near
// 1 MiB. Pin its shape: it grows with the pool, an empty pool costs the fixed
// overhead, and it sits at or above the upper quartile of the measured heap per
// proxy without running away from it.
func TestEstimatedPoolMemoryIsMonotonicAndCoversMeasuredFleetLoad(t *testing.T) {
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
	for _, n := range []int{500, 2001, 4000} {
		perProxy := float64(estimatedPoolMemory(n)-resourceBaseOverhead) / float64(n) / float64(testMiB)
		if perProxy < 1.0 {
			t.Fatalf("estimate at %d proxies is %.2f MiB per proxy, below the ~1 MiB the measured upper quartile needs", n, perProxy)
		}
		if perProxy > 1.5 {
			t.Fatalf("estimate at %d proxies is %.2f MiB per proxy, far above what was measured, so it would cry wolf", n, perProxy)
		}
	}
}

// Boxes measured in the running fleet (days of uptime) that were flowing with no
// resource pressure must stay silent under the limits they actually ran with.
func TestResourceConfigDoesNotWarnOnMeasuredHealthyBoxes(t *testing.T) {
	healthy := []struct {
		name string
		in   resourceConfigInput
	}{
		{"2 GiB box: 1172 proxies, 2355 MiB limit", resourceConfigInput{Proxies: 1172, GoMemLimit: 2355 * testMiB}},
		{"2 GiB box: 975 proxies, 1741 MiB limit", resourceConfigInput{Proxies: 975, GoMemLimit: 1741 * testMiB}},
		{"2 GiB box: 1282 proxies, 1741 MiB limit", resourceConfigInput{Proxies: 1282, GoMemLimit: 1741 * testMiB}},
		{"2 GiB box: 1238 proxies, 1741 MiB limit", resourceConfigInput{Proxies: 1238, GoMemLimit: 1741 * testMiB}},
		{"2 GiB box: 935 proxies, 1536 MiB limit", resourceConfigInput{Proxies: 935, GoMemLimit: 1536 * testMiB}},
		{"3 GiB box: 887 proxies, 2150 MiB limit", resourceConfigInput{Proxies: 887, GoMemLimit: 2150 * testMiB}},
		{"12 GiB box: 2374 proxies, 9523 MiB limit", resourceConfigInput{Proxies: 2374, GoMemLimit: 9523 * testMiB}},
		{"24 GiB box: 4275 proxies, 19149 MiB limit", resourceConfigInput{Proxies: 4275, GoMemLimit: 19149 * testMiB}},
	}
	for _, c := range healthy {
		if got := resourceConfigWarnings(c.in); len(got) != 0 {
			t.Errorf("%s: unexpected warnings %q", c.name, got)
		}
	}
}

// Boxes measured at resource pressure 1.0 (degraded, swapping) under the limits
// they ran with are exactly the ones this warning exists for. The earlier,
// smaller estimate stayed silent on every one of them.
func TestResourceConfigWarnsOnBoxesMeasuredUnderPressure(t *testing.T) {
	pressured := []struct {
		name string
		in   resourceConfigInput
	}{
		{"2 GiB box: 1727 proxies, 1434 MiB limit", resourceConfigInput{Proxies: 1727, GoMemLimit: 1434 * testMiB}},
		{"1 GiB box: 1054 proxies, 512 MiB limit", resourceConfigInput{Proxies: 1054, GoMemLimit: 512 * testMiB}},
		{"5 GiB box: 4147 proxies, 3072 MiB limit", resourceConfigInput{Proxies: 4147, GoMemLimit: 3072 * testMiB}},
	}
	for _, c := range pressured {
		got := resourceConfigWarnings(c.in)
		if len(got) != 1 || !strings.Contains(got[0], "GOMEMLIMIT=") {
			t.Errorf("%s: want one GOMEMLIMIT warning, got %q", c.name, got)
		}
	}
}

func TestRAMCeilingLogLine(t *testing.T) {
	got := ramCeilingLogLine(1<<30, "cgroup v2 memory.max at /system.slice/urnetwork.service")
	want := "[proxy][resources] effective RAM ceiling 1024 MiB (cgroup v2 memory.max at /system.slice/urnetwork.service)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// It must reach events.log with the other capacity-control lines.
	if !isImportantLogLine("0928 12:00:00 " + got) {
		t.Fatal("the ceiling line must match an important-log marker")
	}
}
