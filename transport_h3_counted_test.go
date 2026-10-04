package connect

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"golang.org/x/net/ipv4"
)

// quic-go only keeps ECN, the DF bit and batching if the conn it is given still
// satisfies OOBCapablePacketConn, and only reads through the wrapper if it also
// provides ReadBatch. Losing either silently costs throughput or the receive
// count, so both are pinned at compile time.
var _ quic.OOBCapablePacketConn = (*countedUDPConn)(nil)
var _ interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
} = (*countedUDPConn)(nil)

func listenLoopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// Each read and write method credits the right counter by exactly the bytes it
// moved, and none of them credits the other direction.
func TestCountedUDPConnCountsEveryReadAndWritePath(t *testing.T) {
	bw := &ProxyBandwidth{}
	counted := newCountedUDPConn(listenLoopbackUDP(t), bw)
	peer := listenLoopbackUDP(t)
	countedAddr := counted.LocalAddr().(*net.UDPAddr)
	peerAddr := peer.LocalAddr().(*net.UDPAddr)

	// writes
	if _, err := counted.WriteTo(make([]byte, 100), peerAddr); err != nil {
		t.Fatal(err)
	}
	if _, _, err := counted.WriteMsgUDP(make([]byte, 50), nil, peerAddr); err != nil {
		t.Fatal(err)
	}
	if got := bw.TotalTx.Load(); got != 150 {
		t.Fatalf("TotalTx = %d, want 150", got)
	}
	if got := bw.TotalRx.Load(); got != 0 {
		t.Fatalf("writes credited %d to TotalRx", got)
	}

	// reads: one packet per read path
	send := func(n int) {
		if _, err := peer.WriteToUDP(make([]byte, n), countedAddr); err != nil {
			t.Fatal(err)
		}
	}
	buf := make([]byte, 2048)
	counted.SetReadDeadline(time.Now().Add(2 * time.Second))

	send(10)
	if _, _, err := counted.ReadFrom(buf); err != nil {
		t.Fatal(err)
	}
	send(20)
	if _, _, _, _, err := counted.ReadMsgUDP(buf, nil); err != nil {
		t.Fatal(err)
	}
	send(30)
	msgs := []ipv4.Message{{Buffers: [][]byte{make([]byte, 2048)}}}
	if n, err := counted.ReadBatch(msgs, 0); err != nil || n != 1 {
		t.Fatalf("ReadBatch n=%d err=%v", n, err)
	}
	if got := bw.TotalRx.Load(); got != 60 {
		t.Fatalf("TotalRx = %d, want 60 (10 ReadFrom + 20 ReadMsgUDP + 30 ReadBatch)", got)
	}
	if got := bw.TotalTx.Load(); got != 150 {
		t.Fatalf("reads changed TotalTx to %d", got)
	}
}

func selfSignedTlsConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		NextProtos:   []string{"h3count"},
	}
}

// The real risk: quic-go reads through ipv4.NewPacketConn, which unwraps the file
// descriptor and bypasses a plain wrapper, so a naive wrapper would count what
// the transport sends and never what it receives. Drive an actual handshake and
// a stream round trip through the wrapped client socket and require both
// directions to be counted.
func TestCountedUDPConnCountsARealQuicSession(t *testing.T) {
	serverTls := selfSignedTlsConfig(t)
	listener, err := quic.Listen(listenLoopbackUDP(t), serverTls, nil)
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
		buf := make([]byte, 5)
		if _, err := stream.Read(buf); err == nil {
			stream.Write(buf)
		}
	}()

	bw := &ProxyBandwidth{}
	counted := newCountedUDPConn(listenLoopbackUDP(t), bw)
	transport := &quic.Transport{Conn: counted}
	t.Cleanup(func() { transport.Close() })

	clientTls := &tls.Config{InsecureSkipVerify: true, ServerName: "localhost", NextProtos: []string{"h3count"}}
	conn, err := transport.Dial(ctx, listener.Addr(), clientTls, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 5)
	if _, err := stream.Read(echo); err != nil {
		t.Fatal(err)
	}

	if tx := bw.TotalTx.Load(); tx < 1200 {
		t.Fatalf("TotalTx = %d after a QUIC handshake, want at least one initial packet (1200)", tx)
	}
	if rx := bw.TotalRx.Load(); rx == 0 {
		t.Fatal("TotalRx = 0 after a QUIC handshake and echo: the receive path bypasses the wrapper")
	}
}

// The transport only wraps when it has a bandwidth record to credit; otherwise
// the plain socket is used unchanged.
func TestCountH3SocketWrapsOnlyWithABandwidthRecord(t *testing.T) {
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)

	transport := &PlatformTransport{
		clientStrategy: &ClientStrategy{settings: DefaultClientStrategySettings()},
	}
	udp := listenLoopbackUDP(t)

	if _, wrapped := transport.countH3Socket(udp).(*countedUDPConn); wrapped {
		t.Fatal("wrapped with no bandwidth record registered")
	}

	RegisterProxy(0, "direct", "direct")
	RegisterProxyBandwidth(0)
	if _, wrapped := transport.countH3Socket(udp).(*countedUDPConn); !wrapped {
		t.Fatal("did not wrap although the identity has a bandwidth record")
	}
}
