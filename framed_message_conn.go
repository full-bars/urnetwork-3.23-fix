package connect

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// H1MessageConn is the binary-message surface used by the existing H1
// lifecycle. A FramedMessageConn accepts only BinaryMessage; it does not
// implement WebSocket opcodes, control frames, masking, or compression.
// Successful pooled reads are owned by the caller until MessagePoolReturn.
type H1MessageConn interface {
	Close() error
	WriteMessage(int, []byte) error
	ReadMessage() (int, []byte, error)
	NextReader() (int, io.Reader, error)
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	UnderlyingConn() net.Conn
}

// FramedMessageConn has one reader and one writer, which may run concurrently.
// Each stream has a fixed, negotiated wire format and explicit admission cap.
// The writer lazily owns 16 KiB scratch. Larger compact frames use Framer's
// split path; larger XL frames use a full frame-local temporary buffer that is
// released on return. Read state never retains an application message.
type FramedMessageConn struct {
	net.Conn
	framer    *Framer
	xl        *FramerXl
	maximum   int
	readLimit atomic.Int64
	storage   []byte
	body      framedMessageBody
	stats     *H1PlusStats
	progress  *h1PhysicalProgress
}

func NewFramedMessageConn(conn net.Conn, protocol string, maximum int, stats *H1PlusStats) (*FramedMessageConn, error) {
	if conn == nil || maximum < 0 || maximum > math.MaxInt-4 || !supportedFramedProtocol(protocol) ||
		(protocol == H1FramerProtocol && math.MaxUint16 < maximum) || uint64(maximum) > math.MaxUint32 {
		return nil, errors.New("invalid framed message connection settings")
	}
	c := &FramedMessageConn{Conn: conn, maximum: maximum, stats: stats}
	c.readLimit.Store(int64(maximum))
	if protocol == H1FramerProtocol {
		c.framer = NewFramer(DefaultFramerSettings(maximum))
	} else {
		c.xl = NewFramerXl(DefaultFramerXlSettings(maximum))
	}
	c.body.conn = c
	return c, nil
}

func (c *FramedMessageConn) UnderlyingConn() net.Conn { return c.Conn }

// Write delegates encoded bytes and counts actual stream calls, including
// the legacy split path of a compact frame larger than its caller storage.
func (c *FramedMessageConn) Write(p []byte) (int, error) {
	if c.stats != nil {
		c.stats.writes.Add(1)
	}
	return c.Conn.Write(p)
}
func (c *FramedMessageConn) SetReadLimit(limit int64) {
	c.readLimit.Store(max(0, min(limit, int64(c.maximum))))
}

// RPC keeps its gomobile-compatible binary carrier interface. Framed streams
// use serialized zero-length binary heartbeats, never ping/pong emulation.
func (c *FramedMessageConn) SetPongHandler(func(string) error) {}
func (c *FramedMessageConn) WriteControl(int, []byte, time.Time) error {
	return errors.New("framed stream has no WebSocket control frames")
}

func (c *FramedMessageConn) NextReader() (int, io.Reader, error) {
	if c.body.remaining != 0 {
		if _, err := io.Copy(io.Discard, &c.body); err != nil {
			return 0, nil, err
		}
	}
	var length int
	var err error
	if c.xl != nil {
		length, err = c.xl.ReadHeader(c.Conn)
	} else {
		length, err = c.framer.ReadHeader(c.Conn)
	}
	if err == nil && c.readLimit.Load() < int64(length) {
		err = errors.New("framed stream message limit exceeded")
	}
	if err != nil {
		c.recordReadError(err)
		c.Conn.Close()
		return 0, nil, err
	}
	c.body.remaining = length
	return websocket.BinaryMessage, &c.body, nil
}

type framedMessageBody struct {
	conn      *FramedMessageConn
	remaining int
}

func (r *framedMessageBody) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p = p[:min(len(p), r.remaining)]
	n, err := r.conn.Conn.Read(p)
	r.remaining -= n
	if errors.Is(err, io.EOF) && r.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && !(errors.Is(err, io.EOF) && r.remaining == 0) {
		r.conn.recordReadError(err)
		r.conn.Close()
	}
	return n, err
}

