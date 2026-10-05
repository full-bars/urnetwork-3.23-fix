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
	// The process-wide H1 deliberately differs from the direct identity's, so the
	// share assertion below cannot be satisfied by the wrong denominator:
	// direct gives 10/(30+10) = 25 percent, process-wide would give 10/(70+10) = 12.
	got := transportStatsFromSnapshot(connect.TransportModeStatsSnapshot{
		H1FramesTx: 70, H1FramesRx: 80,
		H1BytesTx: 7000, H1BytesRx: 8000,
		H1DirectFramesTx: 30, H1DirectFramesRx: 40,
		H1DirectBytesTx: 3000, H1DirectBytesRx: 4000,
		H3FramesTx: 10, H3FramesRx: 20,
		H3BytesTx: 1000, H3BytesRx: 2000,
		H3Attempts: 4, H3Connects: 3, H3ConnectFailures: 1, H3Drops: 2, H3Up: 1,
	})
	if got.H1FramesTx != 70 || got.H1FramesRx != 80 || got.H1BytesTx != 7000 || got.H1BytesRx != 8000 {
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

	// A populated mode maps every field, so a copy/paste or dropped sub-field
	// cannot pass: the two modes carry different numbers on purpose.
	full := connect.PtModeSnapshot{
		FramesTx: 1, FramesRx: 2, BytesTx: 10, BytesRx: 20,
		Up: 1, Attempts: 3, Connects: 2, ConnectFailures: 1, Drops: 4,
	}
	withPump := transportStatsFromSnapshot(connect.TransportModeStatsSnapshot{
		Dns:     connect.PtModeSnapshot{Attempts: 2, Up: 1, ConnectFailures: 1},
		DnsPump: full,
	})
	if withPump.Dns == nil || withPump.Dns.Attempts != 2 || withPump.Dns.Up != 1 || withPump.Dns.ConnectFailures != 1 {
		t.Fatalf("dns block not carried: %+v", withPump.Dns)
	}
	if withPump.DnsPump == nil {
		t.Fatal("dns pump block not carried")
	}
	if mapped := *withPump.DnsPump; mapped != (NodePtModeStats{
		FramesTx: 1, FramesRx: 2, BytesTx: 10, BytesRx: 20,
		Up: 1, Attempts: 3, Connects: 2, ConnectFailures: 1, Drops: 4,
	}) {
		t.Fatalf("dns pump block mangled: %+v", mapped)
	}
	// Each block is independent: one mode running must not invent the other.
	if withPump.Dns.Drops != 0 || withPump.Dns.FramesTx != 0 {
		t.Fatalf("dns block took the pump's counters: %+v", withPump.Dns)
	}
}

// The reply carries a transport block as soon as any mode has run. H3 alone used
// to be the gate, which hid a node that runs only the DNS modes.
func TestTransportBlockIsOwedByAnyAttemptedMode(t *testing.T) {
	if transportBlockOwed(connect.TransportModeStatsSnapshot{}) {
		t.Fatal("nothing attempted: no transport block is owed")
	}
	if !transportBlockOwed(connect.TransportModeStatsSnapshot{H3Attempts: 1}) {
		t.Fatal("H3 attempted: the block is owed")
	}
	if !transportBlockOwed(connect.TransportModeStatsSnapshot{Dns: connect.PtModeSnapshot{Attempts: 1}}) {
		t.Fatal("DNS attempted without H3: the block is still owed")
	}
	if !transportBlockOwed(connect.TransportModeStatsSnapshot{DnsPump: connect.PtModeSnapshot{Attempts: 1}}) {
		t.Fatal("DNS pump attempted without H3: the block is still owed")
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
	// The per-mode blocks are mirrored field by field too, so a counter added to
	// PtModeSnapshot has to be added to NodePtModeStats with it.
	modeSnapshot := reflect.TypeOf(connect.PtModeSnapshot{})
	modeMapped := reflect.TypeOf(NodePtModeStats{})
	for i := 0; i < modeSnapshot.NumField(); i++ {
		name := modeSnapshot.Field(i).Name
		if _, ok := modeMapped.FieldByName(name); !ok {
			t.Errorf("connect.PtModeSnapshot.%s has no field on NodePtModeStats: it would silently drop out of the internals reply", name)
		}
	}
}
