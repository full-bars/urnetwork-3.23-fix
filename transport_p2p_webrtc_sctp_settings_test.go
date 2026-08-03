package connect

import (
	"context"
	"io"
	mathrand "math/rand"
	"net"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/pion/webrtc/v4"
)

func TestDefaultWebRtcSettingsSctpTuning(t *testing.T) {
	settings := DefaultWebRtcSettings()

	// ReceiveMtu should match Pion's SCTP path MTU (Ethernet, 1500 bytes),
	// not the old 4 KiB value that only inflated demux buffers with no
	// effect on the actual SCTP window.
	assert.Equal(t, ByteCount(1500), settings.ReceiveMtu)

	// Congestion-avoidance step should be set (four MTUs) rather than left
	// at Pion's single-MTU-per-window default.
	assert.Equal(t, uint32(4*1200), settings.SctpCwndCAStep)

	// The progress watchdog should be enabled by default.
	assert.Equal(t, true, 0 < settings.SctpNoProgressTimeout)
}

func TestSctpProgressWatchdogDisabledWhenTimeoutZero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.SctpNoProgressTimeout = 0

	conn := &peerConn{
		ctx:              ctx,
		cancel:           cancel,
		log:              loggerOrDefault(nil),
		settings:         settings,
		outboundProgress: make(chan struct{}, 1),
	}

	// With the timeout disabled, noting outbound activity must not start the
	// watchdog goroutine or ever call cancel() on its own.
	conn.noteOutboundSctpActivity()

	select {
	case <-ctx.Done():
		t.Fatalf("watchdog fired despite SctpNoProgressTimeout=0")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWebRtcWithSctpProgressWatchdogEnabledPassesRealTraffic(t *testing.T) {
	// Regression guard: a short-but-nonzero SctpNoProgressTimeout must not
	// tear down a healthy, actively-exchanging connection. The watchdog
	// should only fire on genuine no-progress, which real bidirectional
	// traffic never triggers (every write's SACK counts as progress).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	settingsA := DefaultWebRtcSettings()
	settingsA.SctpNoProgressTimeout = 5 * time.Second
	settingsB := DefaultWebRtcSettings()
	settingsB.SctpNoProgressTimeout = 5 * time.Second

	signalPipeA := newSignalPipe(nil)
	signalPipeB := newSignalPipe(nil)

	webRtcManagerA := NewWebRtcManager(ctx, signalPipeA, settingsA)
	webRtcManagerB := NewWebRtcManager(ctx, signalPipeB, settingsB)

	signalPipeA.signalReceiver = webRtcManagerB
	signalPipeB.signalReceiver = webRtcManagerA

	peerIdA := NewId()
	peerIdB := NewId()
	streamId := NewId()

	connA, err := webRtcManagerA.NewP2pConnActive(ctx, NewTransferPath(peerIdA, peerIdB, streamId))
	assert.Equal(t, err, nil)
	defer connA.Close()

	connB, err := webRtcManagerB.NewP2pConnPassive(ctx, NewTransferPath(peerIdB, peerIdA, streamId))
	assert.Equal(t, err, nil)
	defer connB.Close()

	b := make([]byte, 64*1024)
	mathrand.Read(b)

	received := make(chan []byte, 1)
	sendErr := make(chan error, 1)
	receiveErr := make(chan error, 1)

	send := func(conn net.Conn) {
		if _, err := conn.Write(b); err != nil {
			sendErr <- err
			return
		}
		sendErr <- nil
	}
	receive := func(conn net.Conn) {
		b2 := make([]byte, len(b))
		if _, err := io.ReadFull(conn, b2); err != nil {
			receiveErr <- err
			return
		}
		receiveErr <- nil
		received <- b2
	}

	go send(connA)
	go receive(connB)

	select {
	case b2 := <-received:
		assert.Equal(t, b, b2)
	case err := <-sendErr:
		if err != nil {
			t.Fatalf("send error: %s", err)
		}
	case err := <-receiveErr:
		if err != nil {
			t.Fatalf("receive error: %s", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for data; watchdog may have torn down a healthy connection")
	}
}

func TestWebRtcSctpProgressNilPeerConnection(t *testing.T) {
	// A nil *webrtc.PeerConnection must be reported as "no signal", never
	// dereferenced.
	bufferedAmount, bytesReceived, ok := webRtcSctpProgress(nil)
	assert.Equal(t, ok, false)
	assert.Equal(t, bufferedAmount, 0)
	assert.Equal(t, bytesReceived, uint64(0))
}

func TestWebRtcSctpProgressFreshPeerConnection(t *testing.T) {
	// NewPeerConnection always allocates a non-nil SCTPTransport, but no
	// association exists until negotiation completes, so stats report zero
	// rather than being unavailable.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	assert.Equal(t, err, nil)
	defer pc.Close()

	bufferedAmount, bytesReceived, ok := webRtcSctpProgress(pc)
	assert.Equal(t, ok, true)
	assert.Equal(t, bufferedAmount, 0)
	assert.Equal(t, bytesReceived, uint64(0))
}

func TestCreateWebRtcPeerConnectionAppliesSctpCwndCAStep(t *testing.T) {
	// SetSCTPCwndCAStep is only wired up when the setting is non-zero
	// (`0 < settings.SctpCwndCAStep`); both branches must still produce a
	// usable PeerConnection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, cwndCAStep := range []uint32{0, 4 * 1200} {
		settings := DefaultWebRtcSettings()
		settings.SctpCwndCAStep = cwndCAStep

		pc, err := createWebRtcPeerConnection(ctx, true, settings)
		assert.Equal(t, err, nil)
		if pc == nil {
			t.Fatalf("createWebRtcPeerConnection(cwndCAStep=%d) returned a nil PeerConnection", cwndCAStep)
		}
		pc.Close()
	}
}

func TestSctpProgressWatchdogNoCancelWhenBufferedAmountZero(t *testing.T) {
	// The watchdog only starts sampling once BufferedAmount is non-zero. On
	// a PeerConnection whose SCTP association never forms, BufferedAmount
	// stays at 0, so noting outbound activity must not cancel the
	// connection even after the no-progress timeout elapses.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	assert.Equal(t, err, nil)
	defer pc.Close()

	settings := DefaultWebRtcSettings()
	settings.SctpNoProgressTimeout = 50 * time.Millisecond

	conn := &peerConn{
		ctx:              ctx,
		cancel:           cancel,
		log:              loggerOrDefault(nil),
		settings:         settings,
		pc:               pc,
		outboundProgress: make(chan struct{}, 1),
	}

	conn.noteOutboundSctpActivity()

	select {
	case <-ctx.Done():
		t.Fatalf("watchdog canceled the connection despite zero buffered amount")
	case <-time.After(5 * settings.SctpNoProgressTimeout):
	}
}

func TestNoteOutboundSctpActivityCoalescesWithoutBlocking(t *testing.T) {
	// outboundProgress is a capacity-1 coalescing channel: repeated signals
	// before the watchdog drains it must never block the writer (Write
	// itself calls noteOutboundSctpActivity synchronously).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultWebRtcSettings()
	settings.SctpNoProgressTimeout = 0 // keep the watchdog goroutine from starting/draining

	conn := &peerConn{
		ctx:              ctx,
		cancel:           cancel,
		log:              loggerOrDefault(nil),
		settings:         settings,
		outboundProgress: make(chan struct{}, 1),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i += 1 {
			conn.noteOutboundSctpActivity()
		}
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("noteOutboundSctpActivity blocked despite a disabled (undrained) watchdog")
	}
}

// Note: a scenario where the SCTP association genuinely stalls while ICE
// consent remains healthy (e.g. one-way packet loss to a still-live peer) is
// intentionally not simulated here. Locally closing a peer's
// *webrtc.PeerConnection immediately errors and aborts the SCTP association
// on both sides ("dtls fatal: conn is closed"), which drives BufferedAmount
// back to 0 and would make such a test pass or fail based on pion's internal
// teardown ordering rather than the watchdog logic itself.
