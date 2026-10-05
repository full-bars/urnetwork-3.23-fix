package connect

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// dialLoopbackH3 builds an h3ConnStream the way runH3 does: a quic.Transport
// around the caller's own socket, with no isSingleUse, against an in-process server.
func dialLoopbackH3(t *testing.T) (*h3ConnStream, *net.UDPConn) {
	t.Helper()
	listener, err := quic.Listen(listenLoopbackUDP(t), selfSignedTlsConfig(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		buf := make([]byte, 1)
		stream.Read(buf)
	}()

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { udpConn.Close() })

	quicTransport := &quic.Transport{Conn: udpConn}
	t.Cleanup(func() { quicTransport.Close() })

	clientTls := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", NextProtos: []string{"h3count"}}
	conn, err := quicTransport.Dial(ctx, listener.Addr(), clientTls, nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Write([]byte("x"))

	return &h3ConnStream{
		conn:          conn,
		stream:        stream,
		quicTransport: quicTransport,
		packetConn:    udpConn,
	}, udpConn
}

func socketIsClosed(udpConn *net.UDPConn) bool {
	_, err := udpConn.WriteTo([]byte{0}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	return errors.Is(err, net.ErrClosed)
}

// Ending the connection must release the socket the H3 transport opened,
// otherwise every reconnect leaks one udp fd and its read goroutine.
func TestH3ConnStreamCloseReleasesTheSocket(t *testing.T) {
	cs, udpConn := dialLoopbackH3(t)
	if socketIsClosed(udpConn) {
		t.Fatal("socket closed before the connection ended")
	}
	cs.close()
	if !socketIsClosed(udpConn) {
		t.Fatal("socket still open after the H3 connection ended: every reconnect leaks a udp socket")
	}
}

// Pins the original bug so the test above is known to discriminate: closing only
// the connection, which is all runH3 used to do, leaves the socket open because
// quic-go does not close the socket of a transport that is not single use.
func TestClosingOnlyTheQuicConnLeavesTheSocketOpen(t *testing.T) {
	cs, udpConn := dialLoopbackH3(t)
	cs.conn.CloseWithError(0, "")
	// give the transport time to react to the closed connection
	time.Sleep(200 * time.Millisecond)
	if socketIsClosed(udpConn) {
		t.Fatal("quic-go now closes the socket itself: the explicit close in h3ConnStream may be redundant")
	}
}
