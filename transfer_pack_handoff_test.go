package connect

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// newHandoffTestSequence builds a receive sequence whose inbound Pack channel
// admits exactly one Pack, so the second handoff has to wait for capacity.
func newHandoffTestSequence(
	ctx context.Context,
	t *testing.T,
	settings *ReceiveBufferSettings,
) (*Client, *ReceiveSequence) {
	t.Helper()
	client := &Client{}
	sequence := NewReceiveSequence(
		ctx,
		client,
		SourceId(NewId()),
		NewId(),
		sequenceTlsRoleServer,
		false,
		settings,
	)
	return client, sequence
}

func newHandoffTestPack() *ReceivePack {
	return &ReceivePack{
		MessageByteCount:   40,
		TransferFrameBytes: MessagePoolGet(40),
	}
}

type handoffPackResult struct {
	accepted bool
	err      error
}

// handoffPackAsync offers one Pack from another goroutine and returns the
// channel its result will arrive on.
func handoffPackAsync(sequence *ReceiveSequence, receivePack *ReceivePack, timeout time.Duration) <-chan handoffPackResult {
	result := make(chan handoffPackResult, 1)
	go func() {
		accepted, err := sequence.Pack(receivePack, timeout)
		result <- handoffPackResult{accepted: accepted, err: err}
	}()
	return result
}

// waitForHandoffWait parks until the sequence has entered the handoff wait,
// so the assertions below never race the goroutine's first instruction.
func waitForHandoffWait(t *testing.T, client *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for client.ReceiveStats().PackHandoffWaitCount == 0 {
		if deadline.Before(time.Now()) {
			t.Fatal("full handoff did not enter the wait")
		}
		runtime.Gosched()
	}
}

// The reliable-lane setting defaults to a negative wait, which is a wait for
// capacity or cancellation rather than a synthetic gap. A 25 ms observation is
// longer than the rejected fixed timeout and must not complete the handoff.
func TestReceiveSequenceH1HandoffNegativeWaitPreservesReliableBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultReceiveBufferSettings()
	settings.SequenceBufferSize = 1
	settings.H1PackHandoffTimeout = -1
	client, sequence := newHandoffTestSequence(ctx, t, settings)
	defer sequence.Close()

	first := newHandoffTestPack()
	second := newHandoffTestPack()
	if accepted, err := sequence.Pack(first, 0); !accepted || err != nil {
		t.Fatalf("fill H1 handoff: accepted=%t err=%v", accepted, err)
	}
	result := handoffPackAsync(sequence, second, settings.packHandoffTimeout(TransportTypeH1))
	waitForHandoffWait(t, client)
	if got := client.ReceiveStats().PackHandoffWaitCount; got != 1 {
		t.Fatalf("handoff wait count = %d, want 1", got)
	}

	// The wait must remain owned by channel capacity rather than turn into a
	// synthetic gap once a fixed interval passes.
	select {
	case premature := <-result:
		t.Fatalf("negative H1 wait completed while full: %+v", premature)
	case <-time.After(25 * time.Millisecond):
	}

	dequeued := <-sequence.packs
	MessagePoolReturn(dequeued.TransferFrameBytes)
	select {
	case got := <-result:
		if !got.accepted || got.err != nil {
			t.Fatalf("lossless H1 handoff result: accepted=%t err=%v", got.accepted, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("lossless H1 handoff did not wake after capacity opened")
	}
	if stats := client.ReceiveStats(); stats.PackHandoffWaitSuccess != 1 ||
		stats.PackHandoffDropCount != 0 {
		t.Fatalf("lossless H1 handoff stats = %+v", stats)
	}

	dequeued = <-sequence.packs
	MessagePoolReturn(dequeued.TransferFrameBytes)
}

// Cancelling the sequence is the only other way out of a negative wait. The
// caller keeps ownership of the rejected Pack and nothing is retained.
func TestReceiveSequenceH1HandoffNegativeWaitCancellationReturnsToCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultReceiveBufferSettings()
	settings.SequenceBufferSize = 1
	settings.H1PackHandoffTimeout = -1
	client, sequence := newHandoffTestSequence(ctx, t, settings)
	defer sequence.Close()
	defer cancel()

	first := newHandoffTestPack()
	second := newHandoffTestPack()
	if accepted, err := sequence.Pack(first, 0); !accepted || err != nil {
		t.Fatalf("fill H1 handoff: accepted=%t err=%v", accepted, err)
	}
	result := handoffPackAsync(sequence, second, settings.packHandoffTimeout(TransportTypeH1))
	waitForHandoffWait(t, client)

	cancel()
	select {
	case got := <-result:
		if got.accepted || got.err == nil {
			t.Fatalf("canceled H1 handoff result: accepted=%t err=%v", got.accepted, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled H1 handoff remained blocked")
	}
	// a rejected handoff leaves ownership with the caller
	secondBytes := second.TransferFrameBytes
	if pooled, _ := MessagePoolCheck(secondBytes); !pooled {
		t.Fatal("canceled H1 Pack did not return ownership to its caller")
	}
	MessagePoolReturn(second.TransferFrameBytes)
	if pooled, _ := MessagePoolCheck(secondBytes); pooled {
		t.Fatal("caller did not release canceled H1 Pack ownership")
	}
	if stats := client.ReceiveStats(); stats.PackHandoffWaitCount != 1 ||
		stats.PackHandoffWaitSuccess != 0 {
		t.Fatalf("canceled H1 handoff stats = %+v", stats)
	}

	dequeued := <-sequence.packs
	MessagePoolReturn(dequeued.TransferFrameBytes)
}

// The selector is the pin's: an exact route that publishes its reliability
// decides on its own, an unclassified H1 stream falls back to its transport
// type, and every other carrier stays nonblocking.
func TestReceiveBufferSettingsPackHandoffTimeoutSelection(t *testing.T) {
	if got := DefaultReceiveBufferSettings().ReliablePackHandoffTimeout; got != -1 {
		t.Fatalf("ReliablePackHandoffTimeout default = %s, want -1", got)
	}

	settings := DefaultReceiveBufferSettings()
	settings.H1PackHandoffTimeout = 3 * time.Second
	settings.ReliablePackHandoffTimeout = -1
	cases := []struct {
		name          string
		transportType TransportType
		reliability   CarrierReliability
		want          time.Duration
	}{
		{"legacy h1", TransportTypeH1, CarrierReliabilityUnknown, 3 * time.Second},
		{"reliable carrier", TransportTypeP2p, CarrierReliabilityReliable, -1},
		{"reliable h1", TransportTypeH1, CarrierReliabilityReliable, -1},
		{"unreliable carrier", TransportTypeP2p, CarrierReliabilityUnreliable, 0},
		{"unreliable h1", TransportTypeH1, CarrierReliabilityUnreliable, 0},
		{"other transport", TransportTypeH3, CarrierReliabilityUnknown, 0},
	}
	for _, c := range cases {
		if got := settings.packHandoffTimeout(c.transportType, c.reliability); got != c.want {
			t.Fatalf("%s handoff timeout = %s, want %s", c.name, got, c.want)
		}
	}
	if got := settings.packHandoffTimeout(TransportTypeH1); got != 3*time.Second {
		t.Fatalf("unclassified H1 handoff timeout = %s, want the H1 bound", got)
	}
	var nilSettings *ReceiveBufferSettings
	if got := nilSettings.packHandoffTimeout(TransportTypeH1); got != 0 {
		t.Fatalf("nil settings handoff timeout = %s, want 0", got)
	}
}