func (c *FramedMessageConn) ReadPooledMessage() (int, []byte, error) {
	return c.readPooledMessage(c.readLimit.Load())
}

func (c *FramedMessageConn) readPooledMessage(limit int64) (int, []byte, error) {
	kind, _, err := c.NextReader()
	if err != nil {
		return kind, nil, err
	}
	if limit < int64(c.body.remaining) {
		c.Conn.Close()
		return kind, nil, errors.New("framed stream message limit exceeded")
	}
	message := MessagePoolGet(c.body.remaining)
	if _, err = io.ReadFull(&c.body, message); err != nil {
		MessagePoolReturn(message)
		return kind, nil, err
	}
	return kind, message, nil
}

// ReadMessage follows Gorilla's unpooled ownership convention. Hot paths use
// ReadH1PooledMessage or NextReader to avoid the extra copy in this convenience API.
func (c *FramedMessageConn) ReadMessage() (int, []byte, error) {
	kind, message, err := c.ReadPooledMessage()
	if err != nil {
		return kind, nil, err
	}
	defer MessagePoolReturn(message)
	return kind, append([]byte(nil), message...), nil
}

func ReadH1PooledMessage(conn H1MessageConn, limit int64) (int, []byte, error) {
	if framed, ok := conn.(*FramedMessageConn); ok {
		return framed.readPooledMessage(limit)
	}
	kind, reader, err := conn.NextReader()
	if err != nil || kind != websocket.BinaryMessage {
		return kind, nil, err
	}
	message, err := MessagePoolReadAllLimit(reader, limit)
	return kind, message, err
}

func (c *FramedMessageConn) WriteMessage(kind int, message []byte) error {
	if kind != websocket.BinaryMessage {
		return errors.New("framed stream requires a binary message")
	}
	return c.WriteMessages([][]byte{message})
}

// WriteMessages validates the complete batch before output and performs one
// payload copy into each bounded ready flush. The caller retains all inputs.
func (c *FramedMessageConn) WriteMessages(messages [][]byte) error {
	for _, message := range messages {
		if c.maximum < len(message) {
			return fmt.Errorf("framed stream message limit exceeded (%d)", c.maximum)
		}
	}
	if len(messages) == 0 {
		return nil
	}
	if c.storage == nil {
		c.storage = make([]byte, 16*1024)
	}
	for len(messages) != 0 {
		count, bytes := 1, len(messages[0])+4
		for count < len(messages) && bytes <= len(c.storage)-4 && len(messages[count]) <= len(c.storage)-bytes-4 {
			bytes += len(messages[count]) + 4
			count++
		}
		var err error
		if c.progress != nil {
			for _, message := range messages[:count] {
				c.progress.queue(message)
			}
			c.progress.beginWrite()
		}
		if c.framer != nil {
			err = c.framer.WriteBatchWithStorage(c, messages[:count], c.storage)
		} else {
			err = c.xl.WriteBatchWithStorage(c, messages[:count], c.storage)
		}
		c.progress.endWrite(err)
		if err != nil {
			if c.stats != nil {
				c.stats.writeErrors.Add(1)
			}
			c.Conn.Close()
			return err
		}
		if c.stats != nil {
			c.stats.flushes.Add(1)
			c.stats.messages.Add(uint64(count))
			c.stats.bytes.Add(uint64(bytes - 4*count))
		}
		messages = messages[count:]
	}
	return nil
}

func (c *FramedMessageConn) recordReadError(err error) {
	if c.stats != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		c.stats.readErrors.Add(1)
	}
}

