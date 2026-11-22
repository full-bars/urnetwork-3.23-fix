package connect

import (
	"fmt"
	"testing"
)

// The enforced platform/self rule is about direct peer-to-peer lanes. A
// platform lane legitimately names the platform, and any lane may carry this
// client's own id, so both are delivered there with nothing counted, in every
// binding mode and whether or not the lane carries a binding.
func TestForgedSourceAcceptedOnPlatformLanes(t *testing.T) {
	for _, mode := range []string{"off", "count", "drop"} {
		for _, bound := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/bound=%v", mode, bound), func(t *testing.T) {
				setSourcePolicy(t, mode, true)
				var boundSourceId Id
				if bound {
					boundSourceId = NewId()
				}
				client, route := newSourcePolicyClient(t, TransportTypeH1, boundSourceId, 4)
				delivered := deliverContent(client)

				route <- forgeBarrierFrame(t, ControlId, client.ClientId(), "platform")
				expectDelivery(t, delivered, "platform")
				route <- forgeBarrierFrame(t, client.ClientId(), client.ClientId(), "self")
				expectDelivery(t, delivered, "self")

				if stats := client.ReceiveStats(); stats.ForgedSourceDropCount != 0 ||
					stats.SourceMismatchCount != 0 {
					t.Fatalf("platform lane judged: %+v", stats)
				}
			})
		}
	}
}

// The rule does not need a binding: a direct peer-to-peer lane that names the
// platform or this client is forged whether or not it is bound to a peer.
func TestForgedSourceDroppedOnUnboundP2pLane(t *testing.T) {
	for _, mode := range []string{"off", "count", "drop"} {
		t.Run(mode, func(t *testing.T) {
			setSourcePolicy(t, mode, true)
			client, route := newSourcePolicyClient(t, TransportTypeP2p, Id{}, 4)
			delivered := deliverContent(client)

			route <- forgeBarrierFrame(t, ControlId, client.ClientId(), "platform")
			route <- forgeBarrierFrame(t, client.ClientId(), client.ClientId(), "self")

			waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
				return stats.ForgedSourceDropCount == 2
			}, "both forged sources dropped")
			expectNoDelivery(t, delivered)
			if stats := client.ReceiveStats(); stats.ForgedSourcePlatformDropCount != 1 ||
				stats.ForgedSourceSelfDropCount != 1 {
				t.Fatalf("drops not counted by kind: %+v", stats)
			}
		})
	}
}

// The two settings are separate: the kill switch turns off the platform/self
// drops, and the binding rule still judges a frame that is not the bound peer,
// so `p2p-source-binding drop` still drops it. The operator restores delivery
// with the binding setting, not with the kill switch.
func TestKillSwitchLeavesBoundLaneRuleInPlace(t *testing.T) {
	setSourcePolicy(t, "drop", false)
	peer := NewId()
	client, route := newSourcePolicyClient(t, TransportTypeP2p, peer, 4)
	delivered := deliverContent(client)

	route <- forgeBarrierFrame(t, ControlId, client.ClientId(), "platform")

	waitP2PSourceStats(t, client, func(stats ClientReceiveStatsSnapshot) bool {
		return stats.SourceMismatchDroppedCount == 1
	}, "bound lane rule still drops")
	expectNoDelivery(t, delivered)
	if stats := client.ReceiveStats(); stats.ForgedSourceDropCount != 0 {
		t.Fatalf("kill switch did not stop the forged drop count: %+v", stats)
	}
}
