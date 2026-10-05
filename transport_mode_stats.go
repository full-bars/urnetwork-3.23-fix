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

var (
	h1ModeStats transportModeStats
	h3ModeStats transportModeStats

	// h3 lifecycle, process wide: a connect attempt, the outcome, and how many
	// H3 connections are up right now.
	h3Attempts        atomic.Uint64
	h3Connects        atomic.Uint64
	h3ConnectFailures atomic.Uint64
	h3Drops           atomic.Uint64
	h3Up              atomic.Int64
)

// TransportModeStatsSnapshot is a point-in-time copy of the per-mode counters.
type TransportModeStatsSnapshot struct {
	H1FramesTx, H1FramesRx, H1BytesTx, H1BytesRx uint64
	H3FramesTx, H3FramesRx, H3BytesTx, H3BytesRx uint64

	H3Attempts        uint64
	H3Connects        uint64
	H3ConnectFailures uint64
	// H3Drops counts H3 connections that ended after they were up.
	H3Drops uint64
	// H3Up is the number of H3 connections up now.
	H3Up int64
}

// TransportModeStats returns the cumulative per-mode counters.
func TransportModeStats() TransportModeStatsSnapshot {
	return TransportModeStatsSnapshot{
		H1FramesTx:        h1ModeStats.framesTx.Load(),
		H1FramesRx:        h1ModeStats.framesRx.Load(),
		H1BytesTx:         h1ModeStats.bytesTx.Load(),
		H1BytesRx:         h1ModeStats.bytesRx.Load(),
		H3FramesTx:        h3ModeStats.framesTx.Load(),
		H3FramesRx:        h3ModeStats.framesRx.Load(),
		H3BytesTx:         h3ModeStats.bytesTx.Load(),
		H3BytesRx:         h3ModeStats.bytesRx.Load(),
		H3Attempts:        h3Attempts.Load(),
		H3Connects:        h3Connects.Load(),
		H3ConnectFailures: h3ConnectFailures.Load(),
		H3Drops:           h3Drops.Load(),
		H3Up:              h3Up.Load(),
	}
}

// H3TxSharePercent is the share of outbound payload frames carried by H3, or -1
// when nothing has been sent on either mode.
func (self TransportModeStatsSnapshot) H3TxSharePercent() int {
	total := self.H1FramesTx + self.H3FramesTx
	if total == 0 {
		return -1
	}
	return int(self.H3FramesTx * 100 / total)
}

// HealthSuffix renders the H3 part of the [health] line, or "" while H3 has
// never been attempted so a box without H3 sees no new fields.
func (self TransportModeStatsSnapshot) HealthSuffix() string {
	if self.H3Attempts == 0 {
		return ""
	}
	share := "n/a"
	if pct := self.H3TxSharePercent(); 0 <= pct {
		share = fmt.Sprintf("%d%%", pct)
	}
	return fmt.Sprintf(" h3_up=%d h3_tx_share=%s h3_drops=%d h3_conn_fail=%d",
		self.H3Up, share, self.H3Drops, self.H3ConnectFailures)
}
