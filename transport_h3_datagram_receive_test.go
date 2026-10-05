package connect

// Integration tests for the DATAGRAM receive side of the auxiliary H3
// transport, against a loopback QUIC server that accepts or declines the offer.

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/protocol"
)

const h3DatagramTestProto = "urnetwork-h3-datagram-receive-test"

func switchH3DatagramGate(t *testing.T, enabled bool) {
	t.Helper()
	previous := SetH3DatagramsEnabled(enabled)
	t.Cleanup(func() { SetH3DatagramsEnabled(previous) })
}

// h3TestSession is what the loopback server learned about one connection.
type h3TestSession struct {
	offerVersion uint32
	accepted     bool
}

type h3TestServer struct {
	port     int
	sessions chan h3TestSession
}

// startH3TestServer listens on loopback. accept says whether it takes a
// DATAGRAM offer (true) or echoes the request bytes unchanged like an old server
// (false). after runs per connection once auth is answered, with the datagram
// fragmenter ready.
func startH3TestServer(
	t *testing.T,
	ctx context.Context,
	accept bool,
	after func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool),
) *h3TestServer {
	t.Helper()
	certPem, keyPem, err := selfSign([]string{"127.0.0.1"}, "127.0.0.1", 24*time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddrEarly(
		"127.0.0.1:0",
		&tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{h3DatagramTestProto}},
		&quic.Config{EnableDatagrams: true, MaxIdleTimeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	server := &h3TestServer{
		port:     listener.Addr().(*net.UDPAddr).Port,
		sessions: make(chan h3TestSession, 16),
	}
	framerSettings := DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))

	handle := func(conn *quic.Conn) {
		defer conn.CloseWithError(0, "test complete")
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		framer := NewFramer(framerSettings)
		authBytes, err := framer.Read(stream)
		if err != nil {
			return
		}
		defer MessagePoolReturn(authBytes)
		decoded, err := DecodeFrame(authBytes)
		if err != nil {
			return
		}
		auth, ok := decoded.(*protocol.Auth)
		if !ok {
			return
		}
		session := h3TestSession{offerVersion: auth.H3DatagramVersion}
		if accept {
			state := conn.ConnectionState()
			response, accepted := AcceptH3DatagramAuthOffer(
				auth, true, state.SupportsDatagrams.Local, state.SupportsDatagrams.Remote)
			session.accepted = accepted
			responseBytes, err := EncodeFrame(response, DefaultProtocolVersion)
			if err != nil {
				return
			}
			err = framer.Write(stream, responseBytes)
			MessagePoolReturn(responseBytes)
			if err != nil {
				return
			}
		} else if err := framer.Write(stream, authBytes); err != nil {
			return
		}
		server.sessions <- session
		if after != nil {
			after(ctx, conn, stream, framer, session.accepted)
		}
		<-ctx.Done()
	}
	go func() {
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return server
}

// newH3DatagramTestTransport builds an eligible auxiliary H3 transport aimed at
// the loopback server, with a route manager so it can register routes.
func newH3DatagramTestTransport(ctx context.Context, cancel context.CancelFunc, port int) *PlatformTransport {
	settings := DefaultPlatformTransportSettings()
	settings.EnableH3 = true
	settings.H3Port = port
	settings.FramerSettings = DefaultFramerSettings(int(DefaultClientSettings().MinimumMessageLenLimit()))
	settings.QuicTlsConfig = &tls.Config{
		InsecureSkipVerify: true, // test-only self-signed endpoint
		NextProtos:         []string{h3DatagramTestProto},
	}
	settings.QuicConnectTimeout = time.Second
	settings.QuicHandshakeTimeout = time.Second
	settings.AuthTimeout = time.Second
	settings.ReconnectTimeout = 20 * time.Millisecond
	transport := &PlatformTransport{
		ctx:                  ctx,
		cancel:               cancel,
		log:                  loggerOrDefault(nil),
		clientStrategy:       &ClientStrategy{settings: DefaultClientStrategySettings()},
		routeManager:         NewRouteManager(ctx, "h3-datagram-receive-test"),
		platformUrl:          "wss://127.0.0.1:1",
		settings:             settings,
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		targetMode:           TransportModeAuto,
		mode:                 NewMonitorValue[TransportMode](TransportModeNone),
	}
	transport.auth.Store(&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing"})
	return transport
}

func waitForSession(t *testing.T, server *h3TestServer) h3TestSession {
	t.Helper()
	select {
	case session := <-server.sessions:
		return session
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for the H3 connection to authenticate")
		return h3TestSession{}
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// sendSmallDatagramMessage sends one message that fits a single DATAGRAM.
func sendSmallDatagramMessage(t *testing.T, conn *quic.Conn, message []byte) {
	t.Helper()
	settings := DefaultH3DatagramSettings()
	if !settings.UseDatagram(len(message)) {
		t.Fatalf("fixture of %d bytes does not fit a datagram", len(message))
	}
	fragmenter, err := NewH3DatagramFragmenter(settings, &H3DatagramStats{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fragmenter.Send(context.Background(), message, settings.TargetDatagramByteCount, func(_ context.Context, datagram []byte) error { return conn.SendDatagram(datagram) }); err != nil {
		t.Fatal(err)
	}
}

// A server that accepts the offer sends small frames as DATAGRAM, large ones on
// the stream, and the transport must deliver both to its receive route.
func TestH3DatagramAcceptedDeliversBothLanes(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	small := bytes.Repeat([]byte("d"), 200)
	big := bytes.Repeat([]byte("s"), 3000)
	server := startH3TestServer(t, ctx, true, func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool) {
		if !accepted {
			return
		}
		sendSmallDatagramMessage(t, conn, small)
		stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
		framer.Write(stream, big)
	})

	before := TransportModeStats()
	dgBefore := H3DatagramCounters()
	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	session := waitForSession(t, server)
	if session.offerVersion != H3DatagramProtocolVersion || !session.accepted {
		t.Fatalf("server saw offer=%d accepted=%v", session.offerVersion, session.accepted)
	}
	waitFor(t, "both lanes delivered", func() bool {
		return TransportModeStats().H3FramesRx >= before.H3FramesRx+2
	})
	dg := H3DatagramCounters()
	if dg.Offered != dgBefore.Offered+1 || dg.Accepted != dgBefore.Accepted+1 {
		t.Fatalf("offered/accepted = %d/%d, want +1/+1", dg.Offered-dgBefore.Offered, dg.Accepted-dgBefore.Accepted)
	}
	if dg.RxMessages != dgBefore.RxMessages+1 {
		t.Fatalf("datagram layer delivered %d messages, want 1", dg.RxMessages-dgBefore.RxMessages)
	}
	if got := TransportModeStats().H3BytesRx - before.H3BytesRx; got != uint64(len(small)+len(big)) {
		t.Fatalf("h3 payload bytes in = %d, want %d", got, len(small)+len(big))
	}
	if H3DatagramCounters().HealthSuffix() == "" {
		t.Fatal("the [health] suffix must appear once DATAGRAM was offered")
	}
}

// An old server echoes the request bytes unchanged. The connection must stay up
// on the plain stream, which is the intended fallback, not fail auth.
func TestH3DatagramOfferToAnOldServerFallsBackToTheStream(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	big := bytes.Repeat([]byte("s"), 500)
	server := startH3TestServer(t, ctx, false, func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool) {
		stream.SetWriteDeadline(time.Now().Add(2 * time.Second))
		framer.Write(stream, big)
	})

	before := TransportModeStats()
	dgBefore := H3DatagramCounters()
	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	session := waitForSession(t, server)
	if session.offerVersion != H3DatagramProtocolVersion || session.accepted {
		t.Fatalf("server saw offer=%d accepted=%v", session.offerVersion, session.accepted)
	}
	waitFor(t, "the stream frame delivered", func() bool {
		return TransportModeStats().H3FramesRx >= before.H3FramesRx+1
	})
	dg := H3DatagramCounters()
	if dg.Offered != dgBefore.Offered+1 || dg.Accepted != dgBefore.Accepted {
		t.Fatalf("offered/accepted = +%d/+%d, want +1/+0", dg.Offered-dgBefore.Offered, dg.Accepted-dgBefore.Accepted)
	}
	if got := TransportModeStats().H3ConnectFailures - before.H3ConnectFailures; got != 0 {
		t.Fatalf("a declined offer counted as %d connect failures", got)
	}
}

// With the gate off the offer is not made: the connection is the legacy H3 one
// and an old-style byte-for-byte echo check still applies.
func TestH3DatagramGateOffMakesNoOffer(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, false)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	server := startH3TestServer(t, ctx, true, nil)
	dgBefore := H3DatagramCounters()
	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	session := waitForSession(t, server)
	if session.offerVersion != 0 || session.accepted {
		t.Fatalf("gate off: server saw offer=%d accepted=%v", session.offerVersion, session.accepted)
	}
	if got := H3DatagramCounters().Offered - dgBefore.Offered; got != 0 {
		t.Fatalf("gate off but %d offers counted", got)
	}
}

// Closing the transport while datagrams are arriving must not panic: the
// receive channel has one closer, after both readers have finished. A second
// reader sending on a channel the stream reader closed would take down the
// provider, so this is run under -race and repeatedly.
func TestH3DatagramShutdownWhileDatagramsFlowDoesNotPanic(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	for i := 0; i < 5; i++ {
		func() {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var sent atomic.Int64
			server := startH3TestServer(t, ctx, true, func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool) {
				settings := DefaultH3DatagramSettings()
				fragmenter, err := NewH3DatagramFragmenter(settings, &H3DatagramStats{})
				if err != nil {
					return
				}
				message := bytes.Repeat([]byte("f"), 300)
				for ctx.Err() == nil {
					if _, err := fragmenter.Send(context.Background(), message, settings.TargetDatagramByteCount, func(_ context.Context, datagram []byte) error { return conn.SendDatagram(datagram) }); err != nil {
						return
					}
					sent.Add(1)
				}
			})
			transportCtx, transportCancel := context.WithCancel(ctx)
			transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
			done := make(chan struct{})
			go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()

			waitForSession(t, server)
			waitFor(t, "datagrams flowing", func() bool { return 50 <= sent.Load() })
			time.Sleep(time.Duration(i*7) * time.Millisecond)
			transportCancel()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("runH3 did not return after the transport was closed")
			}
		}()
	}
}

// Changing the datagram gate closes the live connection and reconnects with the
// new offer. That is an operator action, so it must not count as an H3 drop.
func TestH3DatagramGateChangeReconnectsWithoutCountingADrop(t *testing.T) {
	switchH3Gate(t, true)
	switchH3DatagramGate(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	server := startH3TestServer(t, ctx, true, nil)
	dropsBefore := TransportModeStats().H3Drops
	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	first := waitForSession(t, server)
	if first.offerVersion != H3DatagramProtocolVersion {
		t.Fatalf("first connection offer=%d", first.offerVersion)
	}
	SetH3DatagramsEnabled(false)
	second := waitForSession(t, server)
	if second.offerVersion != 0 || second.accepted {
		t.Fatalf("after switching off: offer=%d accepted=%v", second.offerVersion, second.accepted)
	}
	if got := TransportModeStats().H3Drops - dropsBefore; got != 0 {
		t.Fatalf("a gate change counted as %d drops", got)
	}
}
