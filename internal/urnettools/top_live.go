package urnettools

import "time"

// topLive turns polled counter sums into live rates, the way btop does: the rate
// is the counter delta over a short sliding window, so it moves with every poll
// (down to 100ms) instead of once a second.
//
// The counters are sums across the provider's proxies, so they drop when a
// proxy is removed or respawns. A drop is never read as negative traffic: the
// window starts over and the rate is unknown until two samples exist again.
const (
	// topLiveWindow is how much history a rate averages over. One second is
	// long enough to smooth a 100ms poll and short enough to feel live.
	topLiveWindow = time.Second
	// topLiveMinSpan is the shortest span a rate is computed over; samples any
	// closer than this say too little about the rate.
	topLiveMinSpan = 50 * time.Millisecond
)

type liveSample struct {
	at         int64 // provider clock, unix nanoseconds
	billable   uint64
	total      uint64
	billableRx uint64
	billableTx uint64
	totalRx    uint64
	totalTx    uint64
}

type topLive struct {
	samples []liveSample
}

func (l *topLive) reset() {
	l.samples = l.samples[:0]
}

// add records one reading. Readings are timed by the provider's clock, not the
// arrival time, so socket latency does not show up as rate noise.
func (l *topLive) add(at int64, billable, total uint64) {
	l.addDirectional(at, billable, total, 0, 0, 0, 0)
}

func (l *topLive) addDirectional(at int64, billable, total, bRx, bTx, tRx, tTx uint64) {
	if n := len(l.samples); n > 0 {
		last := l.samples[n-1]
		if at <= last.at {
			return // the clock did not advance: nothing to learn from it
		}
		if billable < last.billable || total < last.total {
			l.samples = l.samples[:0] // counters restarted: begin a new window
		}
	}
	l.samples = append(l.samples, liveSample{
		at:         at,
		billable:   billable,
		total:      total,
		billableRx: bRx,
		billableTx: bTx,
		totalRx:    tRx,
		totalTx:    tTx,
	})
	// Keep the youngest sample that is at least a window old and everything
	// after it, but never fewer than two, so a slow poll still has a span.
	for len(l.samples) > 2 && time.Duration(at-l.samples[1].at) >= topLiveWindow {
		l.samples = l.samples[1:]
	}
}

// rates returns billable and total bytes per second over the window. ok is
// false until there are two readings far enough apart to say anything.
func (l *topLive) rates() (billable, total float64, ok bool) {
	if len(l.samples) < 2 {
		return 0, 0, false
	}
	first, last := l.samples[0], l.samples[len(l.samples)-1]
	span := time.Duration(last.at - first.at)
	if span < topLiveMinSpan {
		return 0, 0, false
	}
	secs := span.Seconds()
	return float64(last.billable-first.billable) / secs, float64(last.total-first.total) / secs, true
}

func (l *topLive) ratesDirectional() (bRx, bTx, tRx, tTx float64, ok bool) {
	if len(l.samples) < 2 {
		return 0, 0, 0, 0, false
	}
	first, last := l.samples[0], l.samples[len(l.samples)-1]
	span := time.Duration(last.at - first.at)
	if span < topLiveMinSpan {
		return 0, 0, 0, 0, false
	}
	secs := span.Seconds()
	if last.billableRx < first.billableRx || last.billableTx < first.billableTx ||
		last.totalRx < first.totalRx || last.totalTx < first.totalTx {
		return 0, 0, 0, 0, false
	}
	return float64(last.billableRx-first.billableRx) / secs,
		float64(last.billableTx-first.billableTx) / secs,
		float64(last.totalRx-first.totalRx) / secs,
		float64(last.totalTx-first.totalTx) / secs,
		true
}

// recent is rates over the newest span of at least minSpan, for the zoom graph:
// short enough that it moves with each poll, long enough that a 100ms poll's
// counter granularity does not turn it into noise. ok is false until the window
// holds a span that long.
func (l *topLive) recent(minSpan time.Duration) (billable, total float64, ok bool) {
	if len(l.samples) < 2 {
		return 0, 0, false
	}
	last := l.samples[len(l.samples)-1]
	from := l.samples[0]
	for i := len(l.samples) - 2; i >= 0; i-- {
		if time.Duration(last.at-l.samples[i].at) >= minSpan {
			from = l.samples[i]
			break
		}
	}
	span := time.Duration(last.at - from.at)
	if span < topLiveMinSpan {
		return 0, 0, false
	}
	secs := span.Seconds()
	return float64(last.billable-from.billable) / secs, float64(last.total-from.total) / secs, true
}

func (l *topLive) recentDirectional(minSpan time.Duration) (bRx, bTx, tRx, tTx float64, ok bool) {
	if len(l.samples) < 2 {
		return 0, 0, 0, 0, false
	}
	last := l.samples[len(l.samples)-1]
	from := l.samples[0]
	for i := len(l.samples) - 2; i >= 0; i-- {
		if time.Duration(last.at-l.samples[i].at) >= minSpan {
			from = l.samples[i]
			break
		}
	}
	span := time.Duration(last.at - from.at)
	if span < topLiveMinSpan {
		return 0, 0, 0, 0, false
	}
	secs := span.Seconds()
	if last.billableRx < from.billableRx || last.billableTx < from.billableTx ||
		last.totalRx < from.totalRx || last.totalTx < from.totalTx {
		return 0, 0, 0, 0, false
	}
	return float64(last.billableRx-from.billableRx) / secs,
		float64(last.billableTx-from.billableTx) / secs,
		float64(last.totalRx-from.totalRx) / secs,
		float64(last.totalTx-from.totalTx) / secs,
		true
}
