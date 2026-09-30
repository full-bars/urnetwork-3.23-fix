package main

import (
	"strings"
	"sync"
	"testing"
)

const testMiB = int64(1) << 20

// Two 1 GB boxes ran 1,300 proxies under a hand-pinned GOMEMLIMIT=400MiB,
// GOGC=50 and a 450M cgroup MemoryHigh: the live heap (about 470 MiB) sat above
// its own soft limit, so the runtime spent its CPU on GC and the cgroup
// throttled the rest. Nothing said so. At startup the provider now compares the
// limits it was given with an estimate of what the pool needs, and says so
// once, with what to change.
func TestResourceConfigWarnings(t *testing.T) {
	cases := []struct {
		name string
		in   resourceConfigInput
		want []string // substrings, one per expected warning, in order
	}{
		{
			name: "1 GB box: 1300 proxies under a 400 MiB heap limit",
			in:   resourceConfigInput{Proxies: 1300, GoMemLimit: 400 * testMiB, EffectiveRAMBytes: 1024 * testMiB},
			want: []string{"GOMEMLIMIT=400 MiB", "1300 proxies is expected to need about"},
		},
		{
			name: "a limit that fits the pool is silent",
			in:   resourceConfigInput{Proxies: 1300, GoMemLimit: 1600 * testMiB, EffectiveRAMBytes: 4096 * testMiB},
			want: nil,
		},
		{
			name: "no limits at all is silent",
			in:   resourceConfigInput{Proxies: 4000},
			want: nil,
		},
		{
			name: "a small pool under a small limit is fine",
			in:   resourceConfigInput{Proxies: 100, GoMemLimit: 256 * testMiB, EffectiveRAMBytes: 1024 * testMiB},
			want: nil,
		},
		{
			name: "a pool too big for the box warns about RAM, not the heap limit",
			in:   resourceConfigInput{Proxies: 2001, GoMemLimit: 1434 * testMiB, EffectiveRAMBytes: 1930 * testMiB},
			want: []string{"this box gives the provider about 1930 MiB"},
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

// The two estimates are different numbers for different questions: GOMEMLIMIT
// bounds the Go heap, the box has to hold the whole process. Pin both shapes: a
// pool of zero still costs the fixed overhead, both grow with the pool, and
// the heap estimate stays under the measured per-proxy heap while the
// footprint estimate stays under the measured per-proxy footprint.
func TestPoolEstimatesAreMonotonicAndMatchTheSampledFleet(t *testing.T) {
	if estimatedPoolHeap(0) != resourceBaseOverhead {
		t.Fatalf("an empty pool still costs the fixed overhead")
	}
	if estimatedPoolFootprint(0) != resourceBaseOverhead {
		t.Fatalf("an empty pool still costs the fixed overhead")
	}
	prevHeap, prevFoot := estimatedPoolHeap(0), estimatedPoolFootprint(0)
	for _, n := range []int{100, 500, 1000, 2000, 4000} {
		curHeap, curFoot := estimatedPoolHeap(n), estimatedPoolFootprint(n)
		if curHeap <= prevHeap || curFoot <= prevFoot {
			t.Fatalf("estimates must grow with the pool: %d proxies -> heap %d footprint %d", n, curHeap, curFoot)
		}
		prevHeap, prevFoot = curHeap, curFoot
	}
	for _, n := range []int{500, 2001, 4147} {
		perProxy := float64(estimatedPoolHeap(n)-resourceBaseOverhead) / float64(n) / float64(testMiB)
		if perProxy < 0.5 || perProxy > 0.95 {
			t.Fatalf("heap estimate at %d proxies is %.2f MiB per proxy, outside the sampled 0.4 to 0.95 range", n, perProxy)
		}
		foot := float64(estimatedPoolFootprint(n)-resourceBaseOverhead) / float64(n) / float64(testMiB)
		if foot < 0.8 || foot > 1.15 {
			t.Fatalf("footprint estimate at %d proxies is %.2f MiB per proxy, outside the sampled 0.8 to 1.15 range", n, foot)
		}
	}
	// The footprint must always reach or exceed the heap estimate: it is the
	// same process plus stacks, buffers, swap and the OS-side cost, and the box
	// is what has to hold it. At an empty pool the two are the same fixed
	// overhead, so the check starts where the pool does.
	for _, n := range []int{1, 100, 2000, 4147} {
		if estimatedPoolFootprint(n) <= estimatedPoolHeap(n) {
			t.Fatalf("footprint estimate at %d proxies must exceed the heap estimate", n)
		}
	}
}

// The reserve held back for the OS and co-tenants: a floor on small boxes, a
// share of RAM on large ones. A box that sizes the reserve wrong warns on a
// healthy pool or stays silent on a thrashing one.
func TestRAMReserveFloorAndPercent(t *testing.T) {
	// All values in MiB.
	cases := []struct {
		ram  int64
		want int64
	}{
		{1024, 300}, // 10% is 102, below the floor
		{1930, 300},
		{3000, 300}, // 10% is exactly the floor
		{3403, 340}, // 10% wins above it
		{23979, 2397},
	}
	for _, c := range cases {
		if got := ramReserve(c.ram*testMiB) >> 20; got != c.want {
			t.Errorf("ramReserve(%d MiB) = %d MiB, want %d MiB", c.ram, got, c.want)
		}
	}
	if got := ramReserve(0); got != 0 {
		t.Errorf("an unknown box reserves nothing, got %d", got)
	}
}

// Boxes measured in the running fleet (days of uptime) that were flowing with no
// resource pressure must stay silent under BOTH checks with the limits they
// actually ran with. Boxes are named by role, not by address.
func TestResourceConfigDoesNotWarnOnMeasuredHealthyBoxes(t *testing.T) {
	healthy := []struct {
		name string
		in   resourceConfigInput
	}{
		{"5 GiB box: 4147 proxies, 3072 MiB limit", resourceConfigInput{Proxies: 4147, GoMemLimit: 3072 * testMiB, EffectiveRAMBytes: 4761 * testMiB}},
		{"3.3 GiB box: 1172 proxies, 2355 MiB limit", resourceConfigInput{Proxies: 1172, GoMemLimit: 2355 * testMiB, EffectiveRAMBytes: 3403 * testMiB}},
		{"2 GiB box: 975 proxies, 1741 MiB limit", resourceConfigInput{Proxies: 975, GoMemLimit: 1741 * testMiB, EffectiveRAMBytes: 1968 * testMiB}},
		{"2 GiB box: 1282 proxies, 1741 MiB limit", resourceConfigInput{Proxies: 1282, GoMemLimit: 1741 * testMiB, EffectiveRAMBytes: 1968 * testMiB}},
		{"2 GiB box: 1238 proxies, 1741 MiB limit", resourceConfigInput{Proxies: 1238, GoMemLimit: 1741 * testMiB, EffectiveRAMBytes: 1968 * testMiB}},
		{"1.7 GiB box: 935 proxies, 1536 MiB limit", resourceConfigInput{Proxies: 935, GoMemLimit: 1536 * testMiB, EffectiveRAMBytes: 1707 * testMiB}},
		{"3 GiB box: 887 proxies, 2150 MiB limit", resourceConfigInput{Proxies: 887, GoMemLimit: 2150 * testMiB, EffectiveRAMBytes: 3151 * testMiB}},
		{"12 GiB box: 2374 proxies, 9523 MiB limit", resourceConfigInput{Proxies: 2374, GoMemLimit: 9523 * testMiB, EffectiveRAMBytes: 11932 * testMiB}},
		{"24 GiB box: 4275 proxies, 19149 MiB limit", resourceConfigInput{Proxies: 4275, GoMemLimit: 19149 * testMiB, EffectiveRAMBytes: 23979 * testMiB}},
		{"0.85 GiB box: 250 proxies, no limit set", resourceConfigInput{Proxies: 250, EffectiveRAMBytes: 860 * testMiB}},
		{"1.8 GiB box: 535 proxies, no limit set", resourceConfigInput{Proxies: 535, EffectiveRAMBytes: 1855 * testMiB}},
	}
	for _, c := range healthy {
		if got := resourceConfigWarnings(c.in); len(got) != 0 {
			t.Errorf("%s: unexpected warnings %q", c.name, got)
		}
	}
}

// Boxes measured under real memory pressure are exactly what these warnings
// exist for, and each one must be caught by the check that describes it. A box
// whose pool does not fit the RAM is caught by the box check even when its Go
// heap fits its soft limit fine: the two limits answer different questions, and
// a pool that thrashes physical RAM can sit comfortably under a heap limit.
func TestResourceConfigWarnsOnBoxesMeasuredUnderPressure(t *testing.T) {
	pressured := []struct {
		name string
		in   resourceConfigInput
		want int      // how many warnings this box must produce
		all  []string // substrings that must all appear across those warnings
	}{
		{
			// The measured thrashing box: 2001 proxies on 1930 MiB of RAM. Its
			// heap (about 1400 MiB) does fit the 1434 MiB soft limit, so ONLY
			// the box check can catch this one. The old check compared the
			// whole-process footprint against GOMEMLIMIT and missed it.
			"2 GiB box: 2001 proxies, 1434 MiB limit, 1930 MiB RAM",
			resourceConfigInput{Proxies: 2001, GoMemLimit: 1434 * testMiB, EffectiveRAMBytes: 1930 * testMiB},
			1,
			[]string{"this pool of 2001 proxies is expected to need about", "this box gives the provider about 1930 MiB"},
		},
		{
			// A 0.9 GiB box running 1054 proxies: both checks fire, heap first.
			"0.9 GiB box: 1054 proxies, 512 MiB limit, 925 MiB RAM",
			resourceConfigInput{Proxies: 1054, GoMemLimit: 512 * testMiB, EffectiveRAMBytes: 925 * testMiB},
			2,
			[]string{"GOMEMLIMIT=512 MiB", "1054 proxies is expected to need about", "925 MiB"},
		},
		{
			// Borderline, and allowed to warn: the measured heap sat at 81% of
			// this limit, which is what a soft limit this tight predicts. The
			// pool does fit the box, so the box check stays quiet.
			"1.6 GiB box: 934 proxies, 512 MiB limit, 1642 MiB RAM",
			resourceConfigInput{Proxies: 934, GoMemLimit: 512 * testMiB, EffectiveRAMBytes: 1642 * testMiB},
			1,
			[]string{"GOMEMLIMIT=512 MiB"},
		},
		{
			// A cgroup-capped box: the soft limit is generous and the pool
			// overflows the ceiling the box actually gives the provider.
			"1 GiB cgroup-capped box: 5000 proxies, 3072 MiB limit, 1024 MiB RAM",
			resourceConfigInput{Proxies: 5000, GoMemLimit: 3072 * testMiB, EffectiveRAMBytes: 1024 * testMiB},
			2,
			[]string{"GOMEMLIMIT=3072 MiB", "this box gives the provider about 1024 MiB"},
		},
	}
	for _, c := range pressured {
		got := resourceConfigWarnings(c.in)
		if len(got) != c.want {
			t.Errorf("%s: got %d warnings %q, want %d", c.name, len(got), got, c.want)
			continue
		}
		joined := strings.Join(got, " | ")
		for _, w := range c.all {
			if !strings.Contains(joined, w) {
				t.Errorf("%s: warnings %q do not mention %q", c.name, got, w)
			}
		}
	}
}

// The heap warning must never tell the operator to raise GOMEMLIMIT above the
// RAM the box actually has: the whole point of the message is that the limit
// and the machine are both finite, and advice that trades one for the other is
// worse than no advice. With an unknown box it must name no number at all
// rather than say "0 MiB".
func TestHeapWarningNeverAdvisesAboveTheBoxRAM(t *testing.T) {
	in := resourceConfigInput{Proxies: 5000, GoMemLimit: 100 * testMiB, EffectiveRAMBytes: 1024 * testMiB}
	got := strings.Join(resourceConfigWarnings(in), " | ")
	if !strings.Contains(got, "1024 MiB") {
		t.Fatalf("the heap warning must name the box RAM it cannot exceed, got %q", got)
	}
	unknown := strings.Join(resourceConfigWarnings(resourceConfigInput{Proxies: 5000, GoMemLimit: 100 * testMiB}), " | ")
	if strings.Contains(unknown, "the 0 MiB this box") {
		t.Fatalf("with an unknown box the warning must name no RAM figure, got %q", unknown)
	}
	if !strings.Contains(unknown, "never above what this box actually has") {
		t.Fatalf("with an unknown box the warning must still cap the advice, got %q", unknown)
	}
}

// A cgroup-limited box: the footprint includes swap while memory.max does not,
// so the box check is the least validated path. It still has to fire rather
// than go silent when the pool is far past the ceiling.
func TestRAMCheckFiresUnderACgroupCeiling(t *testing.T) {
	in := resourceConfigInput{Proxies: 3000, GoMemLimit: 4096 * testMiB, EffectiveRAMBytes: 1024 * testMiB}
	got := strings.Join(resourceConfigWarnings(in), " | ")
	if !strings.Contains(got, "this box gives the provider about 1024 MiB") {
		t.Fatalf("a pool far past a 1 GiB cgroup ceiling must warn, got %q", got)
	}
}

// The startup warning reads the Go soft memory limit, so it can only be right if
// it runs AFTER the tier/profile limits are applied. A provider that ran the
// check before them saw no limit at all on every default, auto, eco and turbo
// node, and stayed silent on boxes that were actually thrashing. Pin both
// halves of the contract: the limit and the pool count reach the warning, and
// the warning fires exactly once no matter how many proxies launch.
func TestResourceConfigWarningsOnceReadsTheLimitInForce(t *testing.T) {
	// resourceConfigInputFrom reads both from the process environment; a pinned
	// GOGC under URNETWORK_PROFILE=auto adds a third warning and would break the
	// two-line assertion on a CI runner or a developer shell that sets them.
	t.Setenv("GOGC", "")
	t.Setenv("URNETWORK_PROFILE", "")
	var mu sync.Mutex
	var lines []string
	oldGather := resourceConfigGather
	resourceConfigGather = func() resourceConfigInput {
		return resourceConfigInputFrom(1054, 512*testMiB, 925*testMiB)
	}
	oldLogf := importantLogHook
	importantLogHook = func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}
	t.Cleanup(func() {
		resourceConfigGather = oldGather
		importantLogHook = oldLogf
		resourceConfigWarnOnce = sync.Once{}
	})
	resourceConfigWarnOnce = sync.Once{}

	resourceConfigWarningsOnce()
	resourceConfigWarningsOnce() // every later proxy must stay silent
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("the startup warning must produce one line per problem and stay at that, got %d: %q", len(lines), lines)
	}
	heap, box := lines[0], lines[1]
	if !strings.Contains(heap, "GOMEMLIMIT=512 MiB") || !strings.Contains(heap, "this pool of 1054 proxies is expected to need") {
		t.Errorf("first warning must be the heap one, got %q", heap)
	}
	if !strings.Contains(box, "this box gives the provider about 925 MiB") {
		t.Errorf("second warning must be the box one, got %q", box)
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
