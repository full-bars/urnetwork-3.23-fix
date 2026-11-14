package connect

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

func TestMultiClientReceiveIPv6RoutesToFlowOwner(t *testing.T) {
	mc := newReceiveProofTestMultiClient()
	owner := &multiClientChannel{ctx: context.Background()}
	other := &multiClientChannel{ctx: context.Background()}
	path := dialFailureFlowPath(6)
	update := newMultiClientChannelUpdate(context.Background(), path)
	defer update.Close()
	update.client = owner
	registerDialFailureTestFlow(mc, owner, update, path)

	delivered := 0
	mc.receivePacketCallback = func(_ TransferPath, _ protocol.ProvideMode, got *IpPath, packet []byte) {
		delivered++
		if got.Version != 6 || got.ToIp6Path() != path.ToIp6Path() || string(packet) != "reply" {
			t.Fatalf("unexpected delivered path or packet: %v %q", got, packet)
		}
	}
	incoming := path.Reverse()
	incoming.Syn = false
	incoming.Ack = true
	mc.clientReceivePacket(other, TransferPath{}, protocol.ProvideMode_Network, incoming, []byte("wrong owner"))
	if delivered != 0 || update.receivedInbound.Load() {
		t.Fatal("packet from a different provider was accepted")
	}
	mc.clientReceivePacket(owner, TransferPath{}, protocol.ProvideMode_Network, incoming, []byte("reply"))
	if delivered != 1 || !update.receivedInbound.Load() {
		t.Fatalf("IPv6 reply not delivered and established: delivered=%d established=%v", delivered, update.receivedInbound.Load())
	}
}
