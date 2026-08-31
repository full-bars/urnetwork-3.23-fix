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
	default:
		renderDayGraph(snaps)
		renderHourGraph(snaps)
		renderMonthGraph(snaps)
	}
	return nil
}

// dayBucket is one bar in a day graph.
type dayBucket struct {
	day      time.Time
	billable uint64
	total    uint64
}

// bucketSnapshots converts cumulative-per-process snapshots into per-bucket
// deltas (billable bytes moved within each bucket). Because each snapshot is
// cumulative since process start, a bucket's value is (newest snapshot in
// bucket) - (last snapshot before bucket start).
func deltaBuckets(snaps []usageSnapshot, truncate func(time.Time) time.Time, nBuckets int, now time.Time) []dayBucket {
	if len(snaps) == 0 {
		return nil
	}
	type pair struct {
		t      time.Time
		b, tot uint64
	}
	// Sort by ts ascending (reader already does, but be safe).
	sorted := make([]usageSnapshot, len(snaps))
	copy(sorted, snaps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TS.Before(sorted[j].TS) })

	// Map each snapshot to its bucket.
	bucketIDs := make([]int64, len(sorted))
	bucketOf := func(t time.Time) int64 { return truncate(t).Unix() }
	cumToBucket := map[int64]pair{}
	var lastBillable, lastTotal uint64
	for i, s := range sorted {
		id := bucketOf(s.TS)
		cur, ok := cumToBucket[id]
		if !ok || s.RX+s.TX >= cur.tot {
			// keep the max within the bucket (latest cumulative in bucket)
			cur = pair{t: s.TS, b: s.BillableRX + s.BillableTX, tot: s.RX + s.TX}
		}
		cumToBucket[id] = cur
		bucketIDs[i] = id
	}

	// Determine the reference (newest) cumulative at each bucket boundary.
	// For a bucket, its "billable moved" = bucketMax - previousBucketMax.
	// Build ordered bucket list.
	var ids []int64
	for id := range cumToBucket {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]dayBucket, 0, len(ids))
	prevB, prevT := uint64(0), uint64(0)
	_ = lastBillable
	_ = lastTotal
	for _, id := range ids {
		cur := cumToBucket[id]
		out = append(out, dayBucket{
			day:      time.Unix(id, 0).UTC(),
			billable: satSub(cur.b, prevB),
			total:    satSub(cur.tot, prevT),
		})
		prevB, prevT = cur.b, cur.tot
	}
	_ = now
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
	// Keep last 24 buckets ending near now.
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