// Bounded numeric counters shared by client/server experiments; there are no
// identity, URL, credential, payload, or peer-controlled metric labels.
type H1PlusStats struct {
	attempts, accepted, fallbacks, authFailures, handshakeNanos  atomic.Uint64
	flushes, writes, messages, bytes, readErrors, writeErrors    atomic.Uint64
	fallbackRejected, fallbackInvalid, fallbackIO, fallbackProxy atomic.Uint64
	webSocketSelected, handshakeFailures                         atomic.Uint64
	handshakeFailuresTimeout, handshakeFailuresTls               atomic.Uint64
	handshakeFailuresProxyConnect, handshakeFailuresAuth         atomic.Uint64
	handshakeFailuresUpgradeRejected, handshakeFailuresIO        atomic.Uint64
	handshakeFailuresOther, handshakeFailureNanos                atomic.Uint64
}
type H1PlusStatsSnapshot struct {
	Attempts, Accepted, Fallbacks, AuthFailures, HandshakeNanos  uint64
	Flushes, Writes, Messages, Bytes, ReadErrors, WriteErrors    uint64
	FallbackRejected, FallbackInvalid, FallbackIO, FallbackProxy uint64
	WebSocketSelected, HandshakeFailures                         uint64

	// HandshakeFailuresTimeout counts bounded handshakes that hit a dial,
	// TLS, or upgrade response deadline. Distinguishes deadline exhaustion
	// from explicit protocol refusals or stream resets.
	HandshakeFailuresTimeout uint64
	// HandshakeFailuresTls counts certificate validation errors, TLS alert
	// rejections, or malformed TLS record headers. Distinguishes cryptographic
	// or trust configuration failures from network dropouts.
	HandshakeFailuresTls uint64
	// HandshakeFailuresProxyConnect counts failures reaching the intermediate
	// proxy or proxy refusals during HTTP CONNECT tunnel establishment.
	// Distinguishes proxy hop failures from origin endpoint failures.
	HandshakeFailuresProxyConnect uint64
	// HandshakeFailuresAuth counts terminal HTTP 401 Unauthorized or 403
	// Forbidden responses during upgrade handshake. Distinguishes credential
	// rejections from other server refusals and protocol incompatibilities.
	HandshakeFailuresAuth uint64
	// HandshakeFailuresUpgradeRejected counts HTTP responses with status codes
	// other than 401/403 refusing the upgrade request. Distinguishes edge
	// gateway or server rejections (e.g. 502, 503, 400) from auth rejections.
	HandshakeFailuresUpgradeRejected uint64
	// HandshakeFailuresIO counts network read, write, unexpected EOF, closed
	// pipe, or connection reset errors on the underlying connection.
	// Distinguishes active stream drops from deadlines and HTTP refusals.
	HandshakeFailuresIO uint64
	// HandshakeFailuresOther counts failed bounded handshakes not classified into
	// any other bounded category. Keeps remaining failures in a real bucket
	// rather than silently dropping them.
	HandshakeFailuresOther uint64
	// HandshakeFailureNanos counts total nanoseconds elapsed across all failed
	// bounded handshakes. Enables reading the average duration of failed
	// handshakes beside avg_handshake.
	HandshakeFailureNanos uint64
}

func (s *H1PlusStats) Snapshot() H1PlusStatsSnapshot {
	if s == nil {
		return H1PlusStatsSnapshot{}
	}
	// The failure total is derived from the classes, never loaded beside them.
	// Recording increments the total and its class as two separate atomics, so
	// reading both could report failures=1 with an empty breakdown; summing the
	// classes the snapshot itself reports keeps the advertised invariant true
	// for every reader, including one that samples mid-record.
	failureTimeout := s.handshakeFailuresTimeout.Load()
	failureTls := s.handshakeFailuresTls.Load()
	failureProxyConnect := s.handshakeFailuresProxyConnect.Load()
	failureAuth := s.handshakeFailuresAuth.Load()
	failureUpgradeRejected := s.handshakeFailuresUpgradeRejected.Load()
	failureIO := s.handshakeFailuresIO.Load()
	failureOther := s.handshakeFailuresOther.Load()
	return H1PlusStatsSnapshot{
		Attempts: s.attempts.Load(), Accepted: s.accepted.Load(), Fallbacks: s.fallbacks.Load(), AuthFailures: s.authFailures.Load(), HandshakeNanos: s.handshakeNanos.Load(),
		Flushes: s.flushes.Load(), Writes: s.writes.Load(), Messages: s.messages.Load(), Bytes: s.bytes.Load(), ReadErrors: s.readErrors.Load(), WriteErrors: s.writeErrors.Load(),
		FallbackRejected: s.fallbackRejected.Load(), FallbackInvalid: s.fallbackInvalid.Load(), FallbackIO: s.fallbackIO.Load(), FallbackProxy: s.fallbackProxy.Load(),
		WebSocketSelected:                s.webSocketSelected.Load(),
		HandshakeFailures:                failureTimeout + failureTls + failureProxyConnect + failureAuth + failureUpgradeRejected + failureIO + failureOther,
		HandshakeFailuresTimeout:         failureTimeout,
		HandshakeFailuresTls:             failureTls,
		HandshakeFailuresProxyConnect:    failureProxyConnect,
		HandshakeFailuresAuth:            failureAuth,
		HandshakeFailuresUpgradeRejected: failureUpgradeRejected,
		HandshakeFailuresIO:              failureIO,
		HandshakeFailuresOther:           failureOther,
		HandshakeFailureNanos:            s.handshakeFailureNanos.Load(),
	}
}

