package connect

import (
	"sync/atomic"
	"time"
)

// logThrottle rate-limits a high-volume log line to at most one emission per
// interval, counting how many emissions were suppressed in between so the next
// allowed line can print a "(N suppressed)" tail. It replaces four byte-for-byte
// identical shouldLogX implementations (auth/select/write err in transport.go,
// oob err in transfer_contract_manager.go), each of which previously carried its
// own pair of package-global atomics.
//
// Concurrency: lock-free. When multiple goroutines race the same interval
// boundary, exactly one wins the CompareAndSwap and emits; the rest are counted
// as suppressed. That is the intended rate-limiting behavior, not a loss.
type logThrottle struct {
	// intervalNanos is the minimum spacing between emitted lines. Immutable
	// after construction, so it needs no synchronization.
	intervalNanos int64
	// lastNanos is when a line was last emitted, in unix nanos. The
	// CompareAndSwap on this field is what elects the single winner among
	// goroutines racing the same interval boundary.
	lastNanos atomic.Int64
	// suppressed counts lines dropped since the last emission. Swapped to 0 by
	// the emitting call, which is what transfers ownership of the count to the
	// line being printed.
	suppressed atomic.Int64
}

// newLogThrottle returns a throttle that allows at most one line per interval.
//
// The zero lastNanos means the first call to Allow always emits, whatever the
// interval: a throttle starts unthrottled rather than swallowing the first
// occurrence of the thing it guards, which is usually the most diagnostic one.
//
// Throttles are meant to be package-level and long-lived. Constructing one per
// call site per instance defeats the purpose — the flood these guard against is
// across every transport and sequence at once, so a limiter scoped to one of
// them still emits once per instance per interval.
func newLogThrottle(interval time.Duration) *logThrottle {
	return &logThrottle{intervalNanos: int64(interval)}
}

// NewLogThrottle is the exported form of newLogThrottle, for the provider
// package. Same contract; see newLogThrottle.
func NewLogThrottle(interval time.Duration) *logThrottle {
	return &logThrottle{intervalNanos: int64(interval)}
}

// Allow reports whether a log line may be emitted as of now. When it returns
// true, the second value is the number of lines suppressed since the previous
// allowed line (reset to 0 by this call). When false, the caller should stay
// quiet; the suppression is counted internally.
func (t *logThrottle) Allow(now time.Time) (bool, int64) {
	nowNanos := now.UnixNano()
	last := t.lastNanos.Load()
	if nowNanos-last < t.intervalNanos {
		t.suppressed.Add(1)
		return false, 0
	}
	if !t.lastNanos.CompareAndSwap(last, nowNanos) {
		t.suppressed.Add(1)
		return false, 0
	}
	return true, t.suppressed.Swap(0)
}
