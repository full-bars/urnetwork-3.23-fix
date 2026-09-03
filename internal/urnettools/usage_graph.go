package urnettools

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// cmdUsageGraph renders a time-series bar chart from usage history for the
// targeted provider. view ∈ {day, hour, month, ""} — "" defaults to all three.
func cmdUsageGraph(targetArgs []string, view string) error {
	p, _, err := resolveTargetUsage(targetArgs)
	if err != nil {
		return err
	}
	snaps := readUsageHistory(p.StateDir)
	if len(snaps) == 0 {
		fmt.Printf("No usage history yet for %s.\n", providerLabel(p))
		return nil
	}

	switch view {
	case "day":
		renderDayGraph(snaps)
	case "hour":
		renderHourGraph(snaps)
	case "month":
		renderMonthGraph(snaps)
	case "":
		renderDayGraph(snaps)
		renderHourGraph(snaps)
		renderMonthGraph(snaps)
	default:
		return fmt.Errorf("unknown view %q — valid views: day, hour, month", view)
	}
	return nil
}

// dayBucket is one bar in a day graph.
type dayBucket struct {
	day      time.Time
	billable uint64
	total    uint64
}

// deltaBuckets converts cumulative-per-process snapshots into per-bucket
// deltas (billable bytes moved within each bucket). Because each snapshot is
// cumulative since process start, a bucket's value is the sum of deltas
// across all snapshots in that bucket. Restart boundaries (cumulative drops)
// are detected within and between buckets — each segment is summed
// independently so no bytes are lost when a restart falls inside a bucket.
func deltaBuckets(snaps []usageSnapshot, truncate func(time.Time) time.Time, nBuckets int, now time.Time) []dayBucket {
	if len(snaps) == 0 {
		return nil
	}
	ordered := orderChronological(snaps)

	bucketOf := func(t time.Time) int64 { return truncate(t).Unix() }

	// Per-bucket data: track max cumulative and billable for pre-restart
	// and post-restart segments. Also track the previous bucket's final max
	// so cross-bucket deltas use the correct baseline.
	type bucketInfo struct {
		d                      time.Time
		maxCumPreRestart       uint64
		maxBillPreRestart      uint64
		maxCumPostRestart      uint64
		maxBillPostRestart     uint64
		restartCum             uint64 // cumulative at the MOST RECENT restart point (baseline for the current post-restart segment)
		restartBill            uint64
		postRestartSettled     uint64 // sum of deltas from post-restart segments already ended by a LATER restart in this bucket
		postRestartBillSettled uint64
		hasRestart             bool
		crossBucketRestart     bool // restart happened at a bucket boundary (not within the bucket)
		hasData                bool
	}
	byID := map[int64]*bucketInfo{}

	// prevMax tracks the previous bucket's final cumulative max for
	// computing cross-bucket deltas. Reset to 0 on cross-bucket restarts.
	var prevMaxCum, prevMaxBill uint64
	var prevBucketID int64
	var prevCum uint64 // previous snapshot's cumulative (combined, for max tracking)
	// Per-field previous snapshot values for restart detection: an asymmetric
	// dip (one field dropping while the combined sum still rises) must be
	// caught, or per-field deltas later floor to 0 and lose real bytes.
	var prevRX, prevTX, prevBillRX, prevBillTX uint64
	first := true

	for _, s := range ordered {
		id := bucketOf(s.TS)
		curCum := s.RX + s.TX
		curBill := s.BillableRX + s.BillableTX

		bd, ok := byID[id]
		if !ok {
			d := time.Unix(id, 0).UTC()
			bd = &bucketInfo{d: d}
			byID[id] = bd
		}

		if first {
			bd.maxCumPreRestart = curCum
			bd.maxBillPreRestart = curBill
			bd.maxCumPostRestart = curCum
			bd.maxBillPostRestart = curBill
			bd.hasData = true
			prevCum = curCum
			prevRX, prevTX, prevBillRX, prevBillTX = s.RX, s.TX, s.BillableRX, s.BillableTX
			first = false
		} else if id != prevBucketID {
			// Bucket boundary: capture previous bucket's final max for
			// cross-bucket delta computation. Do NOT accumulate into the
			// old bucket here — per-segment deltas within a bucket are
			// summed in the output phase.
			if old, ok := byID[prevBucketID]; ok {
				if !old.hasRestart {
					prevMaxCum = old.maxCumPreRestart
					prevMaxBill = old.maxBillPreRestart
				}
				// else: prevMaxCum already set from the restart case.
			}
			// Detect cross-bucket restart (any field drops between adjacent snapshots).
			if s.RX < prevRX || s.TX < prevTX || s.BillableRX < prevBillRX || s.BillableTX < prevBillTX {
				bd.hasRestart = true
				bd.crossBucketRestart = true
				bd.maxCumPreRestart = prevCum
				bd.maxBillPreRestart = 0 // cross-bucket: segment 1 contribution is 0
				bd.maxCumPostRestart = curCum
				bd.maxBillPostRestart = curBill
				prevMaxCum = 0
				prevMaxBill = 0
			} else {
				bd.maxCumPreRestart = curCum
				bd.maxBillPreRestart = curBill
				bd.maxCumPostRestart = curCum
				bd.maxBillPostRestart = curBill
			}
			bd.hasData = true
		} else if s.RX < prevRX || s.TX < prevTX || s.BillableRX < prevBillRX || s.BillableTX < prevBillTX {
			// Same-bucket restart: a cumulative field dropped within this bucket.
			if bd.hasRestart {
				// This bucket already had a restart (same-bucket or the
				// cross-bucket restart that started it) whose post-restart
				// segment is now itself ending. Settle that segment's delta
				// into the running total before resetting the baseline for
				// the new segment -- otherwise only the pair (restartCum,
				// maxCumPostRestart) from the MOST RECENT restart survives,
				// silently discarding growth from every earlier post-restart
				// segment in this bucket.
				bd.postRestartSettled += satSub(bd.maxCumPostRestart, bd.restartCum)
				bd.postRestartBillSettled += satSub(bd.maxBillPostRestart, bd.restartBill)
			}
			bd.hasRestart = true
			bd.restartCum = curCum // restart trigger snapshot (baseline for post-restart segment)
			bd.restartBill = curBill
			bd.maxCumPostRestart = curCum
			bd.maxBillPostRestart = curBill
		} else if bd.hasRestart {
			// Post-restart growth: update the post-restart max.
			bd.maxCumPostRestart = curCum
			bd.maxBillPostRestart = curBill
		} else {
			// Normal growth within the same bucket.
			bd.maxCumPreRestart = curCum
			bd.maxBillPreRestart = curBill
		}

		prevCum = curCum
		prevRX, prevTX, prevBillRX, prevBillTX = s.RX, s.TX, s.BillableRX, s.BillableTX
		prevBucketID = id
	}

	// Flush the last bucket's final max.
	if old, ok := byID[prevBucketID]; ok && !first {
		if !old.hasRestart {
			prevMaxCum = old.maxCumPreRestart
			prevMaxBill = old.maxBillPreRestart
		}
	}

	// Reset for the output phase: each bucket's delta is computed
	// sequentially from the previous bucket's max, starting at 0.
	prevMaxCum = 0
	prevMaxBill = 0

	// Build ordered bucket list and compute per-bucket deltas.
	var ids []int64
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]dayBucket, 0, len(ids))
	for _, id := range ids {
		bd := byID[id]
		var total, billable uint64
		if bd.hasRestart {
			// Segment 1 delta = maxPreRestart - prevMax.
			total = satSub(bd.maxCumPreRestart, prevMaxCum)
			billable = satSub(bd.maxBillPreRestart, prevMaxBill)
			// Remaining segments: postRestartSettled sums every
			// post-restart segment already ended by a LATER restart in
			// this bucket (see the settle step above); the current
			// (most recent) segment is added on top via restartCum as its
			// baseline. A cross-bucket restart leaves restartCum at its
			// zero value unless a later same-bucket restart reset it, so
			// this also covers the "new process from ~0" cross-bucket case
			// without a separate branch.
			total += bd.postRestartSettled + satSub(bd.maxCumPostRestart, bd.restartCum)
			billable += bd.postRestartBillSettled + satSub(bd.maxBillPostRestart, bd.restartBill)
			prevMaxCum = bd.maxCumPostRestart
			prevMaxBill = bd.maxBillPostRestart
		} else {
			// Simple delta from previous bucket's max.
			total = satSub(bd.maxCumPreRestart, prevMaxCum)
			billable = satSub(bd.maxBillPreRestart, prevMaxBill)
			prevMaxCum = bd.maxCumPreRestart
			prevMaxBill = bd.maxBillPreRestart
		}
		out = append(out, dayBucket{day: bd.d, billable: billable, total: total})
	}
	// Drop a trailing bucket that is the CURRENT, incomplete period (its
	// window has not elapsed yet); rendering it as a full bar overstates the
	// range. The bucket key id == truncate(now) means it is the present period.
	if len(out) > 0 && !truncate(now).IsZero() && out[len(out)-1].day.Equal(truncate(now)) {
		out = out[:len(out)-1]
	}
	// Honor the requested number of buckets: keep only the most recent N.
	if nBuckets > 0 && len(out) > nBuckets {
		out = out[len(out)-nBuckets:]
	}
	// now / nBuckets are consumed above; the graph titles no longer overstate.
	return out
}

