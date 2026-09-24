package urnettools

import (
	"math"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// burst is the true per-second billable rate at provider sample index j: bursty,
// heavy tailed, with quiet gaps, like real relay traffic. It is a pure function
// of j, so a sample never changes once it exists.
func burst(j int64) float64 {
	x := uint64(j)*0x9E3779B97F4A7C15 + 0xD1B54A32D192ED03
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	u := float64(x%10000) / 10000
	switch {
	case u < 0.30:
		return 0
	case u < 0.85:
		return (0.5 + 4*u) * demoMiB
	default:
		return (15 + 40*u) * demoMiB // a burst
	}
}

// fakeProvider is a provider whose per-second sampler ticks at a fixed sub-second
// phase, like the real one: its newest history sample and the wall clock second
// that a poll lands in are NOT the same index, and which one a poll sees depends
// on where the poll falls relative to the tick.
type fakeProvider struct {
	phase float64 // seconds past the second at which the sampler ticks
	epoch int64   // wall second of provider sample 0
}

func (f fakeProvider) newest(t float64) int64 { return int64(math.Floor(t - f.phase)) }

func (f fakeProvider) snapshot(t *testing.T, at float64) *NodeSnapshot {
	s := topFlowing(t)
	j := f.newest(at)
	hist := make([]float64, 600)
	total := make([]float64, 600)
	for i := range hist {
		hist[i] = burst(j - 599 + int64(i))
		total[i] = hist[i] * 1.6
	}
	s.Rate.HistoryBps = hist
	s.Traffic.TotalHistoryBps = total
	s.Rate.HistorySeq = uint64(j)
	s.Now = time.Unix(f.epoch+int64(math.Floor(at)), 0).UTC().Format(time.RFC3339)
	return s
}

// counters returns the live cumulative sums at t: bytes accumulate at the burst
// rate of the sample interval they fall in.
func (f fakeProvider) counters(at float64) (billable, total uint64) {
	var sum float64
	for j := f.newest(0); j < f.newest(at); j++ {
		sum += burst(j)
	}
	frac := at - f.phase - math.Floor(at-f.phase)
	sum += burst(f.newest(at)+1) * frac
	return uint64(sum), uint64(sum * 1.6)
}

// The user-visible rule: once a column is complete it must never change as the
// graph scrolls left; only the newest still-filling column may move. This drives
// the real model with a real-shaped stream: bursty traffic, a provider whose
// ticks land half a second off the wall clock, and snapshot polls that jitter
// across that phase, so the offset between "the second a poll lands in" and "the
// provider's newest sample" flips from one snapshot to the next.
func TestTopGraphHistoryDoesNotChangeAsItScrolls(t *testing.T) {
	const cols = 158 // a wide terminal's plot
	f := fakeProvider{phase: 0.5, epoch: 1_790_000_000}
	m, clock := liveModel(t, 100*time.Millisecond)
	base := clock.Now()

	// Snapshot times: about one a second, drifting across the sampler's tick.
	snapshotAt := func(k int) float64 { return 30 + float64(k) + 0.5 + 0.4*math.Sin(float64(k)*1.7) }

	seen := map[int64]float64{}
	changes, snapshots, k := 0, 0, 0
	offsets := map[int64]bool{}              // (wall second a poll lands in) minus (provider's newest sample)
	for at := 30.0; at < 30+180; at += 0.1 { // three minutes of 100ms ticks
		clock.t = base.Add(time.Duration((at - 30) * float64(time.Second)))
		if gen, _, ok := m.wantTraffic(clock.Now()); ok {
			b, tot := f.counters(at)
			m.applyTraffic(gen, &LiveTraffic{AtUnixNano: int64(at * 1e9), BillableBytes: b, TotalBytes: tot}, nil)
		}
		for at >= snapshotAt(k) {
			m.apply(m.gen, f.snapshot(t, snapshotAt(k)), nil)
			offsets[int64(math.Floor(snapshotAt(k)))-f.newest(snapshotAt(k))] = true
			snapshots++
			k++
		}

		sr := m.series()
		// The graph gives the newest column to the live tail, so the history
		// is bucketed into one column fewer.
		ids, vals := tui.GraphBuckets(sr.billable, cols-1, sr.anchor, topGraphCapacity)
		// Every bucket but the newest two is complete: it must hold the value it had
		// the first time it was seen.
		for i := 0; i < len(ids)-2; i++ {
			if prev, ok := seen[ids[i]]; ok && math.Abs(prev-vals[i]) > 1e-6 {
				changes++
				if changes <= 5 {
					t.Logf("t=%.1f: completed bucket %d changed %.0f -> %.0f", at, ids[i], prev, vals[i])
				}
			}
			seen[ids[i]] = vals[i]
		}
	}
	// The scenario must actually have happened, or a pass means nothing.
	if snapshots < 150 {
		t.Fatalf("only %d snapshots were applied in 3 minutes; the driver is not exercising the model", snapshots)
	}
	if len(offsets) < 2 {
		t.Fatalf("the poll/tick offset never flipped (%v); the scenario does not reproduce the phase problem", offsets)
	}
	if changes > 0 {
		t.Fatalf("%d completed columns changed as the graph scrolled; history must hold still", changes)
	}
}
