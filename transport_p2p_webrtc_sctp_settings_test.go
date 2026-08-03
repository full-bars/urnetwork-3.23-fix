package connect

import (
	"context"
	"io"
	mathrand "math/rand"
	"net"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
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
