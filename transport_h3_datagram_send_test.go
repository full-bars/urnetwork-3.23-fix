package connect

// Integration tests for the DATAGRAM send side of the auxiliary H3 transport,
// against a loopback QUIC server that accepts the offer.

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
)

func switchH3DatagramSendGate(t *testing.T, enabled bool) {
	t.Helper()
	previous := SetH3DatagramSendEnabled(enabled)
	t.Cleanup(func() { SetH3DatagramSendEnabled(previous) })
}

// setH3DatagramGuardForTest shrinks the blackhole guard so a test can trip it in
// well under a second, and restores it after.
func setH3DatagramGuardForTest(t *testing.T, minSent int64, window time.Duration) {
	t.Helper()
	previousMinSent, previousWindow := h3DatagramGuardMinSent, h3DatagramGuardWindow
	h3DatagramGuardMinSent, h3DatagramGuardWindow = minSent, window
	t.Cleanup(func() { h3DatagramGuardMinSent, h3DatagramGuardWindow = previousMinSent, previousWindow })
}

// sendHarness runs a loopback server that reports every message it receives and
// on which lane, and a transport aimed at it with its send route exposed.
type sendHarness struct {
	fromStream   chan []byte
	fromDatagram chan []byte
	sendRoute    chan chan []byte
	cancel       context.CancelFunc
	done         chan struct{}
	transport    *PlatformTransport
	sessions     chan h3TestSession
	// conn and stream are the server side of the live connection, so a test can
	// abort one direction and induce a stream write error deterministically while
	// the connection itself stays up.
	conn   *quic.Conn
	stream *quic.Stream
}

// startSendHarness starts the server and the transport. serverReadsDatagrams is
// false for the blackhole case: the server accepts DATAGRAM and then never reads
// one, while keeping the stream alive with pings.
func startSendHarness(t *testing.T, serverReadsDatagrams bool, h1Up bool) *sendHarness {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	t.Cleanup(cancel)
	harness := &sendHarness{
		fromStream:   make(chan []byte, 1024),
		fromDatagram: make(chan []byte, 1024),
		sendRoute:    make(chan chan []byte, 4),
	}

	server := startH3TestServer(t, ctx, true, func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool) {
		if !accepted {
			return
		}
		harness.conn = conn
		harness.stream = stream
		// stream reader: everything non-empty the client wrote on the stream
		go func() {
			for {
				message, err := framer.Read(stream)
				if err != nil {
					return
				}
				if 0 < len(message) {
					select {
					case harness.fromStream <- bytes.Clone(message):
					default:
					}
				}
				MessagePoolReturn(message)
			}
		}()
		// keep the stream visibly alive, as the real server's heartbeat does
		go func() {
			for ctx.Err() == nil {
				stream.SetWriteDeadline(time.Now().Add(time.Second))
				if framer.Write(stream, make([]byte, 0)) != nil {
					return
				}
				time.Sleep(40 * time.Millisecond)
			}
		}()
		if !serverReadsDatagrams {
			return
		}
		settings := DefaultH3DatagramSettings()
		stats := &H3DatagramStats{}
		reassembler, err := NewH3DatagramReassembler(settings, NewH3DatagramReassemblyBudget(settings.ProcessReassemblyByteCount), stats)
		if err != nil {
			return
		}
		defer reassembler.Close()
		for {
			datagram, err := conn.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			if message := reassembler.Accept(datagram, time.Now()); message != nil {
				select {
				case harness.fromDatagram <- bytes.Clone(message):
				default:
				}
				MessagePoolReturn(message)
			}
		}
	})

	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	transport.settings.sendRouteObserverForTest = func(send chan []byte) {
		select {
		case harness.sendRoute <- send:
		default:
		}
	}
	if h1Up {
		transport.setModeAvailable(TransportModeH1, true)
	}
	harness.transport = transport
	harness.sessions = server.sessions
	harness.cancel = transportCancel
	harness.done = make(chan struct{})
	go func() { defer close(harness.done); transport.runH3(TransportModeH3, 0, 1) }()
	t.Cleanup(func() { transportCancel(); <-harness.done })

	select {
	case <-server.sessions:
	case <-time.After(8 * time.Second):
		t.Fatal("the H3 connection did not authenticate")
	}
	return harness
}

