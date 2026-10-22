package extender

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The core slice of the port carries the server's tcp carrier but not the
// root's carrier client: connect.DialExtender is still the phase-1 stub that
// reports the carrier layer unbuilt. The tests that drive a v2 request
// therefore dial through extenderTestDial, which mirrors the pin's legacy tcp
// path -- outer tls with no alpn, one A3 request, one response frame -- so the
// server is exercised end to end. The quic and dns carriers return the same
// unbuilt error the root client does; the carrier-client slice replaces every
// call with connect.DialExtender again and deletes this file.

// extenderTestDial performs the A3 exchange over the tcp carrier, positioned
// on success at the first byte after the response.
func extenderTestDial(
	ctx context.Context,
	connectSettings *connect.ConnectSettings,
	extenderConfig *connect.ExtenderConfig,
	extenderDial *connect.ExtenderDial,
) (net.Conn, *protocol.ExtenderResponse, error) {
	if extenderConfig.Profile.ConnectMode != connect.ExtenderConnectModeTcpTls {
		return nil, nil, fmt.Errorf(
			"the test carrier client does not speak %s in this phase",
			extenderConfig.Profile.ConnectMode,
		)
	}
	headerBytes, err := extenderTestHeaderBytes(extenderConfig, extenderDial)
	if err != nil {
		return nil, nil, err
	}
	authority := net.JoinHostPort(
		extenderConfig.Ip.String(),
		strconv.Itoa(extenderConfig.Profile.Port),
	)
	conn, err := connectSettings.DialContext(ctx, "tcp", authority)
	if err != nil {
		return nil, nil, err
	}
	success := false
	defer func() {
		if !success {
			conn.Close()
		}
	}()

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         extenderConfig.Profile.ServerName,
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	})
	tlsCtx, tlsCancel := context.WithTimeout(ctx, connectSettings.TlsTimeout)
	err = tlsConn.HandshakeContext(tlsCtx)
	tlsCancel()
	if err != nil {
		return nil, nil, err
	}

	request := extenderTestRequest(extenderConfig, headerBytes)
	if err := request.Write(tlsConn); err != nil {
		return nil, nil, err
	}

	reader := bufio.NewReader(&extenderTestHeaderLimitReader{
		reader:    tlsConn,
		remaining: connect.ExtenderMaxHeaderByteCount,
	})
	httpResponse, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, nil, err
	}
	switch httpResponse.StatusCode {
	case http.StatusTooManyRequests:
		httpResponse.Body.Close()
		return nil, nil, &connect.ExtenderLimitedError{
			RetryAfter: extenderTestRetryAfter(httpResponse.Header, time.Now()),
		}
	case http.StatusOK:
	default:
		httpResponse.Body.Close()
		return nil, nil, &connect.ExtenderRefusedError{StatusCode: httpResponse.StatusCode}
	}
	if len(httpResponse.TransferEncoding) != 0 ||
		httpResponse.ContentLength < 4 ||
		4+int64(connect.ExtenderMaxHeaderByteCount) < httpResponse.ContentLength {
		httpResponse.Body.Close()
		return nil, nil, fmt.Errorf("extender response must have a bounded content length")
	}
	response, err := connect.ReadExtenderResponseFrame(httpResponse.Body)
	httpResponse.Body.Close()
	if err != nil {
		return nil, nil, err
	}

	success = true
	return &extenderTestBufferedConn{Conn: tlsConn, reader: reader}, response, nil
}

// extenderTestHeaderBytes is the serialized request header, with the hmac over
// timestamp and nonce when the extender is private.
func extenderTestHeaderBytes(
	extenderConfig *connect.ExtenderConfig,
	extenderDial *connect.ExtenderDial,
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
		nonce := connect.NewId()
		header.Nonce = nonce.Bytes()

		mac := hmac.New(sha256.New, []byte(extenderConfig.Secret))
		timestampBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(timestampBytes[0:8], header.Timestamp)
		mac.Write(timestampBytes)
		mac.Write(header.Nonce)
		header.Signature = mac.Sum(nil)
	}
	headerBytes, err := proto.Marshal(header)
	if err != nil {
		return nil, err
	}
	if connect.ExtenderMaxHeaderByteCount < len(headerBytes) {
		return nil, fmt.Errorf(
			"extender header is %d bytes, at most %d",
			len(headerBytes),
			connect.ExtenderMaxHeaderByteCount,
		)
	}
	return headerBytes, nil
}

// extenderTestRequest is the A3 request on the tcp carrier.
func extenderTestRequest(
	extenderConfig *connect.ExtenderConfig,
	headerBytes []byte,
) *http.Request {
	authority := net.JoinHostPort(
		extenderConfig.Ip.String(),
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
			"Content-Type": []string{connect.ExtenderContentType},
		},
		Body:          io.NopCloser(bytes.NewReader(headerBytes)),
		ContentLength: int64(len(headerBytes)),
	}
}

// extenderTestRetryAfter is the Retry-After of a 429, in either form RFC 9110
// allows, and zero when there is none.
func extenderTestRetryAfter(header http.Header, now time.Time) time.Duration {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(min(seconds, int64(24*60*60))) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, min(date.Sub(now), 24*time.Hour))
	}
	return 0
}

// extenderTestHeaderLimitReader bounds the response head the client reads, as
// the pin's carrier client does.
type extenderTestHeaderLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (self *extenderTestHeaderLimitReader) Read(b []byte) (int, error) {
	if self.remaining < 0 {
		return 0, fmt.Errorf("the extender response head is too long")
	}
	n, err := self.reader.Read(b)
	self.remaining -= int64(n)
	return n, err
}

// extenderTestBufferedConn keeps the buffered reader of the response, which
// may already hold the first bytes the extender sent after it.
type extenderTestBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (self *extenderTestBufferedConn) Read(b []byte) (int, error) {
	return self.reader.Read(b)
}
