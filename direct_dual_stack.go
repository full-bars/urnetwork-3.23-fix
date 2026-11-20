package connect

import (
	"sync/atomic"
	"time"
)

// DirectFamilyStats holds atomic counters and timestamps for one address family
// on the native direct path (proxy slot 0).
type DirectFamilyStats struct {
	ConnectsOK      atomic.Uint64
	ConnectsFailed  atomic.Uint64
	RxBytes         atomic.Uint64
	TxBytes         atomic.Uint64
	LastSuccessUnix atomic.Int64
}

// DirectFamilySnapshot is a point-in-time copy of a family's counters.
type DirectFamilySnapshot struct {
	ConnectsOK      uint64 `json:"connects_ok"`
	ConnectsFailed  uint64 `json:"connects_failed"`
	RxBytes         uint64 `json:"rx_bytes"`
	TxBytes         uint64 `json:"tx_bytes"`
	LastSuccessUnix int64  `json:"last_success_unix,omitempty"`
}

// Snapshot returns a point-in-time copy of the family stats.
func (s *DirectFamilyStats) Snapshot() DirectFamilySnapshot {
	return DirectFamilySnapshot{
		ConnectsOK:      s.ConnectsOK.Load(),
		ConnectsFailed:  s.ConnectsFailed.Load(),
		RxBytes:         s.RxBytes.Load(),
		TxBytes:         s.TxBytes.Load(),
		LastSuccessUnix: s.LastSuccessUnix.Load(),
	}
}

// DirectDualStackCounters holds dual-stack IPv4 and IPv6 counters for the direct slot.
type DirectDualStackCounters struct {
	IPv4 DirectFamilyStats
	IPv6 DirectFamilyStats
}

var globalDirectDualStack DirectDualStackCounters

// DirectDualStackStats returns the global direct dual-stack counters.
func DirectDualStackStats() *DirectDualStackCounters {
	return &globalDirectDualStack
}

// ResetDirectDualStackForTest resets all direct counters to zero for testing.
func ResetDirectDualStackForTest() {
	globalDirectDualStack.IPv4.ConnectsOK.Store(0)
	globalDirectDualStack.IPv4.ConnectsFailed.Store(0)
	globalDirectDualStack.IPv4.RxBytes.Store(0)
	globalDirectDualStack.IPv4.TxBytes.Store(0)
	globalDirectDualStack.IPv4.LastSuccessUnix.Store(0)

	globalDirectDualStack.IPv6.ConnectsOK.Store(0)
	globalDirectDualStack.IPv6.ConnectsFailed.Store(0)
	globalDirectDualStack.IPv6.RxBytes.Store(0)
	globalDirectDualStack.IPv6.TxBytes.Store(0)
	globalDirectDualStack.IPv6.LastSuccessUnix.Store(0)
}

// RecordConnect records a direct connection outcome for the given family.
func (d *DirectDualStackCounters) RecordConnect(family int, ok bool, now time.Time) {
	switch family {
	case 4:
		if ok {
			d.IPv4.ConnectsOK.Add(1)
			d.IPv4.LastSuccessUnix.Store(now.Unix())
		} else {
			d.IPv4.ConnectsFailed.Add(1)
		}
	case 6:
		if ok {
			d.IPv6.ConnectsOK.Add(1)
			d.IPv6.LastSuccessUnix.Store(now.Unix())
		} else {
			d.IPv6.ConnectsFailed.Add(1)
		}
	}
}

// AddRx adds received bytes for the given family.
func (d *DirectDualStackCounters) AddRx(family int, n uint64) {
	switch family {
	case 4:
		d.IPv4.RxBytes.Add(n)
	case 6:
		d.IPv6.RxBytes.Add(n)
	}
}

// AddTx adds transmitted bytes for the given family.
func (d *DirectDualStackCounters) AddTx(family int, n uint64) {
	switch family {
	case 4:
		d.IPv4.TxBytes.Add(n)
	case 6:
		d.IPv6.TxBytes.Add(n)
	}
}