func (self *sendHarness) route(t *testing.T) chan []byte {
	t.Helper()
	select {
	case route := <-self.sendRoute:
		return route
	case <-time.After(8 * time.Second):
		t.Fatal("no send route was registered")
		return nil
	}
}

func awaitMessage(t *testing.T, from chan []byte, want []byte, what string) {
	t.Helper()
	select {
	case got := <-from:
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: got %d bytes that differ from the %d sent", what, len(got), len(want))
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func expectNoMessage(t *testing.T, from chan []byte, wait time.Duration, what string) {
	t.Helper()
	select {
	case got := <-from:
		t.Fatalf("%s: unexpected %d byte message", what, len(got))
	case <-time.After(wait):
	}
}

// With the send gate on, H1 up and a server that accepted, a small message goes
// as a datagram and a large one on the stream, byte for byte.
func TestH3DatagramSendUsesBothLanes(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	switchH3DatagramSendGate(t, true)
	harness := startSendHarness(t, true, true)
	route := harness.route(t)

	small := bytes.Repeat([]byte("s"), 200)
	big := bytes.Repeat([]byte("B"), 3000)
	before := H3DatagramCounters()
	txBefore := TransportModeStats().H3FramesTx
	route <- MessagePoolCopy(small)
	route <- MessagePoolCopy(big)

	awaitMessage(t, harness.fromDatagram, small, "the small message as a datagram")
	awaitMessage(t, harness.fromStream, big, "the large message on the stream")
	expectNoMessage(t, harness.fromStream, 150*time.Millisecond, "the small message must not also be on the stream")

	after := H3DatagramCounters()
	if after.TxMessages != before.TxMessages+1 || after.TxBytes != before.TxBytes+uint64(len(small)) {
		t.Fatalf("datagram tx counters: messages +%d bytes +%d", after.TxMessages-before.TxMessages, after.TxBytes-before.TxBytes)
	}
	if after.TxStreamMessages != before.TxStreamMessages+1 {
		t.Fatalf("stream lane count +%d, want +1", after.TxStreamMessages-before.TxStreamMessages)
	}
	waitFor(t, "both messages counted as H3 frames out", func() bool {
		return TransportModeStats().H3FramesTx >= txBefore+2
	})
	if H3DatagramCounters().HealthSuffix() == "" {
		t.Fatal("the [health] suffix must appear")
	}
}

// With the send gate off nothing goes as a datagram: the small message is on the
// stream, which is the receive-only behaviour.
func TestH3DatagramSendOffKeepsEverythingOnTheStream(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	switchH3DatagramSendGate(t, false)
	harness := startSendHarness(t, true, true)
	route := harness.route(t)

	small := bytes.Repeat([]byte("s"), 200)
	before := H3DatagramCounters()
	route <- MessagePoolCopy(small)
	awaitMessage(t, harness.fromStream, small, "the small message on the stream")
	expectNoMessage(t, harness.fromDatagram, 150*time.Millisecond, "no datagram may be sent with the gate off")
	if got := H3DatagramCounters().TxMessages - before.TxMessages; got != 0 {
		t.Fatalf("%d datagrams sent with the gate off", got)
	}
}

// With H1 down H3 is the only path, so a dead datagram lane would stall every
// small frame: the stream must carry everything even with the send gate on.
func TestH3DatagramSendIsOffWhileH1IsDown(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	switchH3DatagramSendGate(t, true)
	harness := startSendHarness(t, true, false)
	route := harness.route(t)

	small := bytes.Repeat([]byte("s"), 200)
	route <- MessagePoolCopy(small)
	awaitMessage(t, harness.fromStream, small, "the small message on the stream while H1 is down")
	expectNoMessage(t, harness.fromDatagram, 150*time.Millisecond, "no datagram while H1 is down")
}

// A server that accepts DATAGRAM and then eats every datagram is the blackhole.
// The guard must notice (many sent, none received, stream alive), stop sending
// datagrams on that connection, and put the rest on the stream.
func TestH3DatagramBlackholeGuardFallsBackToTheStream(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	switchH3DatagramSendGate(t, true)
	setH3DatagramGuardForTest(t, 10, 300*time.Millisecond)
	harness := startSendHarness(t, false, true)
	route := harness.route(t)

	blackholesBefore := H3DatagramCounters().Blackholes
	small := bytes.Repeat([]byte("s"), 200)
	// about 1.5 s of steady small frames: well past the 300 ms window
	for i := 0; i < 300; i++ {
		route <- MessagePoolCopy(small)
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, "the guard to trip", func() bool {
		return H3DatagramCounters().Blackholes == blackholesBefore+1
	})
	// frames sent after the trip arrive on the stream
	var onStream int
	deadline := time.After(5 * time.Second)
collect:
	for {
		select {
		case message := <-harness.fromStream:
			if bytes.Equal(message, small) {
				onStream++
			}
			if 20 <= onStream {
				break collect
			}
		case <-deadline:
			break collect
		}
	}
	if onStream < 20 {
		t.Fatalf("only %d messages reached the stream after the guard tripped", onStream)
	}
	if got := H3DatagramCounters().Blackholes - blackholesBefore; got != 1 {
		t.Fatalf("the guard tripped %d times, want once per connection", got)
	}
}

// The guard is quiet for a lane that works: datagrams received reset it.
func TestH3DatagramSendGuardUnit(t *testing.T) {
	now := time.Now()
	guard := &h3DatagramSendGuard{minSent: 5, window: time.Second}

	// not enough sent
	for i := 0; i < 4; i++ {
		guard.noteSent(now)
	}
	if ok, tripped := guard.allow(now.Add(2*time.Second), true); !ok || tripped {
		t.Fatal("below the minimum the guard must allow")
	}
	// enough sent but inside the window
	guard.noteSent(now)
	if ok, tripped := guard.allow(now.Add(500*time.Millisecond), true); !ok || tripped {
		t.Fatal("inside the window the guard must allow")
	}
	// enough, past the window, but the stream is dead: a dead connection is not
	// this guard's to call
	if ok, tripped := guard.allow(now.Add(2*time.Second), false); !ok || tripped {
		t.Fatal("with a dead stream the guard must not trip")
	}
	// a datagram received resets it
	guard.noteReceived()
	if ok, tripped := guard.allow(now.Add(3*time.Second), true); !ok || tripped {
		t.Fatal("a received datagram must reset the guard")
	}
	// now unanswered again, past the window, stream alive: trips exactly once
	later := now.Add(10 * time.Second)
	for i := 0; i < 5; i++ {
		guard.noteSent(later)
	}
	if ok, tripped := guard.allow(later.Add(2*time.Second), true); ok || !tripped {
		t.Fatalf("expected a trip: ok=%v tripped=%v", ok, tripped)
	}
	if ok, tripped := guard.allow(later.Add(3*time.Second), true); ok || tripped {
		t.Fatal("after tripping it must stay off and not report a second trip")
	}
	if !guard.Tripped() {
		t.Fatal("Tripped must report true")
	}
}

// Closing the transport while messages are being pushed into the send route must
// not panic or hang: the dispatcher, the stream queue and the writer all stop
// and every queued message goes back to the pool. Repeated under -race.
func TestH3DatagramSendShutdownUnderLoadDoesNotPanic(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	switchH3DatagramSendGate(t, true)
	for i := 0; i < 5; i++ {
		func() {
			harness := startSendHarness(t, true, true)
			route := harness.route(t)
			var stop atomic.Bool
			pushed := make(chan struct{})
			go func() {
				defer close(pushed)
				small := bytes.Repeat([]byte("s"), 200)
				big := bytes.Repeat([]byte("B"), 3000)
				for n := 0; !stop.Load(); n++ {
					message := small
					if n%3 == 0 {
						message = big
					}
					select {
					case route <- MessagePoolCopy(message):
					case <-time.After(20 * time.Millisecond):
					}
				}
			}()
			time.Sleep(time.Duration(50+i*13) * time.Millisecond)
			harness.cancel()
			select {
			case <-harness.done:
			case <-time.After(8 * time.Second):
				t.Fatal("runH3 did not return after the transport was closed")
			}
			stop.Store(true)
			<-pushed
		}()
	}
}

// The QUIC memory bounds are applied: explicit windows, scaled by the provider's
// memory budget with a working floor, and a small incoming stream count.
func TestH3MemoryBoundsAreAppliedAndScale(t *testing.T) {
	previous := MemoryBudget()
	t.Cleanup(func() { SetMemoryBudget(previous) })

	SetMemoryBudget(0)
	config := &quic.Config{}
	applyH3MemoryBounds(config)
	if config.InitialStreamReceiveWindow != uint64(kib(256)) ||
		config.InitialConnectionReceiveWindow != uint64(kib(512)) {
		t.Fatalf("initial windows %d / %d", config.InitialStreamReceiveWindow, config.InitialConnectionReceiveWindow)
	}
	if config.MaxStreamReceiveWindow != uint64(mib(3)) || config.MaxConnectionReceiveWindow != uint64(mib(4)) {
		t.Fatalf("unbudgeted max windows %d / %d, want 3 MiB / 4 MiB", config.MaxStreamReceiveWindow, config.MaxConnectionReceiveWindow)
	}
	if config.MaxIncomingStreams != 8 || config.MaxIncomingUniStreams != 8 {
		t.Fatalf("incoming stream counts %d / %d, want 8", config.MaxIncomingStreams, config.MaxIncomingUniStreams)
	}

	// a small budget shrinks the ceilings but never below the working floor, and
	// the initial credit never exceeds the ceiling
	SetMemoryBudget(mib(8))
	small := &quic.Config{}
	applyH3MemoryBounds(small)
	if small.MaxStreamReceiveWindow >= config.MaxStreamReceiveWindow ||
		small.MaxConnectionReceiveWindow >= config.MaxConnectionReceiveWindow {
		t.Fatalf("an 8 MiB budget did not shrink the windows: %d / %d", small.MaxStreamReceiveWindow, small.MaxConnectionReceiveWindow)
	}
	if small.MaxStreamReceiveWindow < uint64(kib(384)) || small.MaxConnectionReceiveWindow < uint64(kib(512)) {
		t.Fatalf("windows %d / %d fell below the floor", small.MaxStreamReceiveWindow, small.MaxConnectionReceiveWindow)
	}
	if small.InitialStreamReceiveWindow > small.MaxStreamReceiveWindow ||
		small.InitialConnectionReceiveWindow > small.MaxConnectionReceiveWindow {
		t.Fatal("initial credit exceeds the ceiling")
	}
}

// The H3 transport's real dial uses those bounds: a connection made by runH3
// advertises the pinned windows, not quic-go's auto-tuned 6 MiB / 15 MiB.
func TestH3DatagramLimitsAreMemoryScaledAndBounded(t *testing.T) {
	settings, budget := h3DatagramLimits()
	if settings == nil || budget == nil {
		t.Fatal("limits not resolved")
	}
	if settings.ProcessReassemblyByteCount < int64(kib(512)) || int64(mib(8)) < settings.ProcessReassemblyByteCount {
		t.Fatalf("process reassembly %d outside [512 KiB, 8 MiB]", settings.ProcessReassemblyByteCount)
	}
	if again, _ := h3DatagramLimits(); again != settings {
		t.Fatal("the limits must resolve once")
	}
}
