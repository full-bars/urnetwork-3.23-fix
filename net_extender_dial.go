package connect

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/urnetwork/connect/protocol"
)

// net_extender_dial.go -- the carrier dials of the extender client (EXTENDER.md
// A3, A10, B3). One request over one carrier: tcp 443 terminated TLS, udp 443
// QUIC with ALPN h3, or the dns carrier, the same QUIC over the dns packet
// translation on the ports the config races (net_extender_dns_ports.go).
//
// The ported carrier layer is the upstream one, less the camouflaged tcp
// attempt (net_extender_camouflage.go): a config that carries a reality key
// dials the legacy attempt alone. Everything else -- the record's identity key
// check on the outer leaf (B3), the carrier memory reservations, the bounded
// request write and response read, and the stream the caller keeps -- is the
// upstream behavior.

func newExtenderDialContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
) DialContextFunction {
	// one outer config per dialer, so its session cache is not shared with
	// any other egress path
	extenderTlsConfig := newExtenderTlsConfig(extenderConfig)
	return func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		if network != "tcp" {
			panic(fmt.Errorf("Extender only support tcp network."))
		}

		host, portStr, err := net.SplitHostPort(address)
		if err != nil {
			panic(err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			panic(err)
		}

		// the outer name is the dialer's spoof name, and nothing at all with
		// an empty spoof list (A10). The destination is deliberately not
		// substituted: it is the operator name this whole layer exists to keep
		// out of the outer ClientHello
		serverConn, _, err := dialExtenderStream(
			ctx,
			connectSettings,
			extenderConfig,
			&ExtenderDial{
				DestinationHost: host,
				DestinationPort: port,
			},
			extenderTlsConfig,
		)
		if err != nil {
			return nil, err
		}
		return serverConn, nil
	}
}

// The tls dial is the plain dial with the inner handshake on top: the extender
// relays the inner stream verbatim, so there is nothing tls-specific about
// reaching the destination, only about what is spoken once it is reached.
func newExtenderDialTlsContext(
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	nextProtos []string,
) DialTlsContextFunction {
	dialContext := newExtenderDialContext(connectSettings, extenderConfig)
	innerBaseTlsConfig := newClientTlsConfig(connectSettings.TlsConfig, nextProtos)
	return func(
		ctx context.Context,
		network string,
		address string,
	) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			panic(err)
		}

		serverConn, err := dialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}

		innerTlsConfig := innerBaseTlsConfig.Clone()
		if innerTlsConfig.ServerName == "" {
			innerTlsConfig.ServerName = host
		}
		tlsServerConn := tls.Client(serverConn, innerTlsConfig)

		// inner handshake; bound the timeout so a slow/malicious extender cannot
		// hold the dial open indefinitely
		innerSuccess := false
		defer func() {
			if !innerSuccess {
				tlsServerConn.Close()
			}
		}()
		func() {
			innerCtx, innerCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer innerCancel()
			err = tlsServerConn.HandshakeContext(innerCtx)
		}()
		if err != nil {
			return nil, err
		}
		innerSuccess = true

		return tlsServerConn, nil
	}
}

// DialExtender opens one carrier, performs the extender request and returns
// the raw stream that follows the response. The caller owns the returned
// connection, which for the udp carriers also owns the QUIC connection and its
// socket. The gossip and feed clients use this directly; an ordinary dial goes
// through the tls dialer, which runs the inner TLS on top.
func DialExtender(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	return dialExtenderStream(
		ctx,
		connectSettings,
		extenderConfig,
		extenderDial,
		newExtenderTlsConfig(extenderConfig),
	)
}

// The outer TLS configuration of one extender dialer. The spoof name is
// presented as the sni, an empty name presents no sni at all (A10), the
// self-signed leaf is never checked against a root, and 1.3 is required so the
// certificate is encrypted on the wire.
func newExtenderTlsConfig(extenderConfig *ExtenderConfig) *tls.Config {
	tlsConfig := newClientTlsConfig(&tls.Config{
		ServerName:         extenderConfig.Profile.ServerName,
		InsecureSkipVerify: true,
		// require 1.3 to mask self-signed certs
		MinVersion: tls.VersionTLS13,
	}, nil)
	if 0 < len(extenderConfig.PublicKey) {
		tlsConfig.VerifyPeerCertificate = newExtenderLeafVerifier(extenderConfig.PublicKey)
	}
	return tlsConfig
}

