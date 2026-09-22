package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestTcpSequenceRunReturnsPacketOnDialFailure guards against a leak where
// TcpSequence.Run, after allocating the SynAck packet (ip.go SynAck ->
// MessagePoolGet), leaked it when the upstream DialContext failed. Every
// failed upstream dial (dead/blocked/refused destination, common) removed one
// 2048-byte buffer from the pool permanently. Production telemetry on a
// v3.23.0-fix.32.1 node confirmed it at scale:
//
//	[health][pool][leak] tag=226 caller=ip.go:2775/ip.go:2069 leaked=672880
//	returned=32.36% — pool climbing ~100-140 buffers/hour.
//
// The fix returns the packet on the dial-failure branch only. It deliberately
// does NOT use `defer MessagePoolReturn(packet)` right after SynAck(): the
// success path's receive() already returns the buffer, so a defer would
// double-free on success (invisible to pool stats for an unshared buffer, but
// a live-reference decrement for a SharedReadOnly buffer).
//
// Deterministic: the dial is stubbed to always fail via
// TcpBufferSettings.DialContextSettings (net.go:291-293; ConnectSettings.
// DialContext prefers it, net.go:340). Run() exits through the dial-failure
// branch and its deferred close of sendItems is the completion signal.
func TestTcpSequenceRunReturnsPacketOnDialFailure(t *testing.T) {
	ResetMessagePoolStats()

	// Baseline BEFORE the exercising flow — the assertion compares the DELTA
	// this flow produced, catching both a leak (takenDelta > returnedDelta)
	// and a double-return (returnedDelta > takenDelta).
	baseTaken, baseReturned := waitForPoolBalance(t, 2048)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tcpBufferSettings := DefaultTcpBufferSettings()
	// ConnectTimeout defaults to 0 (the default in DefaultTcpBufferSettings),
	// which would make the pre-SYN `time.After(0)` select case fire instantly
	// and race the SYN delivery. Pin it high so only the sendItems case drives
	// the loop into SynAck.
	tcpBufferSettings.ConnectTimeout = 60 * time.Second
	tcpBufferSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return nil, errors.New("stub dial failure")
		},
	}

	seq := NewTcpSequence(
		ctx,
		func(TransferPath, protocol.ProvideMode, *IpPath, []byte) {},
		TransferPath{},
		protocol.ProvideMode_Network,
		4,
		net.ParseIP("127.0.0.1").To4(), TCPPort(40000),
		net.ParseIP("127.0.0.2").To4(), TCPPort(443),
		tcpBufferSettings,
	)

	done := make(chan struct{})
	go func() {
		seq.Run()
		close(done)
	}()

	// Drive the SYN that makes Run allocate the SynAck packet, then dial (and
	// fail). synPacket is freed by Run itself at the post-SYN MessagePoolReturn
	// (ip.go:2078) — it is not part of the leak under test.
	seq.sendItems <- &TcpSendItem{
		tcp:      &parsedTcp{syn: true},
		ipPacket: MessagePoolGet(2048),
	}

	// Run() can only exit via the dial-failure branch here (ctx not cancelled,
	// ConnectTimeout pinned high, SynAck cannot error). So done-close == the
	// dial-failure path was exercised.
	deadline := time.After(5 * time.Second)
	select {
	case <-done:
	case <-deadline:
		t.Fatal("TcpSequence.Run() did not exit within 5s — the dial-failure path was never reached")
	}

	taken, returned := waitForPoolBalance(t, 2048)
	takenDelta := taken - baseTaken
	returnedDelta := returned - baseReturned
	// Equality in BOTH directions: the SynAck packet is taken once and, with
	// the fix, returned exactly once on the dial-failure path. A leak
	// (takenDelta > returnedDelta) and a double-return (returnedDelta >
	// takenDelta) both corrupt the pool and must both fail.
	if takenDelta != returnedDelta {
		t.Fatalf("dial-failure path unbalanced the 2048 pool: taken_delta=%d returned_delta=%d (want equal; leak and double-return both fail here)", takenDelta, returnedDelta)
	}
}
