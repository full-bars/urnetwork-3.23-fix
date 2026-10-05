package main

import (
	"testing"

	"github.com/urnetwork/connect"
)

// The transport block is the counters read straight off, and the share is the
// snapshot's own computation. A box that has never attempted H3 reports no
// block at all, which is what keeps the panel unchanged for it.
func TestTransportStatsFromSnapshotCarriesTheCounters(t *testing.T) {
	got := transportStatsFromSnapshot(connect.TransportModeStatsSnapshot{
		H1FramesTx: 30, H1FramesRx: 40,
		H1BytesTx: 3000, H1BytesRx: 4000,
		H3FramesTx: 10, H3FramesRx: 20,
		H3BytesTx: 1000, H3BytesRx: 2000,
		H3Attempts: 4, H3Connects: 3, H3ConnectFailures: 1, H3Drops: 2, H3Up: 1,
	})
	if got.H1FramesTx != 30 || got.H1FramesRx != 40 || got.H1BytesTx != 3000 || got.H1BytesRx != 4000 {
		t.Fatalf("h1 counters not carried: %+v", got)
	}
	if got.H3FramesTx != 10 || got.H3FramesRx != 20 || got.H3BytesTx != 1000 || got.H3BytesRx != 2000 {
		t.Fatalf("h3 counters not carried: %+v", got)
	}
	if got.H3Attempts != 4 || got.H3Connects != 3 || got.H3ConnectFailures != 1 || got.H3Drops != 2 || got.H3Up != 1 {
		t.Fatalf("h3 state not carried: %+v", got)
	}
	if got.H3TxSharePercent != 25 {
		t.Fatalf("share = %d, want 25 (10 of 40 outbound frames)", got.H3TxSharePercent)
	}
}

// Nothing sent on either mode must read as "no share", not as 0%: the panel
// prints a placeholder rather than a measurement that was never taken.
func TestTransportStatsShareIsNegativeWhenNothingWasSent(t *testing.T) {
	got := transportStatsFromSnapshot(connect.TransportModeStatsSnapshot{H3Attempts: 2, H3ConnectFailures: 2})
	if got.H3TxSharePercent != -1 {
		t.Fatalf("share = %d, want -1 with no frames sent", got.H3TxSharePercent)
	}
}
