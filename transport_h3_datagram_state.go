package connect

import (
	"fmt"
	"sync/atomic"
)

// Process-wide H3 DATAGRAM state. H3 runs for the direct identity only, so
// there is one connection per process and one set of limits and counters; the
// reassembly budget is shared so a future per-proxy H3 stays bounded as a whole.
var (
	h3DatagramSettings = DefaultH3DatagramSettings()
	h3DatagramStats    = &H3DatagramStats{}
	h3DatagramBudget   = NewH3DatagramReassemblyBudget(h3DatagramSettings.ProcessReassemblyByteCount)

	// h3DatagramOffered counts H3 connections that offered DATAGRAM, and
	// h3DatagramAccepted those where the server accepted. Offered without
	// accepted means the server is old or declined: the connection ran on the
	// plain stream, which is the intended fallback.
	h3DatagramOffered  atomic.Uint64
	h3DatagramAccepted atomic.Uint64
	// h3DatagramDrops counts received datagram messages dropped because the
	// receive route was full. The lane is unreliable, so Transfer resends.
	h3DatagramDrops atomic.Uint64
)

// H3DatagramSnapshot is a point-in-time copy of the H3 DATAGRAM counters.
type H3DatagramSnapshot struct {
	Offered  uint64
	Accepted uint64
	// RxDrops is received messages dropped because the receive route was full.
	RxDrops uint64
	// Datagram layer counters: messages and bytes delivered, and what was refused.
	RxMessages  uint64
	RxBytes     uint64
	RxMalformed uint64
	RxDuplicate uint64
	RxChecksum  uint64
	// Gate is whether the offer is switched on right now.
	Gate bool
}

// H3DatagramCounters returns the cumulative H3 DATAGRAM counters.
func H3DatagramCounters() H3DatagramSnapshot {
	layer := h3DatagramStats.Snapshot()
	return H3DatagramSnapshot{
		Offered:     h3DatagramOffered.Load(),
		Accepted:    h3DatagramAccepted.Load(),
		RxDrops:     h3DatagramDrops.Load(),
		RxMessages:  layer.ReceivedMessageCount,
		RxBytes:     layer.ReceivedMessageByteCount,
		RxMalformed: layer.MalformedFragmentCount,
		RxDuplicate: layer.DuplicateFragmentCount,
		RxChecksum:  layer.ChecksumFailureCount,
		Gate:        H3DatagramsEnabled(),
	}
}

// HealthSuffix renders the DATAGRAM part of the [health] line, or "" until a
// connection has offered DATAGRAM, so a box that never enables it sees no new
// fields. accepted/offered says whether the server takes the offer at all.
func (self H3DatagramSnapshot) HealthSuffix() string {
	if self.Offered == 0 {
		return ""
	}
	return fmt.Sprintf(" h3_dg=%d/%d dg_rx=%d dg_rx_drop=%d",
		self.Accepted, self.Offered, self.RxMessages, self.RxDrops)
}