// The B3 outer check: the presented leaf must be signed by the extender
// identity key from the record. No chain is built and no root is consulted,
// because the extender certificate is issued for a name it does not own.
func newExtenderLeafVerifier(extenderPublicKey []byte) func([][]byte, [][]*x509.Certificate) error {
	publicKey := ed25519.PublicKey(append([]byte(nil), extenderPublicKey...))
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(publicKey) != ed25519.PublicKeySize {
			return fmt.Errorf("extender public key is not an ed25519 key")
		}
		if len(rawCerts) == 0 {
			return fmt.Errorf("extender presented no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		if leaf.SignatureAlgorithm != x509.PureEd25519 {
			return fmt.Errorf("extender leaf is signed with %s, expected ed25519", leaf.SignatureAlgorithm)
		}
		if !ed25519.Verify(publicKey, leaf.RawTBSCertificate, leaf.Signature) {
			return fmt.Errorf("extender leaf is not signed by the record key")
		}
		return nil
	}
}

// Establishes the carrier, sends the A3 request and reads the response. On
// success the returned connection is positioned at the first byte after the
// response.
func dialExtenderStream(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
	extenderTlsConfig *tls.Config,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if extenderConfig.Profile.ConnectMode == ExtenderConnectModeWebRtc {
		// the webrtc carrier is a later phase (EXTENDER.md S); the records and
		// directory layer names it, but no dial builds it here
		return nil, nil, fmt.Errorf("the %s carrier is not built", ExtenderConnectModeWebRtc)
	}

	headerBytes, err := extenderRequestHeaderBytes(extenderConfig, extenderDial)
	if err != nil {
		return nil, nil, err
	}

	switch extenderConfig.Profile.ConnectMode {
	case ExtenderConnectModeTcpTls:
		return dialExtenderTcp(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, extenderDial.RoundTrip)
	case ExtenderConnectModeQuic:
		return dialExtenderQuic(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, extenderDial.RoundTrip)
	case ExtenderConnectModeDns:
		return dialExtenderDns(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, extenderDial.RoundTrip)
	default:
		return nil, nil, fmt.Errorf("bad connect mode %s", extenderConfig.Profile.ConnectMode)
	}
}

// The serialized request header, with the hmac over timestamp and nonce when
// the extender is private.
func extenderRequestHeaderBytes(
	extenderConfig *ExtenderConfig,
	extenderDial *ExtenderDial,
) ([]byte, error) {
	header := &protocol.ExtenderHeader{
		DestinationHost:        extenderDial.DestinationHost,
		DestinationPort:        uint32(extenderDial.DestinationPort),
		Timestamp:              uint64(time.Now().UnixMilli()),
		Challenge:              extenderDial.Challenge,
		Service:                extenderDial.Service,
		Datagram:               extenderDial.Datagram,
		ProbeClientId:          extenderDial.ProbeClientId,
		ProbeExtenderPublicKey: extenderDial.ProbeExtenderPublicKey,
		HopCount:               extenderDial.HopCount,
	}
	if extenderConfig.Secret != "" {
		nonce := NewId()
		header.Nonce = nonce.Bytes()

		mac := hmac.New(sha256.New, []byte(extenderConfig.Secret))
		timestampBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(timestampBytes[0:8], header.Timestamp)
		mac.Write(timestampBytes)
		mac.Write(header.Nonce)
		header.Signature = mac.Sum(nil)
	}

	headerMessageBytes, err := ProtoMarshal(header)
	if err != nil {
		return nil, err
	}
	defer MessagePoolReturn(headerMessageBytes)
	if ExtenderMaxHeaderByteCount < len(headerMessageBytes) {
		return nil, fmt.Errorf("extender header is %d bytes, at most %d", len(headerMessageBytes), ExtenderMaxHeaderByteCount)
	}
	return append([]byte(nil), headerMessageBytes...), nil
}

