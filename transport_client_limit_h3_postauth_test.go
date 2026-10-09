package connect

import (
	"context"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// The production kick on H3: the server accepts the connection, echoes the
// auth, and only then runs the intent check and closes with the client limit
// code. This is the path the pre-auth test does not reach, because that one
// closes before the echo and ends in the dial-error branch.
func TestPlatformTransportH3PostAuthCloseHoldsDials(t *testing.T) {
	switchH3Gate(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	server := startH3TestServer(t, ctx, false, func(ctx context.Context, conn *quic.Conn, stream *quic.Stream, framer *Framer, accepted bool) {
		// the auth echo has been written: now the platform kicks the client
		conn.CloseWithError(ClientLimitCloseCode, ClientLimitCloseText)
	})

	clock := newTestingClientLimitClock()
	backoff := newTestingClientLimitBackoff(clock, 0)
	initFails := backendFails()

	transportCtx, transportCancel := context.WithCancel(ctx)
	transport := newH3DatagramTestTransport(transportCtx, transportCancel, server.port)
	transport.clientLimitBackoff = backoff
	transport.settings.ClientLimitBackoff = backoff
	transport.auth.Store(&ClientAuth{ByJwt: "testing", InstanceId: NewId(), AppVersion: "testing", ProvideIntent: true})

	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3, 0, 1) }()
	defer func() { transportCancel(); <-done }()

	waitForSession(t, server)
	waitFor(t, "the hold to start after the post-auth close", func() bool {
		return backoff.Status().Exceeded
	})

	// held: the transport must not dial again while the hold stands, and the
	// kick must not count as a backend failure or an H3 connect failure
	select {
	case <-server.sessions:
		t.Fatal("the transport dialed again during the client limit hold")
	case <-time.After(500 * time.Millisecond):
	}
	if got := backendFails(); got != initFails {
		t.Fatalf("backend fails changed after a client limit close: was %d, got %d", initFails, got)
	}

	// once the hold ends the transport dials again
	clock.advance(ClientLimitBackoffTimeout + ClientLimitBackoffJitter + time.Second)
	waitForSession(t, server)
}
