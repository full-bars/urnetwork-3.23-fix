package connect

// net_extender_carrier.go -- the carrier vocabulary and the dial seam of the
// extender client (EXTENDER.md A3, A4, A8, B2).
//
// The records/directory/feed client layer (phase 1) describes a carrier dial
// with ExtenderDial and opens one with DialExtender. The carrier
// implementations themselves -- the A3 HTTP exchange over tcp TLS, the quic
// and dns carriers with their memory budgets, camouflage -- are a later phase,
// so DialExtender here reports the carrier layer as not built. What lives in
// this file is what the client layer needs regardless of which carrier answers:
// the carrier names a record and a response carry, the reserved service
// numbers of the header, the refusal and limit answers, the A3 response frame
// codec, and the helpers the client layer shares with the carriers that are
// not built yet: the phase deadlines of the stream write and read and the
// probe budget (probeContext).
//
// Everything is additive to the v1 `net_extender.go`; nothing there changes.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// Carrier names as they appear in an ExtenderResponse and a record (A4, B2).
const (
	ExtenderCarrierTcp  = "tcp"
	ExtenderCarrierQuic = "quic"
	ExtenderCarrierDns  = "dns"
	// The peer-to-peer webrtc carrier (EXTENDER.md S, C2a). It is not an
	// ip:port carrier: it is reached through the exchange signaling and
	// ICE/STUN, so it is never dialed by dialExtenderStream. The name appears
	// in records and in the directory so clients, the uptime task and the
	// strategy can name it.
	ExtenderCarrierWebRtc = "webrtc"
)

// Fixed carrier ports (A1, L2). A record may name other ports (B2); these are
// what an address with no record is dialed on. The dns carrier is on the
// unprivileged 4053, which every extender binds; only the sn miner also binds
// 53, and a client tries both for every extender, whether or not a record
// lists 53 (net_extender_dns_ports.go).
const (
	ExtenderTcpPort  = 443
	ExtenderQuicPort = 443
	ExtenderDnsPort  = DefaultWhodisPort
)

// Reserved services of the extender header (A8). 0 forwards to the
// destination; the others hand the taken-over stream to an in-process server.
const (
	ExtenderServiceForward uint32 = 0
	ExtenderServiceGossip  uint32 = 1
	ExtenderServiceFeed    uint32 = 2
	// A latency probe (DESIGNNOTES4.md, GEOMAP §2). Reserved here so the
	// service numbers are one list; the probe itself is a later phase.
	ExtenderServiceProbe uint32 = 3
)

// Content type of both the extender request and its response (A3).
const ExtenderContentType = "application/x-ur-extender"

// Maximum serialized extender header, request and response alike (A3).
const ExtenderMaxHeaderByteCount = 1024

// Encoding tld of the dns carrier when a record does not carry one (A1).
const DefaultExtenderDnsTld = "ur.xyz."

// The connect mode of a carrier name, and whether the name is one this client
// can dial.
func ExtenderConnectModeForCarrier(carrier string) (ExtenderConnectMode, bool) {
	switch carrier {
	case ExtenderCarrierTcp:
		return ExtenderConnectModeTcpTls, true
	case ExtenderCarrierQuic:
		return ExtenderConnectModeQuic, true
	case ExtenderCarrierDns:
		return ExtenderConnectModeDns, true
	case ExtenderCarrierWebRtc:
		return ExtenderConnectModeWebRtc, true
	default:
		return "", false
	}
}

// The carrier name of a connect mode, as it appears on the wire.
func ExtenderCarrierForConnectMode(connectMode ExtenderConnectMode) string {
	switch connectMode {
	case ExtenderConnectModeQuic:
		return ExtenderCarrierQuic
	case ExtenderConnectModeDns:
		return ExtenderCarrierDns
	case ExtenderConnectModeWebRtc:
		return ExtenderCarrierWebRtc
	default:
		return ExtenderCarrierTcp
	}
}