// The A3 request. The body is written separately on the udp carriers, where
// the request stream carries it as http3 DATA frames, so the body is optional
// here while the content length is not.
func newExtenderRequest(
	extenderConfig *ExtenderConfig,
	headerBytes []byte,
	body io.ReadCloser,
) *http.Request {
	authorityHost := extenderConfig.Ip.String()
	if !extenderConfig.Ip.IsValid() {
		// the webrtc carrier has no ip: the request rides an already
		// authenticated data channel, and the extender reads only the
		// method, path and content type of it (A3)
		authorityHost = ExtenderCarrierForConnectMode(extenderConfig.Profile.ConnectMode)
	}
	authority := net.JoinHostPort(
		authorityHost,
		strconv.Itoa(extenderConfig.Profile.Port),
	)
	requestHost := extenderConfig.Profile.ServerName
	if requestHost == "" {
		requestHost = authority
	}
	return &http.Request{
		Method: http.MethodPost,
		URL: &url.URL{
			Scheme: "https",
			Host:   authority,
			Path:   "/",
		},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       requestHost,
		Header: http.Header{
			"Content-Type": []string{ExtenderContentType},
		},
		Body:          body,
		ContentLength: int64(len(headerBytes)),
	}
}

// The tcp carrier: terminated outer TLS, then one HTTP/1.1 request. The
// buffered reader that read the response is kept, because it may already hold
// the first bytes the extender sent after it.
//
// One tcp carrier memory reservation covers the whole dial, held once however
// the dial reaches the extender (P8), and wrapped around the attempt that
// answered so a failed attempt neither holds the address nor spends a second
// carrier slot.
func dialExtenderTcp(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	reservation, err := acquireExtenderTcpMemory(ctx)
	if err != nil {
		return nil, nil, err
	}
	owned := false
	defer func() {
		if !owned {
			reservation.Release()
		}
	}()

	streamConn, response, err := dialExtenderTcpLegacyConn(ctx, connectSettings, extenderConfig, extenderTlsConfig, headerBytes, roundTrip)
	if err != nil {
		return nil, nil, err
	}

	owned = true
	return &extenderBudgetConn{
		Conn:        streamConn,
		reservation: reservation,
	}, response, nil
}

// The legacy tcp attempt: today's Go-TLS dial to the extender ip, terminated
// outer TLS, then the A3 exchange. It returns the raw stream; the carrier's one
// memory reservation is wrapped around the winning attempt by dialExtenderTcp,
// so this neither acquires nor releases it.
func dialExtenderTcpLegacyConn(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	authority := net.JoinHostPort(
		extenderConfig.Ip.String(),
		strconv.Itoa(extenderConfig.Profile.Port),
	)

	// Deliberately NOT routed through dialControlTlsWithFamilyFallback,
	// unlike the normal and resilient dialers. `authority` is built
	// from extenderConfig.Ip, a netip.Addr, so it is always an IP
	// LITERAL: the family is fixed by the address, there is no other
	// family to retry onto -- `dial tcp6 1.1.1.1:443` is "no suitable
	// address found" -- and there is no name resolution whose family
	// choice a strike could inform. controlDialNetwork leaves literal
	// dials unnarrowed for the same reason, which is what keeps this
	// whole fallback layer alive under a demotion.
	conn, err := connectSettings.DialContext(ctx, "tcp", authority)
	if err != nil {
		return nil, nil, err
	}
	// close the underlying conn on any failure path before we return a
	// successful serverConn to the caller
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	var serverConn net.Conn
	if extenderConfig.Profile.Fragment || extenderConfig.Profile.Reorder {
		rconn := NewResilientTlsConn(conn, extenderConfig.Profile.Fragment, extenderConfig.Profile.Reorder)
		tlsServerConn := tls.Client(rconn, extenderTlsConfig)

		func() {
			tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer tlsCancel()
			err = tlsServerConn.HandshakeContext(tlsCtx)
		}()
		if err != nil {
			return nil, nil, err
		}
		// once the stream is established, no longer need the resilient features
		if err := withConnWritePhaseDeadline(ctx, rconn, connectSettings.ConnectTimeout, rconn.Off); err != nil {
			return nil, nil, err
		}

		serverConn = tlsServerConn
	} else {
		tlsServerConn := tls.Client(conn, extenderTlsConfig)

		func() {
			tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
			defer tlsCancel()
			err = tlsServerConn.HandshakeContext(tlsCtx)
		}()
		if err != nil {
			return nil, nil, err
		}

		serverConn = tlsServerConn
	}

	streamConn, response, err := extenderStreamRequest(
		ctx,
		connectSettings,
		extenderConfig,
		serverConn,
		headerBytes,
		roundTrip,
	)
	if err != nil {
		return nil, nil, err
	}

	success = true
	return streamConn, response, nil
}