// renderBar prints one ASCII bar labeled with `label`.
func renderBar(label string, value, maxValue uint64, maxWidth int) {
	if maxValue == 0 {
		fmt.Printf("%-14s (no traffic)\n", label)
		return
	}
	w := int(float64(value) / float64(maxValue) * float64(maxWidth))
	if w < 1 && value > 0 {
		w = 1
	}
	fmt.Printf("%-14s %s %s\n", label, strings.Repeat("=", w), fmtBytes(value))
}

func renderDayGraph(snaps []usageSnapshot) {
	fmt.Println("USAGE BY DAY (billable)")
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}, 30, time.Now())
	if len(buckets) > 7 {
		buckets = buckets[len(buckets)-7:]
	}
	var max uint64
	for _, b := range buckets {
		if b.billable > max {
			max = b.billable
		}
	}
	for _, b := range buckets {
		renderBar(b.day.Format("Mon 01-02"), b.billable, max, 18)
	}
	fmt.Println()
}

func renderHourGraph(snaps []usageSnapshot) {
	fmt.Println("USAGE BY HOUR (billable, last 24h)")
	now := time.Now().UTC()
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
	}, 24, now)
	if len(buckets) > 24 {
		buckets = buckets[len(buckets)-24:]
	}
	var max uint64
	for _, b := range buckets {
		if b.billable > max {
			max = b.billable
		}
	}
	for _, b := range buckets {
		renderBar(b.day.Format("15:04"), b.billable, max, 18)
	}
	fmt.Println()
}

func renderMonthGraph(snaps []usageSnapshot) {
	fmt.Println("USAGE BY MONTH (billable, last 12mo)")
	buckets := deltaBuckets(snaps, func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}, 12, time.Now())
	if len(buckets) > 12 {
		buckets = buckets[len(buckets)-12:]
	}
	var max uint64
	for _, b := range buckets {
		if b.billable > max {
			max = b.billable
		}
	}
	for _, b := range buckets {
		renderBar(b.day.Format("2006-01"), b.billable, max, 18)
	}
	fmt.Println()
}