// One extender request (A3, A4). The zero value forwards to the destination
// with no challenge, which is what an ordinary dial sends.
type ExtenderDial struct {
	DestinationHost string
	DestinationPort int
	// 32 random bytes; the response carries the signature over it. Probes and
	// activation set it, an ordinary dial leaves it empty.
	Challenge []byte
	// 0 forward, 1 gossip, 2 feed, 3 probe. DestinationHost is ignored when
	// set.
	Service uint32
	// Datagram asks the extender to relay udp datagrams to the destination
	// rather than a stream (net_extender_datagram.go).
	Datagram bool
	// Probe only: the 16 byte client id of an attesting PROVIDER.
	ProbeClientId []byte
	// Probe only: the 32 byte identity key of an attesting EXTENDER. At most
	// one of the two is set; the extender refuses a header that names both.
	ProbeExtenderPublicKey []byte
	// How many extenders the request has already crossed (A11). A client
	// leaves it 0; an NLayer extender sends one more than the header it
	// received.
	HopCount uint32
	// RoundTrip, when set, receives the send time of the request and the
	// receive time of the response. Every dial leaves it nil but a probe.
	RoundTrip *ExtenderRoundTrip
}

// The timing of one extender request over an established carrier: the request
// is written after the outer handshake and the response is the first thing
// read after it, so the two marks bracket exactly one round trip plus the
// extender's handling of the header.
type ExtenderRoundTrip struct {
	SendTime    time.Time
	ReceiveTime time.Time
}

func (self *ExtenderRoundTrip) markSend() {
	if self != nil {
		self.SendTime = time.Now()
	}
}

func (self *ExtenderRoundTrip) markReceive() {
	if self != nil {
		self.ReceiveTime = time.Now()
	}
}

// Rtt is the measured round trip, zero until both marks are set. The marks
// carry the monotonic clock, so a wall clock step cannot make it negative.
func (self *ExtenderRoundTrip) Rtt() time.Duration {
	if self == nil || self.SendTime.IsZero() || self.ReceiveTime.IsZero() {
		return 0
	}
	return self.ReceiveTime.Sub(self.SendTime)
}

// An extender that answered the request with a status other than 200 (A4): it
// was reached and would not carry this request, which is a different fact from
// an extender that could not be reached at all.
type ExtenderRefusedError struct {
	StatusCode int
}

// Implements error.
func (self *ExtenderRefusedError) Error() string {
	return fmt.Sprintf("extender refused the request with status %d", self.StatusCode)
}

// An extender that answered 429: it is over one of its admission limits (A12)
// and will take the request later. It is not a failure of the extender -- no
// failure count, no hold -- but a backoff: the caller leaves the address alone
// for about RetryAfter and tries other extenders first.
type ExtenderLimitedError struct {
	// The Retry-After the answer carried, zero when it carried none.
	RetryAfter time.Duration
}

// Implements error.
func (self *ExtenderLimitedError) Error() string {
	if self.RetryAfter <= 0 {
		return "extender limited the request"
	}
	return fmt.Sprintf("extender limited the request; retry after %s", self.RetryAfter)
}

// How long to leave a limited extender alone (A12): the Retry-After it
// answered with, or defaultBackoff when it gave none, jittered uniformly by
// +-50 %, so the clients one limit turned away do not all come back at once.
func JitterExtenderLimitedBackoff(retryAfter time.Duration, defaultBackoff time.Duration) time.Duration {
	backoff := retryAfter
	if backoff <= 0 {
		backoff = defaultBackoff
	}
	if backoff <= 0 {
		return 0
	}
	return backoff/2 + time.Duration(rand.Int63n(int64(backoff)+1))
}

