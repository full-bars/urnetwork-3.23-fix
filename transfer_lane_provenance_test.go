package connect

import (
	"testing"
	"time"
)

// newLaneProvenanceTestSequence builds inert in-flight state for the
// per-route acknowledgement table. No goroutine and no wall clock takes part,
// so every assertion below is deterministic.
func newLaneProvenanceTestSequence(
	t *testing.T,
	settings *SendBufferSettings,
	itemCount int,
	sendTime time.Time,
) (*SendSequence, []*sendItem) {
	t.Helper()
	sequence, items := newSelectiveAckRecoveryTestSequence(itemCount, sendTime)
	sequence.sendBufferSettings = settings
	return sequence, items
}

// The default construction must not count anything by route. Provenance is
// still recorded, because it costs two assignments and is what a later
// measurement reads, but no slot is assigned and no route is compared.
func TestLaneProvenanceOffAssignsNoRouteSlot(t *testing.T) {
	settings := DefaultSendBufferSettings()
	if settings.ReliableLaneProvenRecovery {
		t.Fatalf("ReliableLaneProvenRecovery default = true, want false")
	}
	sequence, items := newLaneProvenanceTestSequence(t, settings, 2, time.Now())
	reliableRoute := make(Route, 1)
	unreliableRoute := make(Route, 1)

	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeH1,
		reliable:      true,
		route:         reliableRoute,
	})
	if items[0].carrierRoute != reliableRoute {
		t.Fatalf("carrierRoute = %v, want the reliable route", items[0].carrierRoute)
	}
	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeP2p,
		unreliable:    true,
		route:         unreliableRoute,
	})
	if items[0].carrierRoute != unreliableRoute || !items[0].carrierChanged {
		t.Fatalf("carrier after a lane move = %v changed=%t, want the unreliable route and changed",
			items[0].carrierRoute, items[0].carrierChanged)
	}

	if slot := sequence.laneSlotFor(reliableRoute); slot != -1 {
		t.Fatalf("laneSlotFor with the setting off = %d, want -1", slot)
	}
	if slot := sequence.laneSlotFor(unreliableRoute); slot != -1 {
		t.Fatalf("laneSlotFor with the setting off = %d, want -1", slot)
	}
	for index := range sequence.laneAcks {
		if sequence.laneAcks[index].set {
			t.Fatalf("lane slot %d was assigned with the setting off: %+v", index, sequence.laneAcks[index])
		}
	}
	if _, ok := sequence.laneHighestSent(reliableRoute); ok {
		t.Fatal("laneHighestSent answered with the setting off")
	}
	if sequence.laneProvenRecovery(items[1]) {
		t.Fatal("laneProvenRecovery answered with the setting off")
	}
	if sequence.laneLoneTail(items[1]) {
		t.Fatal("laneLoneTail answered with the setting off")
	}
}

// With the setting off, a hole on the reliable lane is still judged by the
// merged count of later selective acknowledgements from any lane.
func TestLaneProvenRecoveryOffUsesMergedAckCount(t *testing.T) {
	settings := DefaultSendBufferSettings()
	sendTime := time.Now()
	sequence, items := newLaneProvenanceTestSequence(t, settings, 6, sendTime)
	reliableRoute := make(Route, 1)
	unreliableRoute := make(Route, 1)

	// item 0 is the hole, carried by the reliable lane
	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeH1,
		reliable:      true,
		route:         reliableRoute,
	})
	// three later items were carried by the direct lane and acknowledged
	for _, item := range items[1:4] {
		sequence.observeCarrierWrite(item, transferWriteDisposition{
			transportType: TransportTypeP2p,
			unreliable:    true,
			route:         unreliableRoute,
		})
		item.selectiveAcked = true
	}

	sequence.scheduleSelectiveAckRecovery(sendTime.Add(time.Millisecond))
	if items[0].recoveryKind != sendRecoverySelectiveGap {
		t.Fatalf("hole recovery kind = %v, want %v: the merged count proves the hole",
			items[0].recoveryKind, sendRecoverySelectiveGap)
	}
	if items[0].ackTailProbeCount != 0 {
		t.Fatalf("hole ack tail probes = %d, want 0 with the setting off", items[0].ackTailProbeCount)
	}
}