// The A3 exchange of the stream carriers on an established, terminated stream:
// one HTTP/1.1 request carrying the header, then the response frame. The tcp
// carrier runs it after its outer tls, so a refusal, a limit and an answer come
// from exactly the same extender handler. On success the returned connection is
// positioned at the first byte after the response; it keeps the buffered
// reader, which may already hold the first bytes the extender sent after the
// response. On an error the caller still owns serverConn.
func extenderStreamRequest(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	serverConn net.Conn,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	request := newExtenderRequest(
		extenderConfig,
		headerBytes,
		io.NopCloser(bytes.NewReader(headerBytes)),
	)
	if err := withConnWritePhaseDeadline(ctx, serverConn, connectSettings.ConnectTimeout, func() error {
		roundTrip.markSend()
		return request.Write(serverConn)
	}); err != nil {
		return nil, nil, err
	}

	// the reader may buffer past the response; it becomes the read side of
	// the returned connection
	headerReader := &extenderResponseHeaderReader{reader: serverConn, remaining: ExtenderMaxHeaderByteCount}
	reader := bufio.NewReader(headerReader)
	var response *protocol.ExtenderResponse
	if err := withConnReadPhaseDeadline(ctx, serverConn, connectSettings.ConnectTimeout, func() error {
		httpResponse, err := http.ReadResponse(reader, request)
		if err != nil {
			return err
		}
		headerReader.remaining = -1
		if httpResponse.StatusCode == http.StatusTooManyRequests {
			return &ExtenderLimitedError{RetryAfter: extenderRetryAfter(httpResponse.Header, time.Now())}
		}
		if httpResponse.StatusCode != http.StatusOK {
			return &ExtenderRefusedError{StatusCode: httpResponse.StatusCode}
		}
		// A stream extender sends exactly one bounded, length-delimited frame
		// before handing the connection over. Reject chunked bodies/trailers:
		// Body.Close would otherwise parse unbounded trailer MIME headers.
		if len(httpResponse.TransferEncoding) != 0 || httpResponse.ContentLength < 4 || httpResponse.ContentLength > 4+ExtenderMaxHeaderByteCount {
			return fmt.Errorf("extender response must have a bounded content length")
		}
		defer httpResponse.Body.Close()
		response, err = ReadExtenderResponseFrame(httpResponse.Body)
		if err == nil {
			roundTrip.markReceive()
		}
		return err
	}); err != nil {
		return nil, nil, err
	}
	return newBufferedConn(serverConn, reader), response, nil
}

