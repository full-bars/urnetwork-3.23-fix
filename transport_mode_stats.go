package connect

import (
	"fmt"
	"sync/atomic"
)

// transportModeStats counts what each platform transport mode carried, so an
// operator can tell whether H3 moved any traffic beside H1 instead of guessing
// from "H3 connected". Frames are payload frames only: keepalive pings are
// empty and would otherwise make an idle H3 look busy.
type transportModeStats struct {
	framesTx atomic.Uint64
	framesRx atomic.Uint64
	bytesTx  atomic.Uint64
	bytesRx  atomic.Uint64
}

func (self *transportModeStats) addTx(byteCount int) {
	if 0 < byteCount {
		self.framesTx.Add(1)
		self.bytesTx.Add(uint64(byteCount))
	}
}

func (self *transportModeStats) addRx(byteCount int) {
	if 0 < byteCount {
		self.framesRx.Add(1)
		self.bytesRx.Add(uint64(byteCount))
	}
}

// h3FamilyCounters is what one mode that runs through runH3 carried, and its
// connection lifecycle. H3 and the two DNS packet-translation modes (WhoDis DNS
// and the DNS pump) all run through runH3, so they are counted separately by
// the mode they were started for instead of being folded into "h3".
type h3FamilyCounters struct {
	transportModeStats

	// a connect attempt, the outcome, and how many connections are up right now
	attempts        atomic.Uint64
	connects        atomic.Uint64
	connectFailures atomic.Uint64
	drops           atomic.Uint64
	up              atomic.Int64
}

var (
	// h1ModeStats counts every H1 transport in the process, which on a provider
	// is one per proxy identity. h1DirectStats counts only the identities that
	// are eligible for H3 (the direct identity), which is the fair thing to put
	// H3 against: H3 exists for that one identity, so comparing it with all the
	// proxies' H1 traffic reads as about 0% whatever H3 actually does.
	h1ModeStats   transportModeStats
	h1DirectStats transportModeStats

	h3Counters        h3FamilyCounters
	h3DnsCounters     h3FamilyCounters
	h3DnsPumpCounters h3FamilyCounters

	// h3ModeStats is the H3 frame and byte counters, kept for callers that count
	// only what was carried.
	h3ModeStats = &h3Counters.transportModeStats
)

// h3CountersFor returns the counters for the mode runH3 was started with.
func h3CountersFor(mode TransportMode) *h3FamilyCounters {
	switch mode {
	case TransportModeH3Dns:
		return &h3DnsCounters
	case TransportModeH3DnsPump:
		return &h3DnsPumpCounters
	default:
		return &h3Counters
	}
}

// TransportModeStatsSnapshot is a point-in-time copy of the per-mode counters.
type TransportModeStatsSnapshot struct {
	H1FramesTx, H1FramesRx, H1BytesTx, H1BytesRx uint64
	// H1Direct is H1 for the identities eligible for H3 only.
	H1DirectFramesTx, H1DirectFramesRx, H1DirectBytesTx, H1DirectBytesRx uint64
	H3FramesTx, H3FramesRx, H3BytesTx, H3BytesRx                         uint64

	H3Attempts        uint64
	H3Connects        uint64
	H3ConnectFailures uint64
	// H3Drops counts H3 connections that ended after they were up.
	H3Drops uint64
	// H3Up is the number of H3 connections up now.
	H3Up int64

	// Dns and DnsPump are the WhoDis DNS and DNS-pump packet-translation modes.
	// Auto mode does not start them, so they stay zero unless a transport is
	// built with one of those target modes.
	Dns     PtModeSnapshot
	DnsPump PtModeSnapshot
}

// PtModeSnapshot is what a packet-translation mode carried and its lifecycle.
type PtModeSnapshot struct {
	FramesTx, FramesRx, BytesTx, BytesRx uint64
	Up                                   int64
	Attempts, Connects, ConnectFailures  uint64
	Drops                                uint64
}

func ptSnapshot(c *h3FamilyCounters) PtModeSnapshot {
	return PtModeSnapshot{
		FramesTx:        c.framesTx.Load(),
		FramesRx:        c.framesRx.Load(),
		BytesTx:         c.bytesTx.Load(),
		BytesRx:         c.bytesRx.Load(),
		Up:              c.up.Load(),
		Attempts:        c.attempts.Load(),
		Connects:        c.connects.Load(),
		ConnectFailures: c.connectFailures.Load(),
		Drops:           c.drops.Load(),
	}
}

// TransportModeStats returns the cumulative per-mode counters.
func TransportModeStats() TransportModeStatsSnapshot {
	return TransportModeStatsSnapshot{
		H1FramesTx:        h1ModeStats.framesTx.Load(),
		H1FramesRx:        h1ModeStats.framesRx.Load(),
		H1BytesTx:         h1ModeStats.bytesTx.Load(),
		H1BytesRx:         h1ModeStats.bytesRx.Load(),
		H1DirectFramesTx:  h1DirectStats.framesTx.Load(),
		H1DirectFramesRx:  h1DirectStats.framesRx.Load(),
		H1DirectBytesTx:   h1DirectStats.bytesTx.Load(),
		H1DirectBytesRx:   h1DirectStats.bytesRx.Load(),
		H3FramesTx:        h3ModeStats.framesTx.Load(),
		H3FramesRx:        h3ModeStats.framesRx.Load(),
		H3BytesTx:         h3ModeStats.bytesTx.Load(),
		H3BytesRx:         h3ModeStats.bytesRx.Load(),
		H3Attempts:        h3Counters.attempts.Load(),
		H3Connects:        h3Counters.connects.Load(),
		H3ConnectFailures: h3Counters.connectFailures.Load(),
		H3Drops:           h3Counters.drops.Load(),
		H3Up:              h3Counters.up.Load(),
		Dns:               ptSnapshot(&h3DnsCounters),
		DnsPump:           ptSnapshot(&h3DnsPumpCounters),
	}
}

// H3TxSharePercent is the share of the direct identity's outbound payload frames
// carried by H3, or -1 when nothing has been sent on either mode. It is measured
// against the direct identity's H1 traffic, not every proxy's, because H3 runs
// for that identity alone.
func (self TransportModeStatsSnapshot) H3TxSharePercent() int {
	total := self.H1DirectFramesTx + self.H3FramesTx
	if total == 0 {
		return -1
	}
	return int(self.H3FramesTx * 100 / total)
}

// HealthSuffix renders the H3 part of the [health] line, or "" while H3 has
// never been attempted so a box without H3 sees no new fields. The DNS modes add
// their own fields only once one of them has been attempted; their keys carry
// the h3 prefix so a log scraper cannot conflate them with the dns_failures=
// figure the same line already reports for DoH resolution.
func (self TransportModeStatsSnapshot) HealthSuffix() string {
	out := ""
	if self.H3Attempts != 0 {
		share := "n/a"
		if pct := self.H3TxSharePercent(); 0 <= pct {
			share = fmt.Sprintf("%d%%", pct)
		}
		out = fmt.Sprintf(" h3_up=%d h3_tx_share=%s h3_drops=%d h3_conn_fail=%d",
			self.H3Up, share, self.H3Drops, self.H3ConnectFailures)
	}
	if self.Dns.Attempts != 0 {
		out += fmt.Sprintf(" h3dns_up=%d h3dns_conn_fail=%d", self.Dns.Up, self.Dns.ConnectFailures)
	}
	if self.DnsPump.Attempts != 0 {
		out += fmt.Sprintf(" h3dnspump_up=%d h3dnspump_conn_fail=%d", self.DnsPump.Up, self.DnsPump.ConnectFailures)
	}
	return out
}
