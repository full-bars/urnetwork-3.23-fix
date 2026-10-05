package connect

// Coverage for DATAGRAM invariants that the receive-side tests do not reach:
// who may offer the DATAGRAM transport, what happens when the receive route is
// full, and the two reassembly bounds (message count, retired-id window).

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

// captureWithOneFragmenter sends every message through a single fragmenter so
// each capture gets a distinct message id, and copies each datagram because the
// production fragmenter reuses one scratch buffer after each synchronous send.
func captureWithOneFragmenter(
	t *testing.T,
	settings *H3DatagramSettings,
	maxDatagramByteCount int,
	messages ...[]byte,
) [][][]byte {
	t.Helper()
	fragmenter, err := NewH3DatagramFragmenter(settings, &H3DatagramStats{})
	if err != nil {
		t.Fatal(err)
	}
	captured := make([][][]byte, 0, len(messages))
	for _, message := range messages {
		var fragments [][]byte
		_, err := fragmenter.Send(context.Background(), message, maxDatagramByteCount, func(_ context.Context, datagram []byte) error {
			fragments = append(fragments, bytes.Clone(datagram))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		captured = append(captured, fragments)
	}
	return captured
}

// The DATAGRAM offer belongs to the gated auxiliary H3 on the plain path only.
// A sole H3 target mode runs H3 as the only transport and must never offer —
// both runtime gates on must not change that, and the connection must still
// authenticate on the stream.
func TestH3DatagramSoleTargetModeNeverOffers(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	server := startH3TestServer(t, ctx, true, nil)
	dgBefore := H3DatagramCounters()
	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	transport.targetMode = TransportModeH3
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	session := waitForSession(t, server)
	if session.offerVersion != 0 || session.accepted {
		t.Fatalf("sole target mode: server saw offer=%d accepted=%v, want no offer", session.offerVersion, session.accepted)
	}
	if got := H3DatagramCounters().Offered - dgBefore.Offered; got != 0 {
		t.Fatalf("sole target mode counted %d offers, want 0", got)
	}
}

// The DNS translation modes are an availability fallback and never offer
// DATAGRAM. The offer is counted before the dial, so a wrong condition would
// show up even though this endpoint is closed: the DNS attempt must be counted
// and the offer must not.
func TestRunH3NeverOffersDatagramsForTheDnsMode(t *testing.T) {
	switchH3DatagramGate(t, true)
	before := H3DatagramCounters()
	statsBefore := TransportModeStats()
	transport, cancel := newClosedPortTransport(t, TransportModeH3Dns, true)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3Dns, 0, 1) }()
	time.Sleep(600 * time.Millisecond)
	cancel()
	<-done

	after := TransportModeStats()
	if after.Dns.Attempts <= statsBefore.Dns.Attempts {
		t.Fatal("expected the DNS mode attempt to be counted")
	}
	if got := H3DatagramCounters().Offered - before.Offered; got != 0 {
		t.Fatalf("DNS mode counted %d offers, want 0", got)
	}
}

// With no consumer on the receive route the channel fills to its buffer and
// every further datagram message is dropped immediately — counted as a drop,
// and never counted as payload received. The lane is unreliable and Transfer
// resends, so the pump must not block on a full route either.
func TestH3DatagramRouteFullDropsMessagesWithoutCountingPayload(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	message := bytes.Repeat([]byte("d"), 300)
	server := startH3TestServer(t, ctx, true, func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool) {
		if !accepted {
			return
		}
		settings := DefaultH3DatagramSettings()
		fragmenter, err := NewH3DatagramFragmenter(settings, &H3DatagramStats{})
		if err != nil {
			return
		}
		for i := 0; i < 120 && ctx.Err() == nil; i++ {
			if _, err := fragmenter.Send(context.Background(), message, settings.TargetDatagramByteCount, func(_ context.Context, datagram []byte) error {
				return conn.SendDatagram(datagram)
			}); err != nil {
				return
			}
		}
	})

	before := TransportModeStats()
	dgBefore := H3DatagramCounters()
	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	waitForSession(t, server)
	waitFor(t, "datagram drops on a full receive route", func() bool {
		return 0 < H3DatagramCounters().RxDrops-dgBefore.RxDrops
	})

	buffer := transport.settings.TransportBufferSize
	dg := H3DatagramCounters()
	if got := TransportModeStats().H3BytesRx - before.H3BytesRx; uint64(buffer)*uint64(len(message)) < got {
		t.Fatalf("payload rx = %d bytes, more than the %d-slot receive route accepted", got, buffer)
	}
	if dg.RxMessages-dgBefore.RxMessages <= dg.RxDrops-dgBefore.RxDrops {
		t.Fatalf("completed messages = %d with %d drops: drops must come from delivered messages",
			dg.RxMessages-dgBefore.RxMessages, dg.RxDrops-dgBefore.RxDrops)
	}
}

