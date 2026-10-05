package connect

import (
	"fmt"
	"sync/atomic"
)

// Process-wide H3 DATAGRAM state. H3 runs for the direct identity only, so
// there is one connection per process and one set of limits and counters; the
// reassembly budget is shared so a future per-proxy H3 stays bounded as a whole.
var (
	// the limits and the shared reassembly budget are resolved on first use, see
	// h3DatagramLimits, so they can scale with the provider's memory budget
	h3DatagramStats = &H3DatagramStats{}

	// h3DatagramOffered counts H3 connections that offered DATAGRAM, and
	// h3DatagramAccepted those where the server accepted. Offered without
	// accepted means the server is old or declined: the connection ran on the
	// plain stream, which is the intended fallback.
	h3DatagramOffered  atomic.Uint64
	h3DatagramAccepted atomic.Uint64
	// h3DatagramDrops counts received datagram messages dropped because the
	// receive route was full. The lane is unreliable, so Transfer resends.
	h3DatagramDrops atomic.Uint64
	// h3DatagramBlackholes counts connections whose datagram lane the send guard
	// switched off because datagrams went out and none came back.
	h3DatagramBlackholes atomic.Uint64
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
	// What this side sent: messages and bytes that went as datagrams, those
	// that were too large and went on the stream instead, and send errors.
	TxMessages       uint64
	TxBytes          uint64
	TxStreamMessages uint64
	TxErrors         uint64
	// Blackholes counts connections whose datagram send the guard switched off.
	Blackholes uint64
	// Gate is whether the offer is switched on right now, SendGate whether
	// sending datagrams is.
	Gate     bool
	SendGate bool
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

		TxMessages:       layer.SentMessageCount,
		TxBytes:          layer.SentMessageByteCount,
		TxStreamMessages: layer.StreamSentMessageCount,
		TxErrors:         layer.SendErrorCount,
		Blackholes:       h3DatagramBlackholes.Load(),
		Gate:             H3DatagramsEnabled(),
		SendGate:         H3DatagramSendEnabled(),
	}
}

// HealthSuffix renders the DATAGRAM part of the [health] line, or "" until a
// connection has offered DATAGRAM, so a box that never enables it sees no new
// fields. accepted/offered says whether the server takes the offer at all.
func (self H3DatagramSnapshot) HealthSuffix() string {
	if self.Offered == 0 {
		return ""
	}
	out := fmt.Sprintf(" h3_dg=%d/%d dg_rx=%d dg_rx_drop=%d",
		self.Accepted, self.Offered, self.RxMessages, self.RxDrops)
	// the send part appears once anything was sent as a datagram, so a
	// receive-only box does not grow fields it cannot fill
	if self.TxMessages != 0 || self.TxErrors != 0 || self.Blackholes != 0 {
		out += fmt.Sprintf(" dg_tx=%d dg_tx_stream=%d dg_tx_err=%d dg_blackhole=%d",
			self.TxMessages, self.TxStreamMessages, self.TxErrors, self.Blackholes)
	}
	return out
}