// RecordH1PlusSelection records one completed bounded handshake result.
func RecordH1PlusSelection(stats *H1PlusStats, elapsed time.Duration, err error) {
	if stats == nil {
		return
	}
	stats.attempts.Add(1)
	stats.handshakeNanos.Add(uint64(max(0, elapsed)))
	if err == nil {
		stats.accepted.Add(1)
	} else if HTTPUpgradeAllowsFallback(err) {
		stats.fallbacks.Add(1)
		var upgradeErr *HTTPUpgradeError
		if errors.As(err, &upgradeErr) {
			switch upgradeErr.Reason {
			case "rejected":
				stats.fallbackRejected.Add(1)
			case "response-io":
				stats.fallbackIO.Add(1)
			case "http-proxy":
				stats.fallbackProxy.Add(1)
			default:
				stats.fallbackInvalid.Add(1)
			}
		}
	} else {
		stats.handshakeFailures.Add(1)
		stats.handshakeFailureNanos.Add(uint64(max(0, elapsed)))
		var upgradeErr *HTTPUpgradeError
		if errors.As(err, &upgradeErr) && (upgradeErr.StatusCode == 401 || upgradeErr.StatusCode == 403) {
			stats.authFailures.Add(1)
		}
		switch classifyH1PlusFailure(err) {
		case h1PlusFailureTimeout:
			stats.handshakeFailuresTimeout.Add(1)
		case h1PlusFailureTls:
			stats.handshakeFailuresTls.Add(1)
		case h1PlusFailureProxyConnect:
			stats.handshakeFailuresProxyConnect.Add(1)
		case h1PlusFailureAuth:
			stats.handshakeFailuresAuth.Add(1)
		case h1PlusFailureUpgradeRejected:
			stats.handshakeFailuresUpgradeRejected.Add(1)
		case h1PlusFailureIO:
			stats.handshakeFailuresIO.Add(1)
		case h1PlusFailureOther:
			stats.handshakeFailuresOther.Add(1)
		}
	}
}

type h1PlusFailureClass uint8

const (
	h1PlusFailureTimeout h1PlusFailureClass = iota
	h1PlusFailureTls
	h1PlusFailureProxyConnect
	h1PlusFailureAuth
	h1PlusFailureUpgradeRejected
	h1PlusFailureIO
	h1PlusFailureOther
)

func classifyH1PlusFailure(err error) h1PlusFailureClass {
	// A status the server actually sent is stronger evidence than a transport
	// error joined onto it: the upgrade path joins its cleanup and context
	// errors with the failure it was already returning, so a 401 followed by a
	// deadline is an auth rejection, not a timeout. Classification therefore
	// reads the HTTP evidence first and the transport causes after it.
	if isAuthFailure(err) {
		return h1PlusFailureAuth
	}
	if isUpgradeRejectedFailure(err) {
		return h1PlusFailureUpgradeRejected
	}
	if isTimeoutFailure(err) {
		return h1PlusFailureTimeout
	}
	if isTLSFailure(err) {
		return h1PlusFailureTls
	}
	if isProxyConnectFailure(err) {
		return h1PlusFailureProxyConnect
	}
	if isIOFailure(err) {
		return h1PlusFailureIO
	}
	return h1PlusFailureOther
}