// With the setting on, the same three acknowledgements arrive on the direct
// lane and prove nothing about the reliable lane's hole, which is that lane's
// only outstanding item and is therefore probed. Three acknowledgements on
// the hole's own route still schedule the gap resend, so the count is per
// route rather than simply disabled.
func TestLaneProvenRecoveryCountsAcksPerCarrierRoute(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.ReliableLaneProvenRecovery = true
	sendTime := time.Now()
	reliableRoute := make(Route, 1)
	unreliableRoute := make(Route, 1)

	// another lane's acknowledgements: the hole is probed, not gap-resent
	sequence, items := newLaneProvenanceTestSequence(t, settings, 6, sendTime)
	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeH1,
		reliable:      true,
		route:         reliableRoute,
	})
	for _, item := range items[1:4] {
		sequence.observeCarrierWrite(item, transferWriteDisposition{
			transportType: TransportTypeP2p,
			unreliable:    true,
			route:         unreliableRoute,
		})
		item.selectiveAcked = true
	}
	if !sequence.laneProvenRecovery(items[0]) {
		t.Fatal("a reliable-carried hole that stayed on its route was not read by lane")
	}
	if !sequence.laneLoneTail(items[0]) {
		t.Fatal("the reliable lane's only outstanding item was not its lone tail")
	}
	reliableSlot, unreliableSlot := sequence.laneSlotFor(reliableRoute), sequence.laneSlotFor(unreliableRoute)
	if reliableSlot < 0 || unreliableSlot < 0 || reliableSlot == unreliableSlot {
		t.Fatalf("slots = %d/%d, want two assigned and distinct", reliableSlot, unreliableSlot)
	}
	if highest, ok := sequence.laneHighestSent(unreliableRoute); !ok || highest != items[3].sequenceNumber {
		t.Fatalf("highest sent on the direct lane = %d/%t, want %d", highest, ok, items[3].sequenceNumber)
	}
	sequence.scheduleSelectiveAckRecovery(sendTime.Add(time.Millisecond))
	if items[0].recoveryKind != sendRecoveryAckTailProbe || items[0].ackTailProbeCount != 1 {
		t.Fatalf("hole after the other lane's acknowledgements = %v count=%d, want one probe on its own lane",
			items[0].recoveryKind, items[0].ackTailProbeCount)
	}

	// its own lane's acknowledgements: the gap resend is scheduled as before
	sequence, items = newLaneProvenanceTestSequence(t, settings, 6, sendTime)
	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeH1,
		reliable:      true,
		route:         reliableRoute,
	})
	for _, item := range items[1:4] {
		sequence.observeCarrierWrite(item, transferWriteDisposition{
			transportType: TransportTypeH1,
			reliable:      true,
			route:         reliableRoute,
		})
		item.selectiveAcked = true
	}
	if sequence.laneLoneTail(items[0]) {
		t.Fatal("a hole with later acknowledgements on its own route was still its lane's tail")
	}
	sequence.scheduleSelectiveAckRecovery(sendTime.Add(time.Millisecond))
	if items[0].recoveryKind != sendRecoverySelectiveGap {
		t.Fatalf("hole recovery kind = %v, want %v: its own lane proved it",
			items[0].recoveryKind, sendRecoverySelectiveGap)
	}
	if items[0].ackTailProbeCount != 0 {
		t.Fatalf("hole ack tail probes = %d, want 0 when its own lane proved it", items[0].ackTailProbeCount)
	}
}

// An item that has been written on two routes no longer speaks for either:
// whichever copy is acknowledged first decides it, so it holds no lane's
// tail and never proves recovery by lane.
func TestLaneProvenRecoveryExcludesChangedCarrier(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.ReliableLaneProvenRecovery = true
	sequence, items := newLaneProvenanceTestSequence(t, settings, 2, time.Now())
	reliableRoute := make(Route, 1)
	otherReliableRoute := make(Route, 1)

	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeH1,
		reliable:      true,
		route:         reliableRoute,
	})
	if !sequence.laneProvenRecovery(items[0]) {
		t.Fatal("an item that stayed on one reliable route was not read by lane")
	}
	sequence.observeCarrierWrite(items[0], transferWriteDisposition{
		transportType: TransportTypeH1,
		reliable:      true,
		route:         otherReliableRoute,
	})
	if !items[0].carrierChanged {
		t.Fatal("carrierChanged was not set by a write on another route")
	}
	if sequence.laneProvenRecovery(items[0]) {
		t.Fatal("a changed-carrier item was still read by lane")
	}
	if sequence.laneOldestOutstanding(reliableRoute) == items[0] {
		t.Fatal("a changed-carrier item still held its old lane's head")
	}
	if sequence.laneLoneTail(items[0]) {
		t.Fatal("a changed-carrier item was still a lane tail")
	}
}

// The reliable-lane reading is re-aimed at the fork's existing
// unreliableFlightTracked rather than a new latch, so the flight's own answer
// decides it.
func TestLaneProvenRecoveryDefersToUnreliableFlightTracking(t *testing.T) {
	settings := DefaultSendBufferSettings()
	settings.ReliableLaneProvenRecovery = true
	sequence, items := newLaneProvenanceTestSequence(t, settings, 1, time.Now())
	reliableRoute := make(Route, 1)

	items[0].carrierRoute = reliableRoute
	items[0].reliableCarrierObserved = true
	if !sequence.laneProvenRecovery(items[0]) {
		t.Fatal("a reliable-carried item on one route was not read by lane")
	}
	items[0].unreliableFlightTracked = true
	if sequence.laneProvenRecovery(items[0]) {
		t.Fatal("an item tracked in the unreliable flight was still read by lane")
	}
	items[0].unreliableFlightTracked = false
	items[0].carrierChanged = true
	if sequence.laneProvenRecovery(items[0]) {
		t.Fatal("a changed-carrier item was still read by lane")
	}
}