// The udp carriers: one QUIC connection with ALPN h3 straight to the extender
// ip, the dns carrier over the packet translation first, then one H3 request
// stream that becomes the byte stream. It dials Profile.Port alone; the dns
// carrier's ports are raced over it by dialExtenderDns.
func dialExtenderQuic(
	ctx context.Context,
	connectSettings *ConnectSettings,
	extenderConfig *ExtenderConfig,
	extenderTlsConfig *tls.Config,
	headerBytes []byte,
	roundTrip *ExtenderRoundTrip,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if !extenderConfig.Ip.IsValid() {
		return nil, nil, fmt.Errorf("extender address is not valid")
	}
	udpAddr := net.UDPAddrFromAddrPort(netip.AddrPortFrom(
		extenderConfig.Ip,
		uint16(extenderConfig.Profile.Port),
	))

	policy := newExtenderQuicMemoryPolicy(ctx, connectSettings)
	var ptSettings *PacketTranslationSettings
	if extenderConfig.Profile.ConnectMode == ExtenderConnectModeDns {
		ptSettings = policy.packetTranslationSettings()
	}
	reservation, err := policy.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Reverse-order cleanup releases the claim only after the connection,
	// QUIC transport, translation and raw socket have all stopped owning bytes.
	closers := []func(){reservation.Release}
	success := false
	defer func() {
		if !success {
			for i := len(closers) - 1; 0 <= i; i -= 1 {
				closers[i]()
			}
		}
	}()

	packetConn, err := openExtenderPacketConn(ctx, connectSettings, udpAddr)
	if err != nil {
		// ownership transfers for every non-nil result, including a
		// rejected one
		if packetConn != nil {
			packetConn.Close()
		}
		return nil, nil, err
	}
	if packetConn == nil {
		return nil, nil, fmt.Errorf("extender packet connection factory returned nil")
	}
	packetConn = capPlatformPacketConn(packetConn, policy.readBufferByteCount, policy.writeBufferByteCount)
	closers = append(closers, func() { packetConn.Close() })

	if extenderConfig.Profile.ConnectMode == ExtenderConnectModeDns {
		tld := extenderConfig.Profile.DnsTld
		if tld == "" {
			tld = DefaultExtenderDnsTld
		}
		ptSettings.Log = connectSettings.Log
		ptSettings.DnsTlds = [][]byte{[]byte(tld)}
		// The connection cleanup owns the translated PacketConn. Keep its
		// encoder alive while cancellation closes QUIC gracefully, exactly as
		// the platform h3 dns carrier does.
		translation, err := NewPacketTranslation(
			context.WithoutCancel(ctx),
			PacketTranslationModeDns,
			packetConn,
			ptSettings,
		)
		if err != nil {
			return nil, nil, err
		}
		packetConn = translation
		closers = append(closers, func() { translation.Close() })
	}

	quicTransport := &quic.Transport{
		Conn: packetConn,
	}
	closers = append(closers, func() { quicTransport.Close() })

	// with an empty spoof list the outer name is empty (A10). quic-go fills an
	// empty ServerName with the ip literal it is dialing, and crypto/tls omits
	// an ip literal from the sni extension, so the ClientHello still carries no
	// name -- the same shape the tcp carrier sends
	quicTlsConfig := extenderTlsConfig.Clone()
	quicTlsConfig.NextProtos = []string{http3.NextProtoH3}
	installQuicSendFlight(policy.quicConfig)
	quicConn, err := quicTransport.Dial(ctx, udpAddr, quicTlsConfig, policy.quicConfig)
	if err != nil {
		return nil, nil, err
	}
	closers = append(closers, func() { quicConn.CloseWithError(0, "") })
	flight := quicSendFlightForConn(quicConn)
	flight.bind(quicConn)

	h3Transport := &http3.Transport{MaxResponseHeaderBytes: ExtenderMaxHeaderByteCount}
	clientConn := h3Transport.NewClientConn(quicConn)
	stream, err := clientConn.OpenRequestStream(ctx)
	if err != nil {
		return nil, nil, err
	}
	writer := flight.newWriter(stream)

	// http3 rejects a request stream header that carries a body, so the body
	// is written as DATA frames after it; the content length still describes
	// it
	request := newExtenderRequest(extenderConfig, headerBytes, http.NoBody)
	deadline := time.Now().Add(connectSettings.ConnectTimeout)
	if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if err := stream.SetDeadline(deadline); err != nil {
		return nil, nil, err
	}
	if err := writer.SetWriteDeadline(deadline); err != nil {
		return nil, nil, err
	}
	roundTrip.markSend()
	if err := stream.SendRequestHeader(request); err != nil {
		return nil, nil, err
	}
	if _, err := writer.Write(headerBytes); err != nil {
		return nil, nil, err
	}
	httpResponse, err := stream.ReadResponse()
	if err != nil {
		return nil, nil, err
	}
	if httpResponse.StatusCode == http.StatusTooManyRequests {
		return nil, nil, &ExtenderLimitedError{RetryAfter: extenderRetryAfter(httpResponse.Header, time.Now())}
	}
	if httpResponse.StatusCode != http.StatusOK {
		return nil, nil, &ExtenderRefusedError{StatusCode: httpResponse.StatusCode}
	}
	response, err := ReadExtenderResponseFrame(stream)
	if err != nil {
		return nil, nil, err
	}
	roundTrip.markReceive()
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, nil, err
	}
	if err := writer.SetWriteDeadline(time.Time{}); err != nil {
		return nil, nil, err
	}

	success = true
	return newStreamConn(
		stream,
		writer,
		packetConn.LocalAddr(),
		udpAddr,
		closers,
	), response, nil
}

