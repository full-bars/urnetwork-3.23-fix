package connect

import (
	"context"
	"net"
	"testing"

	"github.com/urnetwork/connect/protocol"
)

// TestUdp4BufferSendReturnsPacketOnBackpressureDrop guards against a leak
// where UdpBuffer.udpSend (called from LocalUserNat.runShard for every
// outbound UDP packet) discarded the (bool, error) result of
// UdpSequence.send without freeing the pooled ipPacket buffer on the drop
// paths (idle-close, full sequence buffer, write timeout). Every dropped
// packet under sustained backpressure permanently removed one buffer from
// the pool.
//
// To make the drop deterministic (not a timing race against the sequence's
// own write-pipeline goroutine), this pre-seeds the buffer's private
// sequence map with a UdpSequence whose Run() is never started — nothing
// ever drains its single-slot sendItems channel, so a subsequent
// non-blocking send is guaranteed to observe it full.
func TestUdp4BufferSendReturnsPacketOnBackpressureDrop(t *testing.T) {
	ResetMessagePoolStats()

	// Baseline captured BEFORE the exercising code: the assertion compares
	// the DELTA the flow itself produced, so a permanent leak (buffers taken
	// and never returned) shows up as takenDelta > returnedDelta. Capturing
	// the baseline after the flow would make the assertion tautological.
	baseTaken, baseReturned := waitForPoolBalance(t, 2048)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	udpBufferSettings := DefaultUdpBufferSettings()
	udpBufferSettings.SequenceBufferSize = 1

	udp4Buffer := NewUdp4Buffer(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, udpBufferSettings, nil)

	udp := &parsedUdp{
		sourceIp:        net.ParseIP("127.0.0.1").To4(),
		destinationIp:   net.ParseIP("127.0.0.2").To4(),
		sourcePort:      UDPPort(40000),
		destinationPort: UDPPort(443),
		payload:         []byte("x"),
	}

	seq := NewUdpSequence(
		ctx,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		TransferPath{},
		protocol.ProvideMode_Network,
		4,
		udp.sourceIp, udp.sourcePort,
		udp.destinationIp, udp.destinationPort,
		udpBufferSettings,
	)
	// Deliberately never call seq.Run() — nothing will drain sendItems, so
	// the buffer stays full until we clean it up below.
	bufferId := NewBufferId4(TransferPath{}, udp.sourceIp, int(udp.sourcePort), udp.destinationIp, int(udp.destinationPort))
	udp4Buffer.sequences[bufferId] = seq

	seedPacket := MessagePoolGet(2048)
	seq.sendItems <- &UdpSendItem{udp: udp, ipPacket: seedPacket}

	packet := MessagePoolGet(2048)
	success, err := udp4Buffer.send(TransferPath{}, protocol.ProvideMode_Network, udp, 0, packet)
	if err != nil {
		t.Fatalf("send: unexpected error %v", err)
	}
	if success {
		t.Fatal("expected the send to be dropped (sequence buffer deterministically full), got success=true")
	}

	// Clean up the filler item manually queued above — it was never drained
	// since seq.Run() was never started, so it isn't part of what this test
	// is verifying (the drop path's own cleanup).
	MessagePoolReturn(seedPacket)

	taken, returned := waitForPoolBalance(t, 2048)
	takenDelta := taken - baseTaken
	returnedDelta := returned - baseReturned
	// Equality in BOTH directions: a leak (takenDelta > returnedDelta) and a
	// double-return (returnedDelta > takenDelta) both corrupt the pool and
	// must fail the test.
	if takenDelta != returnedDelta {
		t.Fatalf("backpressure drop unbalanced the pool: taken_delta=%d returned_delta=%d (want equal; leak and double-return both fail here)", takenDelta, returnedDelta)
	}
}

// TestTcp4BufferSendNonSynDropDoesNotDoubleReturn documents the correct
// ownership on the non-SYN-drop path: TcpBuffer.tcpSend's initSequence
// closure returns ipPacket to the pool itself (see the "!tcp.syn" branch)
// before returning nil, so the tcpSend call site must not free it again.
//
// An earlier revision of the fix in this file added a second
// MessagePoolReturn at the call site for that same nil result. Note this
// test cannot actually distinguish that buggy revision from the fix here:
// MessagePoolReturn's own refcount guard makes a same-buffer double call a
// silent no-op for an unshared buffer (both versions leave the pool stats
// identical). The real risk that motivated removing the extra call is a
// packet shared elsewhere (refcount > 1 via MessagePoolShareReadOnly),
// where a redundant return would decrement a live reference held by
// another owner — a scenario this synchronous, unshared-packet test does
// not reach. What is verified here is the simpler invariant: after
// tcpSend returns, the packet must already be fully returned exactly once.
func TestTcp4BufferSendNonSynDropDoesNotDoubleReturn(t *testing.T) {
	ResetMessagePoolStats()

	// Baseline BEFORE the exercising flow — see the sibling test: the delta
	// comparison must measure what THIS flow did, not what arrived in the
	// window already (which would make the assertion vacuous).
	baseTaken, baseReturned := waitForPoolBalance(t, 2048)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tcpBufferSettings := DefaultTcpBufferSettings()
	tcp4Buffer := NewTcp4Buffer(ctx, func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {}, tcpBufferSettings, nil)

	// No SYN and no existing sequence for this bufferId: initSequence's
	// "!tcp.syn" branch drops the packet, freeing ipPacket exactly once.
	tcp := &parsedTcp{
		sourceIp:        net.ParseIP("127.0.0.1").To4(),
		destinationIp:   net.ParseIP("127.0.0.2").To4(),
		sourcePort:      TCPPort(40000),
		destinationPort: TCPPort(443),
		syn:             false,
		ack:             true,
	}

	packet := MessagePoolGet(2048)
	success, err := tcp4Buffer.send(TransferPath{}, protocol.ProvideMode_Network, tcp, 0, packet)
	if err != nil {
		t.Fatalf("send: unexpected error %v", err)
	}
	if success {
		t.Fatal("expected the non-SYN packet to be dropped, got success=true")
	}

	// tcpSend must have fully returned the packet exactly once already; a
	// further manual return must be rejected (see MessagePoolReturn's
	// count == 0 guard), not silently accepted.
	if MessagePoolReturn(packet) {
		t.Fatal("packet was still returnable after tcpSend — it was not returned as part of the non-SYN drop")
	}

	taken, returned := waitForPoolBalance(t, 2048)
	takenDelta := taken - baseTaken
	returnedDelta := returned - baseReturned
	// Equality, not just leak-direction: a double-return makes returnedDelta
	// exceed takenDelta and must fail too (the packet is acquired once and
	// freed exactly once by tcpSend).
	if takenDelta != returnedDelta {
		t.Fatalf("non-SYN drop unbalanced the pool: taken_delta=%d returned_delta=%d (want equal; leak and double-return both fail here)", takenDelta, returnedDelta)
	}
}
