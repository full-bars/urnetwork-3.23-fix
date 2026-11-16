package urnettools

import (
	"math"
	"time"
)

// top_window.go keeps downsampled throughput history for the 1h and 24h
// windows. The provider keeps ten minutes of per-second samples (a fixed
// 600-slot ring) and nothing longer; rather than change that ring, top bins
// the samples it already polls at one second into one-minute and
// fifteen-minute buckets. The memory is bounded by the bucket counts, 60 and
// 96, whatever the provider or the poll rate does.

const (
	// topMinuteBuckets holds one hour of one-minute buckets.
	topMinuteBuckets = 60
	// topQuarterBuckets holds twenty-four hours of fifteen-minute buckets.
	topQuarterBuckets = 96
)

// topWindow is the downsampled history of one throughput series. Every bucket
// is an average of the samples that fell into it, and every bucket is keyed by
// its absolute clock index, so a young window fills in from the right and old
// buckets are overwritten by buckets one period later.
type topWindow struct {
	minuteSum    [topMinuteBuckets]float64
	minuteCount  [topMinuteBuckets]int
	minuteIndex  [topMinuteBuckets]int64
	quarterSum   [topQuarterBuckets]float64
	quarterCount [topQuarterBuckets]int
	quarterIndex [topQuarterBuckets]int64
}

// reset drops every bucket.
func (w *topWindow) reset() { *w = topWindow{} }

// push records one sample. It is called with the provider's own rate (not the
// live tail), so the same reading lands in both resolutions.
func (w *topWindow) push(rate float64, at time.Time) {
	mi := at.Unix() / 60
	i := int(mod(mi, int64(topMinuteBuckets)))
	if w.minuteIndex[i] != mi {
		w.minuteSum[i], w.minuteCount[i], w.minuteIndex[i] = 0, 0, mi
	}
	w.minuteSum[i] += rate
	w.minuteCount[i]++
	qi := at.Unix() / 900
	j := int(mod(qi, int64(topQuarterBuckets)))
	if w.quarterIndex[j] != qi {
		w.quarterSum[j], w.quarterCount[j], w.quarterIndex[j] = 0, 0, qi
	}
	w.quarterSum[j] += rate
	w.quarterCount[j]++
}

// mod is the non-negative remainder of a divided by b, for indices derived
// from the unix clock.
func mod(a, b int64) int64 {
	r := a % b
	if r < 0 {
		r += b
	}
	return r
}

// bucket returns the average of the bucket at index want, or NaN when that
// bucket does not exist (too old, or never filled).
func bucket(sum []float64, count []int, index []int64, want int64, buckets int) float64 {
	i := int(mod(want, int64(buckets)))
	if index[i] != want || count[i] == 0 {
		return math.NaN()
	}
	return sum[i] / float64(count[i])
}

// windowFor returns the downsampled series for the last h hours, oldest
// first. h must be 1 or 24. Buckets with no samples are NaN, so the graph
// leaves them empty instead of drawing a fake zero.
func (w *topWindow) windowFor(h int, now time.Time) []float64 {
	switch h {
	case 1:
		out := make([]float64, topMinuteBuckets)
		cur := now.Unix() / 60
		for i := range out {
			out[i] = bucket(w.minuteSum[:], w.minuteCount[:], w.minuteIndex[:], cur-int64(len(out)-1-i), topMinuteBuckets)
		}
		return out
	case 24:
		out := make([]float64, topQuarterBuckets)
		cur := now.Unix() / 900
		for i := range out {
			out[i] = bucket(w.quarterSum[:], w.quarterCount[:], w.quarterIndex[:], cur-int64(len(out)-1-i), topQuarterBuckets)
		}
		return out
	}
	return nil
}

// windowTitle says how much history the window carries.
func windowTitle(h int) string {
	switch h {
	case 1:
		return "last 1 hour"
	case 24:
		return "last 24 hours"
	}
	return "last 10 minutes"
}
