package connect

import (
	"bytes"
	"context"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// A single datagram sent long ago must not license the guard to trip on a fresh
// burst: the unanswered window is measured from the burst's own start. Without
// that, one unacknowledged frame followed by a quiet spell and then normal
// traffic would switch the datagram lane off for the rest of the connection.
func TestH3DatagramSendGuardQuietIntervalBurstDoesNotTrip(t *testing.T) {
	t0 := time.Now()

	guard := newH3DatagramSendGuard()
	guard.noteSent(t0)
	burst := t0.Add(30 * time.Second)
	for i := int64(0); i < guard.minSent-1; i++ {
		guard.noteSent(burst)
	}
	if ok, tripped := guard.allow(burst.Add(10*time.Millisecond), true); !ok || tripped {
		t.Fatalf("a burst after a quiet interval tripped the guard: ok=%v trippedNow=%v", ok, tripped)
	}

	// The real blackhole still trips: the threshold reached, then unanswered past
	// the window, with the stream plainly alive.
	blackhole := newH3DatagramSendGuard()
	for i := int64(0); i < blackhole.minSent; i++ {
		blackhole.noteSent(t0)
	}
	if ok, tripped := blackhole.allow(t0.Add(30*time.Second), true); ok || !tripped {
		t.Fatalf("a blackhole must trip the guard: ok=%v trippedNow=%v", ok, tripped)
	}
	if !blackhole.Tripped() {
		t.Fatal("the guard must stay tripped once it has fired")
	}

	// And receiving one datagram clears it: the lane is alive.
	blackhole.noteReceived()
	if !blackhole.Tripped() {
		t.Fatal("a trip must not be undone by later traffic on the same connection")
	}
}

// A stream write error must end that connection generation so the transport can
// dial again. The dispatcher closes streamSend when the generation ends, so a
// drain registered ahead of the cancel waits on a channel nothing will close:
// the generation stalls and the transport never reconnects.
func TestH3HybridStreamWriteErrorDoesNotStallTheGeneration(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	switchH3DatagramSendGate(t, true)
	harness := startSendHarness(t, true, true)
	route := harness.route(t)

	// Abort the stream's read direction while the connection stays up: the client's
	// next write then fails, which is the error the teardown path has to survive.
	// Before the ordering was fixed the drain waited on a channel that only the
	// dispatcher closes after it sees the cancel that the same drain was holding
	// up, so the generation stalled and never re-dialed.
	harness.stream.CancelRead(0)
	select {
	case route <- MessagePoolCopy(bytes.Repeat([]byte("w"), 200)):
	case <-time.After(2 * time.Second):
	}

	select {
	case <-harness.sessions:
	case <-time.After(10 * time.Second):
		t.Fatal("the transport did not dial again after a stream write error: the generation stalled")
	}
}

// A path limit that cannot carry even one fragment must divert the message to
// the reliable stream instead of returning an error: an error there tears the
// whole H3 connection down over a datagram-path limit.
func TestH3HybridSendDatagramSubHeaderMTUFallsBackToStream(t *testing.T) {
	settings := DefaultH3DatagramSettings()
	fragmenter, err := NewH3DatagramFragmenter(settings, &H3DatagramStats{})
	if err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 200)
	if !settings.UseDatagram(len(message)) {
		t.Fatalf("fixture of %d bytes is not eligible for the datagram lane", len(message))
	}

	belowHeader := H3DatagramHeaderByteCount - 8
	useStream, _, err := fragmenter.SendHybrid(context.Background(), message, settings.TargetDatagramByteCount,
		func(context.Context, []byte) error {
			return &quic.DatagramTooLargeError{MaxDatagramPayloadSize: int64(belowHeader)}
		})
	if err != nil {
		t.Fatalf("a sub-header path limit must not surface as an error, got %v", err)
	}
	if !useStream {
		t.Fatal("a path that cannot carry one fragment must take the reliable stream")
	}
}
