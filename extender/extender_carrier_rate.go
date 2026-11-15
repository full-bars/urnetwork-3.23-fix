package extender

import (
	"time"
)

// The per-carrier rate and history tracker (the extender traffic view).
//
// The relay counters are cumulative byte totals, taken on the copy path so a
// byte is counted exactly once. Everything an operator reads as a rate or a
// chart is derived here, sampled from the periodic tick rather than per byte,
// so the copy path stays a single atomic add and never computes anything.
//
// Two derived views are kept per carrier:
//   - a rate in bytes per second, from the delta of the counters since the
//     previous sample over the elapsed time;
//   - a bounded ring of one minute buckets of ingress plus egress bytes, so a
//     minute of quiet closes its bucket and a long quiet rolls the ring over
//     without ever growing. Slots carry the absolute minute they hold, so the
//     history of more than one server lines up when it is summed.
//
// The struct is not safe for concurrent use: the server guardes it, sampling
// and reading it from the status and metrics paths under its own lock is not
// needed because sampling and reading both go through the server. See
// ExtenderServer.SampleCarrierRates and ExtenderServer.carrierByteSnapshot.

const (
	// extenderRateBucketCount is how many one minute buckets the history
	// holds. Sixty minutes is the "last hour" an operator asks for.
	extenderRateBucketCount = 60
	// extenderRateBucketWidth is one bucket.
	extenderRateBucketWidth = time.Minute
	// extenderRateMinuteCount is how many of the buckets the status exposes,
	// the most recent first when read. The ring holds the rest for the chart
	// that wants a wider window.
	extenderRateMinuteCount = 30
)

// extenderCarrierRate derives rates and a minute history from the per-carrier
// relay counters. The zero value is ready and reports no traffic.
type extenderCarrierRate struct {
	haveSample     bool
	lastSampleTime time.Time
	lastIngress    [extenderCarrierPortCount]uint64
	lastEgress     [extenderCarrierPortCount]uint64

	rateIn  [extenderCarrierPortCount]uint64
	rateOut [extenderCarrierPortCount]uint64

	// slotMinute holds the absolute unix minute of each history slot, 0 when
	// the slot was never used, so a minute's bytes land in the same slot
	// whatever order servers are sampled in.
	slotMinute [extenderRateBucketCount]int64
	// slotKiB holds ingress plus egress bytes of that minute, as whole KiB, so
	// a minute of a very busy carrier still fits a uint32 (4 TiB).
	slotKiB [extenderCarrierPortCount][extenderRateBucketCount]uint32
}

// sample folds one reading of the cumulative per-carrier counters into the
// rates and the history. The first sample is a baseline: it sets the rate to
// zero and adds nothing to the history, because a delta over no known elapsed
// time is not a rate.
func (r *extenderCarrierRate) sample(now time.Time, ingress, egress [extenderCarrierPortCount]uint64) {
	if !r.haveSample {
		r.haveSample = true
		r.lastSampleTime = now
		r.lastIngress = ingress
		r.lastEgress = egress
		return
	}
	elapsed := now.Sub(r.lastSampleTime)
	if elapsed <= 0 {
		return
	}
	nanos := uint64(elapsed)
	var deltaKiB [extenderCarrierPortCount]uint32
	for i := 0; i < extenderCarrierPortCount; i++ {
		dIn := ingress[i] - r.lastIngress[i]
		dOut := egress[i] - r.lastEgress[i]
		// bytes per second over the elapsed time, with no float: a byte whose
		// second has not passed rounds off the rate, which is what a sampler
		// the operator reads once a tick wants.
		r.rateIn[i] = dIn * uint64(time.Second) / nanos
		r.rateOut[i] = dOut * uint64(time.Second) / nanos
		r.lastIngress[i] = ingress[i]
		r.lastEgress[i] = egress[i]
		deltaKiB[i] = uint32((dIn + dOut) / 1024)
	}
	r.lastSampleTime = now
	r.addKiB(now.Unix()/60, deltaKiB)
}

// addKiB adds one sample's whole KiB to its absolute minute bucket, reusing the
// slot that minute already holds, or an unused slot, or else evicting the
// oldest minute so the ring never grows.
func (r *extenderCarrierRate) addKiB(minute int64, deltaKiB [extenderCarrierPortCount]uint32) {
	idx := -1
	for j := 0; j < extenderRateBucketCount; j++ {
		if r.slotMinute[j] == minute {
			idx = j
			break
		}
	}
	if idx < 0 {
		for j := 0; j < extenderRateBucketCount; j++ {
			if r.slotMinute[j] == 0 {
				idx = j
				break
			}
		}
	}
	if idx < 0 {
		// every slot is used: the oldest minute leaves the ring
		oldest := 0
		for j := 1; j < extenderRateBucketCount; j++ {
			if r.slotMinute[j] < r.slotMinute[oldest] {
				oldest = j
			}
		}
		idx = oldest
		for i := 0; i < extenderCarrierPortCount; i++ {
			r.slotKiB[i][idx] = 0
		}
	}
	r.slotMinute[idx] = minute
	for i := 0; i < extenderCarrierPortCount; i++ {
		r.slotKiB[i][idx] += deltaKiB[i]
	}
}

// minutes returns the bytes of the most recent count minutes for one carrier,
// oldest first, so the last value is the minute now falls in. A minute with no
// sample is zero.
func (r *extenderCarrierRate) minutes(carrierIdx int, count int, now time.Time) []uint32 {
	if count > extenderRateBucketCount {
		count = extenderRateBucketCount
	}
	out := make([]uint32, 0, count)
	nowMinute := now.Unix() / 60
	for back := count - 1; 0 <= back; back-- {
		minute := nowMinute - int64(back)
		var value uint32
		for j := 0; j < extenderRateBucketCount; j++ {
			if r.slotMinute[j] == minute {
				value = r.slotKiB[carrierIdx][j]
				break
			}
		}
		out = append(out, value)
	}
	return out
}
