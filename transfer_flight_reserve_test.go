package connect

import (
	"testing"
)

// limitedFlightController returns a controller under an active flight policy
// with the flow reserve enabled, driven to its byte limit so the next send for
// a fresh scheduling key must take the reserve slot.
func limitedFlightController(t *testing.T) (*sendFlightController, sendSchedulingKey) {
	t.Helper()
	settings := DefaultSendBufferSettings()
	controller := newSendFlightController(settings)
	applied := controller.applyPolicy(transferFlightPolicySnapshot{
		generation:  1,
		limited:     true,
		flowReserve: true,
	})
	if !applied {
		t.Fatal("expected the flight policy to apply")
	}
	if !controller.limited {
		t.Fatal("expected the controller to be limited under the policy")
	}
	key := sendSchedulingKey{valid: true}
	// Fill to the byte limit on the unkeyed path so the reserve is the only
	// remaining admission route for a keyed send.
	for controller.byteCount < controller.byteLimit {
		controller.send(controller.byteLimit)
	}
	if controller.canSend() {
		t.Fatal("expected the unkeyed path to be at the limit")
	}
	return controller, key
}

// A zero-byte acknowledgement must still hand the reserve slot back. The byte
// guard in acknowledgeForKey used to return before the release, stranding the
// slot for the life of the controller.
func TestFlightControllerReleasesReserveOnZeroByteAck(t *testing.T) {
	controller, key := limitedFlightController(t)

	reserved := controller.sendForKey(0, key)
	if !reserved {
		t.Fatal("expected the reserve to be granted at the limit for a fresh key")
	}
	if !controller.flowReserveInUse {
		t.Fatal("expected the reserve to be marked in use after the grant")
	}

	controller.acknowledgeForKey(0, key, reserved)

	if controller.flowReserveInUse {
		t.Fatal("reserve slot was not released by a zero-byte acknowledgement")
	}
	if !controller.canSendForKey(key) {
		t.Fatal("expected a later keyed send to be admitted once the reserve is free")
	}
	if regranted := controller.sendForKey(0, key); !regranted {
		t.Fatal("expected the reserve to be grantable again after release")
	}
}

// The grant in sendForKey sits outside the byte guard, so a zero-byte send
// still claims the slot. This pins that asymmetry so the release side is not
// "fixed" back to sitting behind the guard.
func TestFlightControllerGrantsReserveOnZeroByteSend(t *testing.T) {
	controller, key := limitedFlightController(t)

	byteCountBefore := controller.byteCount
	messageCountBefore := controller.messageCount

	if reserved := controller.sendForKey(0, key); !reserved {
		t.Fatal("expected a zero-byte send to claim the reserve slot")
	}
	if controller.byteCount != byteCountBefore {
		t.Errorf(
			"zero-byte send changed byteCount: before %d, after %d",
			byteCountBefore,
			controller.byteCount,
		)
	}
	if controller.messageCount != messageCountBefore {
		t.Errorf(
			"zero-byte send changed messageCount: before %d, after %d",
			messageCountBefore,
			controller.messageCount,
		)
	}
}

// The ordinary path: a reserve taken by a real send is released by the
// matching acknowledgement, and byte accounting is unaffected by the release.
func TestFlightControllerReleasesReserveOnNormalAck(t *testing.T) {
	controller, key := limitedFlightController(t)

	byteCount := ByteCount(1024)
	reserved := controller.sendForKey(byteCount, key)
	if !reserved {
		t.Fatal("expected the reserve to be granted at the limit for a fresh key")
	}
	inFlightByteCount := controller.byteCount

	controller.acknowledgeForKey(byteCount, key, reserved)

	if controller.flowReserveInUse {
		t.Fatal("reserve slot was not released by a normal acknowledgement")
	}
	if controller.byteCount != inFlightByteCount-byteCount {
		t.Errorf(
			"expected %d bytes in flight after ack, got %d",
			inFlightByteCount-byteCount,
			controller.byteCount,
		)
	}
	if 0 < controller.messageCountByKey[key] {
		t.Errorf(
			"expected the keyed message count to drain, got %d",
			controller.messageCountByKey[key],
		)
	}
}

// Only one reserve slot exists, and a key that already holds an in-flight
// message is refused.
//
// Note the limit of what this can assert: sendSchedulingKey carries a single
// `valid` bool, so every valid key compares equal and there is no second,
// distinct key to contend with. What this pins is the single-occupancy flag
// and the per-key in-flight guard, not cross-flow fairness, which the key type
// cannot currently express.
func TestFlightControllerReserveIsSingleOccupancy(t *testing.T) {
	controller, key := limitedFlightController(t)

	if reserved := controller.sendForKey(512, key); !reserved {
		t.Fatal("expected the first keyed send to take the reserve")
	}
	if reserved := controller.sendForKey(512, key); reserved {
		t.Fatal("expected the reserve to be refused while already in use")
	}
	if controller.canSendForKey(key) {
		t.Fatal("expected a key holding in-flight messages to be refused the reserve")
	}
}

// An acknowledgement that never held a reserve must not free somebody else's
// slot.
func TestFlightControllerUnreservedAckLeavesReserveHeld(t *testing.T) {
	controller, key := limitedFlightController(t)

	if reserved := controller.sendForKey(512, key); !reserved {
		t.Fatal("expected the keyed send to take the reserve")
	}

	// Go through acknowledgeForKey with reserved=false rather than the unkeyed
	// acknowledge helper: the release lives in acknowledgeForKey, so a
	// regression that cleared the slot regardless of the flag would not show up
	// through a wrapper that never passes one.
	controller.acknowledgeForKey(256, key, false)

	if !controller.flowReserveInUse {
		t.Fatal("an unreserved acknowledgement released the reserve slot")
	}

	// The unkeyed wrapper must behave the same way.
	controller.acknowledge(256)
	if !controller.flowReserveInUse {
		t.Fatal("the unkeyed acknowledge helper released the reserve slot")
	}
}

// With the reserve disabled by policy, no send may claim a slot regardless of
// byte count.
func TestFlightControllerNoReserveWhenPolicyDisablesIt(t *testing.T) {
	controller := newSendFlightController(DefaultSendBufferSettings())
	controller.applyPolicy(transferFlightPolicySnapshot{
		generation:  1,
		limited:     true,
		flowReserve: false,
	})
	key := sendSchedulingKey{valid: true}
	for controller.byteCount < controller.byteLimit {
		controller.send(controller.byteLimit)
	}

	if reserved := controller.sendForKey(0, key); reserved {
		t.Fatal("expected no reserve grant while the policy disables the reserve")
	}
	if controller.flowReserveInUse {
		t.Fatal("expected the reserve to stay free while the policy disables it")
	}
}