// MaxReassemblyMessageCount bounds how many ids may wait for their missing
// fragments at once, independently of the byte budget: a new id is refused at
// the limit, the counter records it, and once a pending message completes the
// refused id installs and finishes normally.
func TestH3DatagramReassemblyRefusesNewIdsAtTheMessageLimit(t *testing.T) {
	settings := DefaultH3DatagramSettings()
	settings.MaxFragmentCount = 2
	settings.MaxReassemblyMessageCount = 1
	messageA := bytes.Repeat([]byte{0xa1}, 32)
	messageB := bytes.Repeat([]byte{0xb2}, 32)
	maxDatagramByteCount := H3DatagramHeaderByteCount + 16
	datagrams := captureWithOneFragmenter(t, settings, maxDatagramByteCount, messageA, messageB)
	if len(datagrams[0]) != 2 || len(datagrams[1]) != 2 {
		t.Fatalf("want two 2-fragment messages, got %d and %d fragments", len(datagrams[0]), len(datagrams[1]))
	}

	budget := NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount)
	stats := &H3DatagramStats{}
	reassembler, err := NewH3DatagramReassembler(settings, budget, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer reassembler.Close()
	now := time.Unix(400, 0)

	if message := reassembler.Accept(datagrams[0][0], now); message != nil {
		MessagePoolReturn(message)
		t.Fatal("first fragment delivered a message")
	}
	if message := reassembler.Accept(datagrams[1][0], now); message != nil {
		MessagePoolReturn(message)
		t.Fatal("a second id installed while the message limit was one")
	}
	if snapshot := stats.Snapshot(); snapshot.ReassemblyLimitCount != 1 {
		t.Fatalf("limit refusal not recorded: %+v", snapshot)
	}

	received := reassembler.Accept(datagrams[0][1], now)
	if !bytes.Equal(received, messageA) {
		t.Fatalf("first message = %x, want %x", received, messageA)
	}
	MessagePoolReturn(received)

	if message := reassembler.Accept(datagrams[1][0], now); message != nil {
		MessagePoolReturn(message)
		t.Fatal("pending id refused before the limit freed")
	}
	received = reassembler.Accept(datagrams[1][1], now)
	if !bytes.Equal(received, messageB) {
		t.Fatalf("second message = %x, want %x", received, messageB)
	}
	MessagePoolReturn(received)

	if budget.Used() != 0 {
		t.Fatalf("reassembly retained %d shared bytes", budget.Used())
	}
	if snapshot := stats.Snapshot(); snapshot.ReceivedMessageCount != 2 || snapshot.ReassemblyLimitCount != 1 {
		t.Fatalf("stats=%+v", snapshot)
	}
}

// The retired-id window is deliberately bounded: after 256 ids have retired,
// the oldest is forgotten and a late fragment of it reassembles again (Transfer
// is the final duplicate authority), while a recent id is still suppressed.
func TestH3DatagramRetiredIdWindowForgetsTheOldest(t *testing.T) {
	settings := DefaultH3DatagramSettings()
	total := defaultH3DatagramReplayIds + 1
	messages := make([][]byte, total)
	for i := range messages {
		messages[i] = []byte(fmt.Sprintf("message-%03d", i))
	}
	datagrams := captureWithOneFragmenter(t, settings, settings.TargetDatagramByteCount, messages...)

	budget := NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount)
	stats := &H3DatagramStats{}
	reassembler, err := NewH3DatagramReassembler(settings, budget, stats)
	if err != nil {
		t.Fatal(err)
	}
	defer reassembler.Close()
	now := time.Unix(600, 0)

	for i, fragments := range datagrams {
		for _, datagram := range fragments {
			if received := reassembler.Accept(datagram, now); received != nil {
				if !bytes.Equal(received, messages[i]) {
					t.Fatalf("message %d = %x, want %x", i, received, messages[i])
				}
				MessagePoolReturn(received)
			}
		}
	}

	if received := reassembler.Accept(datagrams[0][0], now); received == nil {
		t.Fatal("the oldest id must age out of the bounded window")
	} else {
		if !bytes.Equal(received, messages[0]) {
			t.Fatalf("aged-out id reassembled to %x, want %x", received, messages[0])
		}
		MessagePoolReturn(received)
	}
	if duplicate := reassembler.Accept(datagrams[total-1][0], now); duplicate != nil {
		MessagePoolReturn(duplicate)
		t.Fatal("the newest id must still be suppressed as a duplicate")
	}
	if budget.Used() != 0 {
		t.Fatalf("reassembly retained %d shared bytes", budget.Used())
	}
}
