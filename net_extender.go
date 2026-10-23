package connect

import (
	"net/http"
	"net/netip"
)

// an extender uses an independent url that is hard-coded to forward to the platform
// the `platformUrl` here must match the hard coded url in the extender, which is
// done by using a prior vetted extender
// The connection to the platform is end-to-end encrypted with TLS,
// using the hostname from `platformUrl`

type ExtenderConnectMode string

const (
	ExtenderConnectModeTcpTls ExtenderConnectMode = "tcptls"
	// quic and dns carriers: declared so the ported platform transport compiles.
	// This tree's extender dialers only produce tcptls, so no config carries
	// these modes (stage 3, upstream 20b29f79).
	ExtenderConnectModeQuic ExtenderConnectMode = "quic"
	ExtenderConnectModeDns  ExtenderConnectMode = "dns"
	// The peer-to-peer webrtc carrier (EXTENDER.md S, C2a). Not an ip:port
	// dial: the extender is reached through the exchange signaling and
	// ICE/STUN, by the rendezvous id its record carries. Declared so the
	// records/directory layer can name it; the carrier itself is a later
	// phase.
	ExtenderConnectModeWebRtc ExtenderConnectMode = "webrtc"
	// TODO
	// ExtenderConnectModeUdp ExtenderConnectMode = "udp"
)

// extenders will do a TLS connection using the given server name to the given port
// comparable
type ExtenderProfile struct {
	ConnectMode ExtenderConnectMode
	ServerName  string
	Port        int
	Fragment    bool
	Reorder     bool
	// DnsTld is the encoding tld of the dns carrier only (A1, L2).
	DnsTld string
}

type ExtenderConfig struct {
	Profile ExtenderProfile
	Ip      netip.Addr
	Secret  string
	// PublicKey, when set, is the extender identity key from a verified
	// record. The outer leaf certificate must be signed by it (B3). Empty
	// keeps the unauthenticated outer TLS of a manually configured extender.
	// The carrier that reads it is a later phase; the directory and the feed
	// config still carry it.
	PublicKey []byte
	// DnsPorts are the ports one dns carrier dial races, in launch order
	// (L2), with Profile.Port the first of them. A config drawn from a
	// directory candidate carries the ports its record lists (and the
	// directory's candidate adds whichever of 4053 and 53 the record does not).
	// Empty dials Profile.Port alone.
	DnsPorts []int
}

func NewExtenderHttpClient(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) *http.Client {
	transport := &http.Transport{
		DialTLSContext:    NewExtenderDialTlsContext(connectSettings, extenderConfig),
		ForceAttemptHTTP2: true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   connectSettings.RequestTimeout,
	}
}

// client

// http client can plug in DialContext as part of Transport
// https://cs.opensource.google/go/go/+/refs/tags/go1.22.4:src/net/http/transport.go;l=95

// create a tls connect to (destinationHost, destinationPort) on the connection
// returned by this
// the returned connection is not a tls connection
func NewExtenderDialTlsContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialTlsContextFunction {
	return newExtenderDialTlsContext(connectSettings, extenderConfig, nil)
}

/*
type streamConn struct {
	stream quic.Stream
}

func newStreamConn(stream quic.Stream) *streamConn {
	return &streamConn{
		stream: stream,
	}
}

func (self *streamConn) Read(b []byte) (int, error) {
	return self.stream.Read(b)
}

func (self *streamConn) Write(b []byte) (int, error) {
	return self.stream.Write(b)
}

func (self *streamConn) Close() error {
	return self.stream.Close()
}

func (self *streamConn) LocalAddr() net.Addr {
	return nil
}

func (self *streamConn) RemoteAddr() net.Addr {
	return nil
}

func (self *streamConn) SetDeadline(t time.Time) error {
	return self.stream.SetDeadline(t)
}

func (self *streamConn) SetReadDeadline(t time.Time) error {
	return self.stream.SetReadDeadline(t)
}

func (self *streamConn) SetWriteDeadline(t time.Time) error {
	return self.stream.SetWriteDeadline(t)
}
*/
