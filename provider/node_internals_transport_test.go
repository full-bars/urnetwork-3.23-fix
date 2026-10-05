package main

import (
	"reflect"
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
		H1DirectFramesTx: 30, H1DirectFramesRx: 40,
		H1DirectBytesTx: 3000, H1DirectBytesRx: 4000,
		H3FramesTx: 10, H3FramesRx: 20,
		H3BytesTx: 1000, H3BytesRx: 2000,
		H3Attempts: 4, H3Connects: 3, H3ConnectFailures: 1, H3Drops: 2, H3Up: 1,
	})
	if got.H1FramesTx != 30 || got.H1FramesRx != 40 || got.H1BytesTx != 3000 || got.H1BytesRx != 4000 {
		t.Fatalf("h1 counters not carried: %+v", got)
	}
	if got.H1DirectFramesTx != 30 || got.H1DirectFramesRx != 40 || got.H1DirectBytesTx != 3000 || got.H1DirectBytesRx != 4000 {
		t.Fatalf("h1 direct counters not carried: %+v", got)
	}
	if got.H3FramesTx != 10 || got.H3FramesRx != 20 || got.H3BytesTx != 1000 || got.H3BytesRx != 2000 {
		t.Fatalf("h3 counters not carried: %+v", got)
	}
	if got.H3Attempts != 4 || got.H3Connects != 3 || got.H3ConnectFailures != 1 || got.H3Drops != 2 || got.H3Up != 1 {
		t.Fatalf("h3 state not carried: %+v", got)
	}
	// The share is measured against the direct identity's H1, not every proxy's:
	// 10 of that identity's 40 outbound frames, not 10 of the process total.
	if got.H3TxSharePercent != 25 {
		t.Fatalf("share = %d, want 25 (10 of the direct identity's 40 outbound frames)", got.H3TxSharePercent)
	}
	// The DNS blocks stay absent until their mode has been attempted, like the
	// H3 block on a box that never enables it.
	if got.Dns != nil || got.DnsPump != nil {
		t.Fatalf("dns blocks must be absent when untouched: %+v %+v", got.Dns, got.DnsPump)
	}

	withDns := transportStatsFromSnapshot(connect.TransportModeStatsSnapshot{
		Dns: connect.PtModeSnapshot{Attempts: 2, Up: 1, ConnectFailures: 1},
	})
	if withDns.Dns == nil || withDns.Dns.Attempts != 2 || withDns.Dns.Up != 1 || withDns.Dns.ConnectFailures != 1 {
		t.Fatalf("dns block not carried: %+v", withDns.Dns)
	}
	if withDns.DnsPump != nil {
		t.Fatalf("dns pump must stay absent when untouched: %+v", withDns.DnsPump)
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

// The mapping is written field by field, so a counter added in connect would be
// dropped here (and then dropped again in the tool's mirror struct) with nothing
// failing. This pins it.
func TestTransportStatsMappingCoversEverySnapshotCounter(t *testing.T) {
	// Derived from the frame counters rather than copied, so the tool cannot
	// recompute it and disagree with the provider.
	derived := map[string]bool{"H3TxSharePercent": true}
	snapshot := reflect.TypeOf(connect.TransportModeStatsSnapshot{})
	mapped := reflect.TypeOf(NodeTransportStats{})
	for i := 0; i < snapshot.NumField(); i++ {
		name := snapshot.Field(i).Name
		if derived[name] {
			continue
		}
		if _, ok := mapped.FieldByName(name); !ok {
			t.Errorf("connect.TransportModeStatsSnapshot.%s has no field on NodeTransportStats: it would silently drop out of the internals reply", name)
		}
	}
}