func isTimeoutFailure(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func isTLSFailure(err error) bool {
	var (
		recHdrErr   *tls.RecordHeaderError
		alertVal    tls.AlertError
		alertPtr    *tls.AlertError
		certVerErr  *tls.CertificateVerificationError
		echErr      *tls.ECHRejectionError
		certInvVal  x509.CertificateInvalidError
		certInvPtr  *x509.CertificateInvalidError
		hostVal     x509.HostnameError
		hostPtr     *x509.HostnameError
		unkAuthVal  x509.UnknownAuthorityError
		unkAuthPtr  *x509.UnknownAuthorityError
		sysRootsVal x509.SystemRootsError
		sysRootsPtr *x509.SystemRootsError
		constrVal   x509.ConstraintViolationError
		constrPtr   *x509.ConstraintViolationError
		insecVal    x509.InsecureAlgorithmError
		insecPtr    *x509.InsecureAlgorithmError
	)
	if errors.As(err, &recHdrErr) ||
		errors.As(err, &alertVal) ||
		errors.As(err, &alertPtr) ||
		errors.As(err, &certVerErr) ||
		errors.As(err, &echErr) ||
		errors.As(err, &certInvVal) ||
		errors.As(err, &certInvPtr) ||
		errors.As(err, &hostVal) ||
		errors.As(err, &hostPtr) ||
		errors.As(err, &unkAuthVal) ||
		errors.As(err, &unkAuthPtr) ||
		errors.As(err, &sysRootsVal) ||
		errors.As(err, &sysRootsPtr) ||
		errors.As(err, &constrVal) ||
		errors.As(err, &constrPtr) ||
		errors.As(err, &insecVal) ||
		errors.As(err, &insecPtr) {
		return true
	}
	// Fallback to error message prefix when the Go crypto/tls package returns
	// untyped errors for protocol alerts or handshake negotiation failures.
	msg := err.Error()
	if strings.HasPrefix(msg, "tls: ") || strings.Contains(msg, ": tls: ") {
		return true
	}
	return false
}

func isProxyConnectFailure(err error) bool {
	var upgradeErr *HTTPUpgradeError
	if errors.As(err, &upgradeErr) && upgradeErr.Reason == "http-proxy" {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "proxyconnect" {
		return true
	}
	// This fork proxies through SOCKS5, not net/http's proxy, so a failed hop
	// arrives as the proxyDialError that ProxySettings.NewDialContext wraps
	// around it, never as a "proxyconnect" OpError. A hop that timed out is
	// still counted as a timeout, which is checked before this class.
	var proxyDialErr *proxyDialError
	if errors.As(err, &proxyDialErr) {
		return true
	}
	// Fallback to string matching when intermediate proxies or custom dialers
	// return untyped errors indicating proxy CONNECT rejection or reachability failure.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "proxyconnect") || strings.Contains(msg, "proxy connect") {
		return true
	}
	return false
}

func isAuthFailure(err error) bool {
	var upgradeErr *HTTPUpgradeError
	if errors.As(err, &upgradeErr) {
		if upgradeErr.StatusCode == http.StatusUnauthorized || upgradeErr.StatusCode == http.StatusForbidden {
			return true
		}
	}
	return false
}

func isUpgradeRejectedFailure(err error) bool {
	var upgradeErr *HTTPUpgradeError
	if errors.As(err, &upgradeErr) {
		if upgradeErr.StatusCode == http.StatusUnauthorized || upgradeErr.StatusCode == http.StatusForbidden {
			return false
		}
		if upgradeErr.Reason == "rejected" || (upgradeErr.StatusCode >= 200 && upgradeErr.StatusCode < 600) {
			return true
		}
	}
	return false
}

func isIOFailure(err error) bool {
	var upgradeErr *HTTPUpgradeError
	if errors.As(err, &upgradeErr) && upgradeErr.Reason == "response-io" {
		return true
	}
	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "read" || opErr.Op == "write" {
			return true
		}
	}
	return false
}
