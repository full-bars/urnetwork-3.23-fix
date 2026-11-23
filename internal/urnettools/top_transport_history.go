package urnettools

import "time"

// topTransportWindow is the span the Transports page covers. It matches the
// Remote page, so the two views answer over the same stretch of time.
const topTransportWindow = 60 * time.Second

// topTransportHistory is a ring of cumulative transport readings, so the page
// can difference two of them into a window share. It keeps readings, not rates:
// a share needs the pair, and the provider's counters are cumulative.
type topTransportHistory struct {
	readings []topTransportReading
}

// topTransportReading is one poll of the provider's transport block.
type topTransportReading struct {
	at         int64
	transports *LiveTransports
}

// reset empties the ring, so a reconnected provider cannot be differenced
// against counters from before the outage.
func (h *topTransportHistory) reset() { h.readings = nil }

// push records a reading. A clock that went backwards is dropped, and a repeat
// of the same instant replaces the last reading instead of adding a zero-span
// pair. Readings older than two windows are dropped, which still leaves one
// reading a window back for the share.
func (h *topTransportHistory) push(at int64, transports *LiveTransports) {
	if transports == nil || at <= 0 {
		return
	}
	if n := len(h.readings); n > 0 {
		last := h.readings[n-1]
		if at < last.at {
			return
		}
		if at == last.at {
			h.readings[n-1] = topTransportReading{at: at, transports: transports}
			return
		}
	}
	h.readings = append(h.readings, topTransportReading{at: at, transports: transports})
	cutoff := at - int64(2*topTransportWindow)
	drop := 0
	for drop < len(h.readings)-1 && h.readings[drop].at < cutoff {
		drop++
	}
	if drop > 0 {
		h.readings = append(h.readings[:0], h.readings[drop:]...)
	}
}

// newest returns the most recent reading, or ok false while none is kept.
func (h *topTransportHistory) newest() (topTransportReading, bool) {
	if len(h.readings) == 0 {
		return topTransportReading{}, false
	}
	return h.readings[len(h.readings)-1], true
}

// window returns the newest reading and the reference reading one window
// earlier: the newest reading at or before that instant, or the oldest held
// while the session is still shorter than a window. ok is false until there
// are two readings to difference.
func (h *topTransportHistory) window() (prev, cur topTransportReading, ok bool) {
	n := len(h.readings)
	if n < 2 {
		return topTransportReading{}, topTransportReading{}, false
	}
	cur = h.readings[n-1]
	start := cur.at - int64(topTransportWindow)
	prev = h.readings[0]
	for i := n - 2; i >= 0; i-- {
		if h.readings[i].at <= start {
			prev = h.readings[i]
			break
		}
	}
	return prev, cur, true
}