// continue as the inner tunnel's read side. All parser roots fit the outer
// H1 claim; at most the 1-KiB wire header plus the fixed bufio buffer is read.
type extenderResponseHeaderReader struct {
	reader    io.Reader
	remaining int
}

func (self *extenderResponseHeaderReader) Read(b []byte) (int, error) {
	if self.remaining == 0 {
		return 0, fmt.Errorf("extender HTTP response headers exceed %d bytes", ExtenderMaxHeaderByteCount)
	}
	if self.remaining > 0 {
		b = b[:min(len(b), self.remaining)]
	}
	n, err := self.reader.Read(b)
	if self.remaining > 0 {
		self.remaining -= n
	}
	return n, err
}

// One unconnected udp endpoint for a carrier dial, narrowed to the family of
// the extender address. A configured packet endpoint factory wins, so a
// headless host keeps one source identity.
func openExtenderPacketConn(
	ctx context.Context,
	connectSettings *ConnectSettings,
	udpAddr *net.UDPAddr,
) (net.PacketConn, error) {
	if connectSettings.DialContextSettings != nil && connectSettings.DialContextSettings.PacketConnFactory != nil {
		return connectSettings.DialContextSettings.PacketConnFactory(ctx)
	}
	udpNetwork, wildcard := udpWildcardForFamily(udpAddrFamily(udpAddr))
	return net.ListenUDP(udpNetwork, wildcard)
}

// bufferedConn is the stream plus the reader that read the response, so bytes
// the reader already pulled past the response are not lost.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedConn(conn net.Conn, reader *bufio.Reader) *bufferedConn {
	return &bufferedConn{
		Conn:   conn,
		reader: reader,
	}
}

func (self *bufferedConn) Read(b []byte) (int, error) {
	return self.reader.Read(b)
}

// streamConn adapts one http3 request stream to a connection. It owns the
// QUIC connection, transport and socket underneath, which are released in
// reverse order on Close, so a caller that holds only the stream still frees
// the whole carrier.
type streamConn struct {
	stream     *http3.RequestStream
	writer     *quicSendFlightWriter
	localAddr  net.Addr
	remoteAddr net.Addr
	closers    []func()
	closeOnce  sync.Once
}

func newStreamConn(
	stream *http3.RequestStream,
	writer *quicSendFlightWriter,
	localAddr net.Addr,
	remoteAddr net.Addr,
	closers []func(),
) *streamConn {
	return &streamConn{
		stream:     stream,
		writer:     writer,
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
		closers:    closers,
	}
}

func (self *streamConn) Read(b []byte) (int, error) {
	return self.stream.Read(b)
}

func (self *streamConn) Write(b []byte) (int, error) {
	return self.writer.Write(b)
}

func (self *streamConn) Close() error {
	self.closeOnce.Do(func() {
		self.stream.Close()
		self.stream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
		for i := len(self.closers) - 1; 0 <= i; i -= 1 {
			self.closers[i]()
		}
	})
	return nil
}

func (self *streamConn) LocalAddr() net.Addr {
	return self.localAddr
}

func (self *streamConn) RemoteAddr() net.Addr {
	return self.remoteAddr
}

func (self *streamConn) SetDeadline(t time.Time) error {
	if err := self.writer.SetWriteDeadline(t); err != nil {
		return err
	}
	return self.stream.SetReadDeadline(t)
}

func (self *streamConn) SetReadDeadline(t time.Time) error {
	return self.stream.SetReadDeadline(t)
}

func (self *streamConn) SetWriteDeadline(t time.Time) error {
	return self.writer.SetWriteDeadline(t)
}