// withConnWritePhaseDeadline bounds a synchronous connection write phase by
// the earlier of the caller deadline and its phase budget. Context cancellation
// alone cannot interrupt a net.Conn.Write already in progress.
func withConnWritePhaseDeadline(
	ctx context.Context,
	conn net.Conn,
	phaseTimeout time.Duration,
	write func() error,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	if 0 < phaseTimeout {
		phaseDeadline := time.Now().Add(phaseTimeout)
		if !hasDeadline || phaseDeadline.Before(deadline) {
			deadline = phaseDeadline
			hasDeadline = true
		}
	}
	if !hasDeadline {
		return write()
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	defer func() {
		clearErr := conn.SetWriteDeadline(time.Time{})
		if resultErr == nil {
			resultErr = clearErr
		}
	}()
	return write()
}

// withConnReadPhaseDeadline is the read side of the same bound, for the first
// thing a caller reads after the outer handshake: the extender response
// (net_extender_probe_latency.go).
func withConnReadPhaseDeadline(
	ctx context.Context,
	conn net.Conn,
	phaseTimeout time.Duration,
	read func() error,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, hasDeadline := ctx.Deadline()
	if 0 < phaseTimeout {
		phaseDeadline := time.Now().Add(phaseTimeout)
		if !hasDeadline || phaseDeadline.Before(deadline) {
			deadline = phaseDeadline
			hasDeadline = true
		}
	}
	if !hasDeadline {
		return read()
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	defer func() {
		clearErr := conn.SetReadDeadline(time.Time{})
		if resultErr == nil {
			resultErr = clearErr
		}
	}()
	return read()
}

// The probe budget: the caller's deadline, further bounded by the request
// timeout so a probe of an unresponsive address ends on its own. The pin
// keeps it beside its operator probes (net_extender_probe.go); the client's
// latency probe (net_extender_probe_latency.go) shares it, so it is here with
// the other helpers the probes need, and the operator probes join it in their
// own phase.
func probeContext(
	ctx context.Context,
	connectSettings *ConnectSettings,
) (context.Context, context.CancelFunc) {
	if 0 < connectSettings.RequestTimeout {
		return context.WithTimeout(ctx, connectSettings.RequestTimeout)
	}
	return context.WithCancel(ctx)
}

// The A3 response body: a 4-byte big-endian length and the serialized
// ExtenderResponse. The length prefix makes the body self-delimiting, which
// the udp carriers need: there the response body and the raw bytes that follow
// it are the same http3 DATA stream, so a content length would bound the
// reader the caller keeps using.
func ExtenderResponseFrame(response *protocol.ExtenderResponse) ([]byte, error) {
	responseBytes, err := proto.Marshal(response)
	if err != nil {
		return nil, err
	}
	if ExtenderMaxHeaderByteCount < len(responseBytes) {
		return nil, fmt.Errorf("extender response is %d bytes, at most %d", len(responseBytes), ExtenderMaxHeaderByteCount)
	}
	frameBytes := make([]byte, 4+len(responseBytes))
	binary.BigEndian.PutUint32(frameBytes[0:4], uint32(len(responseBytes)))
	copy(frameBytes[4:], responseBytes)
	return frameBytes, nil
}

// Reads exactly one response frame, leaving the reader on the first byte that
// follows it.
func ReadExtenderResponseFrame(reader io.Reader) (*protocol.ExtenderResponse, error) {
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(reader, lengthBytes); err != nil {
		return nil, err
	}
	responseByteCount := int(binary.BigEndian.Uint32(lengthBytes))
	if ExtenderMaxHeaderByteCount < responseByteCount {
		return nil, fmt.Errorf("extender response is %d bytes, at most %d", responseByteCount, ExtenderMaxHeaderByteCount)
	}
	responseBytes := make([]byte, responseByteCount)
	if _, err := io.ReadFull(reader, responseBytes); err != nil {
		return nil, err
	}
	response := &protocol.ExtenderResponse{}
	if err := proto.Unmarshal(responseBytes, response); err != nil {
		return nil, err
	}
	return response, nil
}

// extenderRetryAfter is the Retry-After of a 429, in either form RFC 9110
// allows -- seconds, or a date -- and zero when there is none or it does not
// parse, or has passed. It is unexported here and used by the carrier phase,
// which reads the answer of a real response.
func extenderRetryAfter(header http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		// a day is past any backoff this client would keep, and bounds the
		// arithmetic of a hostile value
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

// The network country the host reports, SetNetworkCountryCode and
// NetworkCountryCode, live with the operator hint that overrides it
// (net_extender_proximity.go). The directory reads the pair through
// NetworkCountryCode alone, so nothing of that file but this pair is needed
// here.
