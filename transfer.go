package connect

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	// "runtime/debug"
	// "runtime"
	// "reflect"
	mathrand "math/rand"
	"slices"
	"strings"

	"golang.org/x/exp/maps"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

var (
	dropErrLogThrottle = newLogThrottle(time.Minute)
	pingLogThrottle    = newLogThrottle(5 * time.Minute)
	pingErrLogThrottle = newLogThrottle(5 * time.Minute)
)

/*
Sends frames to destinations with properties:
- as long the sending client is active, frames are eventually delivered up to timeout
- frames are received in order of send
- sender is notified when frames are received
- sender and receiver account for mutual transfer with a shared contract
- return transfer is accounted to the sender
- support for multiple routes to the destination
- senders are verified with pre-exchanged keys
- high throughput and bounded resource usage

*/

/*
Each transport should apply the forwarding ACL:
- reject if source id does not match network id
- reject if not an active contract between sender and receiver

*/

// The transfer speed of each client is limited by its slowest destination.
// All traffic is multiplexed to a single connection, and blocking
// the connection ultimately limits the rate of `SendWithTimeout`.
// In this a client is similar to a socket. Multiple clients
// can be active in parallel, each limited by their slowest destination.

// *important* note on how "nack" transfer works with contracts
// nack data is associated with a contract, which is sent with ack=true
// on the other side, if the contract_id is not active when the nack arrives,
// the nack is dropped.
// To avoid racing the nack message with the ack contract,
// nacks are sent as ack until the contract is acked

// use 0 for deadlock testing

// DefaultTransferBufferSize is the depth (slot count, not byte size) of the
// per-stream send/receive/forward sequence and ack ring buffers: how many
// in-flight frames the transfer layer holds before applying backpressure.
// Depth interacts with the path bandwidth-delay product - high-latency,
// high-bandwidth links need more outstanding frames to stay saturated. This
// fork uses 16 where upstream uses 32, as a conservative memory baseline for
// nodes carrying many concurrent streams; the turbo profile widens the
// transfer-layer buffers for RAM-rich, low-latency nodes. Whether 16-vs-32
// meaningfully changes long-haul throughput here has not been measured - flag
// for a before/after memory+throughput check before tuning this further.
const DefaultTransferBufferSize = 16

// e2e-pqe merge: upstream code refers to the unexported name in several places;
// alias it to the fork's exported constant so both spellings resolve.
const defaultTransferBufferSize = DefaultTransferBufferSize

var DebugTransferCopyOnWrite = false

type AckFunction = func(err error)

// safeAck invokes an ack callback with panic isolation, without allocating a
// per-send wrapper closure. The raw caller AckFunction is stored in SendPack and
// passed here at ack time.
func safeAck(cb func(error), err error) {
	if cb != nil {
		HandleError(func() {
			cb(err)
		})
	}
}

// provideMode is the mode of where these frames are from: network, friends and family, public
// provideMode nil means no contract
type ReceiveFunction = func(source TransferPath, frames []*protocol.Frame, peer Peer)

type Peer struct {
	ProvideMode protocol.ProvideMode
}

type NetworkPeer struct {
	ClientId       Id
	ProvideModes   []protocol.ProvideMode
	ProvideEnabled bool
	Principal      string
	Roles          []string
	DeviceName     string
	DeviceSpec     string
}

type ForwardFunction = func(path TransferPath, transferFrameBytes []byte)

func DefaultClientSettings() *ClientSettings {
	// e2e-pqe merge: keep the fork's plain constructors (the upstream
	// *WithBufferSize variants for send/receive/forward were not carried over),
	// but use upstream's `settings :=` form so the EncryptionSettings.IdleTimeout
	// adjustment below has a named value to mutate before returning.
	settings := &ClientSettings{
		SendBufferSize:          DefaultTransferBufferSize,
		ForwardBufferSize:       DefaultTransferBufferSize,
		ReadTimeout:             30 * time.Second,
		BufferTimeout:           15 * time.Second,
		ControlPingTimeout:      30 * time.Second,
		SendBufferSettings:      DefaultSendBufferSettings(),
		ReceiveBufferSettings:   DefaultReceiveBufferSettings(),
		ForwardBufferSettings:   DefaultForwardBufferSettings(),
		ContractManagerSettings: DefaultContractManagerSettings(),
		StreamManagerSettings:   DefaultStreamManagerSettings(),
		WebRtcSettings:          DefaultWebRtcSettings(),
		EncryptionSettings:      DefaultEncryptionSettings(),
		ProtocolVersion:         DefaultProtocolVersion,
		DefaultTransferOpts:     DefaultTransferOpts(),
	}
	// A per-peer session is ref-held by both a send and a receive sequence, so
	// it must outlive the longer of the two — otherwise the next burst (after a
	// transport reform or lull) churns a fresh handshake instead of reusing the
	// live cipher.
	settings.EncryptionSettings.IdleTimeout = max(
		settings.SendBufferSettings.IdleTimeout,
		settings.ReceiveBufferSettings.IdleTimeout,
	)
	return settings
}

func DefaultClientSettingsNoNetworkEvents() *ClientSettings {
	clientSettings := DefaultClientSettings()
	clientSettings.ContractManagerSettings = DefaultContractManagerSettingsNoNetworkEvents()
	return clientSettings
}

func DefaultSendBufferSettings() *SendBufferSettings {
	return &SendBufferSettings{
		CreateContractTimeout:       60 * time.Second,
		CreateContractRetryInterval: 5 * time.Second,
		MinResendInterval:           2 * time.Second,
		MaxResendInterval:           8 * time.Second,
		// no backoff
		// ResendBackoffScale: 0,
		RttScale:         1.2,
		RttWindowSize:    128,
		RttWindowTimeout: 60 * time.Second,
		AckTimeout:       60 * time.Second,
		IdleTimeout:      300 * time.Second,
		// pause on resend for selectively acked messaged
		SelectiveAckTimeout: 60 * time.Second,
		SequenceBufferSize:  DefaultTransferBufferSize,
		AckBufferSize:       DefaultTransferBufferSize,
		MinMessageByteCount: ByteCount(1),
		// this includes transport reconnections
		WriteTimeout:            15 * time.Second,
		ResendQueueMaxByteCount: MemoryScaledByteCount(mib(4), kib(256)),
		ResendQueueMinByteCount: kib(256),
		ContractFillFraction:    0.7,
		ProtocolVersion:         DefaultProtocolVersion,
		MaxResendCount:          16,
	}
}

func DefaultReceiveBufferSettings() *ReceiveBufferSettings {
	return &ReceiveBufferSettings{
		GapTimeout: 60 * time.Second,
		// the receive idle timeout should be a bit longer than the send idle timeout
		IdleTimeout:        120 * time.Second,
		SequenceBufferSize: DefaultTransferBufferSize,
		// AckBufferSize: DefaultTransferBufferSize,
		AckCompressTimeout:  time.Duration(0),
		MinMessageByteCount: ByteCount(1),
		// ResendAbuseThreshold: 4,
		// ResendAbuseMultiple:  0.5,
		MaxPeerAuditDuration: 60 * time.Second,
		// this includes transport reconnections
		WriteTimeout:             15 * time.Second,
		ReceiveQueueMaxByteCount: MemoryScaledByteCount(mib(2)+kib(512), kib(320)),
		ReceiveQueueMinByteCount: kib(320),
		AllowLegacyNack:          true,
		MaxOpenReceiveContract:   4,
		ProtocolVersion:          DefaultProtocolVersion,
	}
}

func DefaultForwardBufferSettings() *ForwardBufferSettings {
	return &ForwardBufferSettings{
		IdleTimeout:        300 * time.Second,
		SequenceBufferSize: DefaultTransferBufferSize,
		WriteTimeout:       15 * time.Second,
	}
}

type SendPack struct {
	TransferOptions

	// frame and destination is repacked by the send buffer into a Pack,
	// with destination and frame from the tframe, and other pack properties filled in by the buffer
	Frame           *protocol.Frame
	Destination     TransferPath
	IntermediaryIds MultiHopId
	// called (true) when the pack is ack'd, or (false) if not ack'd (closed before ack)
	AckCallback      AckFunction
	MessageByteCount ByteCount
	Ctx              context.Context
	// ForceUnwrapped pins the wire frame to plaintext for the item's lifetime,
	// including retransmits. Used by session control messages (TLS handshake
	// bytes) that bootstrap the cipher: they must never be sent encrypted, since
	// the peer may not have completed its half of the handshake even after our
	// local cipher is established.
	ForceUnwrapped bool
	// EncryptionRole selects which per-peer session this pack uses, keying the
	// SendSequence so the roles run as distinct sequences: client (the default —
	// the client's own outbound data, whose handshake it initiates/restarts) or
	// server (EncryptedControl carriers and server-session replies, which never
	// restart the handshake).
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion is the per-peer session identity companion this pack
	// uses — keys the SendSequence and its session, distinct from
	// `TransferOptions.CompanionContract` (the contract it rides; the two differ
	// only for a server-role EncryptedControl reply carrier).
	EncryptionCompanion bool
}

type ReceivePack struct {
	Source             TransferPath
	SequenceId         Id
	Pack               *protocol.Pack
	ReceiveCallback    ReceiveFunction
	MessageByteCount   ByteCount
	TransferFrameBytes []byte
	Ctx                context.Context
	// Unwrapped is true when the inbound TransferFrame arrived as plaintext (no
	// outer wrap). The ack for this pack (and any aggregated ack including it) is
	// sent plaintext to mirror, so a peer whose cipher isn't up yet isn't handed
	// a wrapped ack it can't open.
	Unwrapped bool
	// EncryptionRole is the local per-peer session role that owns this inbound
	// stream — the complement of the sender's role, keying the ReceiveSequence
	// and the session it holds. Normal peer data (peer is the TLS client) maps
	// to our server session (the default); EncryptedControl carriers and
	// server-session replies map to our client session.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion is the local session identity companion that owns this
	// inbound stream (shared by both peers, not complemented). Derived from the
	// wire companion hint, the decrypting session, or the EncryptedControl;
	// defaults false. Keys the ReceiveSequence and its session.
	EncryptionCompanion bool
}

type ForwardPack struct {
	Destination        TransferPath
	TransferFrameBytes []byte
	Ctx                context.Context
}

type TransferOptions struct {
	// items can choose to not be acked
	// in this case, the ack callback is called on send, and no retry is done
	// when false, items may arrive out of order amongst un-acked sequence neighbors
	Ack bool
	// use a companion contract
	// a companion contract replies to an existing contract
	// using this option limits the destination to clients that have an active contract to the sender
	CompanionContract bool
	// force contract streams, even when there are zero intermediaries
	ForceStream bool
}

func DefaultTransferOpts() TransferOptions {
	return TransferOptions{
		Ack:               true,
		CompanionContract: false,
		ForceStream:       false,
	}
}

type transferOptionsSetAck struct {
	Ack bool
}

func NoAck() transferOptionsSetAck {
	return transferOptionsSetAck{
		Ack: false,
	}
}

type transferOptionsSetCompanionContract struct {
	CompanionContract bool
}

func CompanionContract() transferOptionsSetCompanionContract {
	return transferOptionsSetCompanionContract{
		CompanionContract: true,
	}
}

type transferOptionsSetForceStream struct {
	ForceStream bool
}

func ForceStream() transferOptionsSetForceStream {
	return transferOptionsSetForceStream{
		ForceStream: true,
	}
}

type transferCtx struct {
	Ctx context.Context
}

func Ctx(ctx context.Context) transferCtx {
	return transferCtx{
		Ctx: ctx,
	}
}

type ClientSettings struct {
	SendBufferSize    int
	ForwardBufferSize int
	ReadTimeout       time.Duration
	BufferTimeout     time.Duration
	// if 0, the client will not send control pings
	ControlPingTimeout time.Duration

	// Log, when set, is used by the client and all nested components
	// (propagated to nested settings `Log` fields that are nil).
	// nil resolves to `DefaultLogger()`. See log.go.
	Log Logger

	SendBufferSettings      *SendBufferSettings
	ReceiveBufferSettings   *ReceiveBufferSettings
	ForwardBufferSettings   *ForwardBufferSettings
	ContractManagerSettings *ContractManagerSettings
	StreamManagerSettings   *StreamManagerSettings
	WebRtcSettings          *WebRtcSettings
	EncryptionSettings      *EncryptionSettings

	// ClientKeySeed, when set, is the long-lived Ed25519 client identity key
	// seed (`ed25519.NewKeyFromSeed`); must be `ed25519.SeedSize` (32) bytes.
	// When empty, `ClientKeyManager` generates a fresh seed. Persist the running
	// value (`Client.ClientKeyManager().Seed()`) and reload it on the next run
	// to keep the published `ClientKey` (and contract bindings to it) stable
	// across process lifetimes.
	ClientKeySeed []byte

	ProtocolVersion int

	DefaultTransferOpts TransferOptions
}

// MinimumMessageLenLimit returns the smallest per-transport framer
// `MaxMessageLen` (and receive-side caps, e.g. `websocket.SetReadLimit`) the
// runtime can reliably operate under. Below it, the per-peer handshake can
// deadlock: the TLS server flight ships as one large `EncryptedControl{Handshake}`
// Pack, and if any hop's framer rejects it as oversized the stream closes
// mid-handshake, the retransmit re-sends the same oversized pack, and both sides
// time out.
//
// Worst-case sizing (verified against the active TLS profile — TLS 1.3,
// X25519MLKEM768 hybrid group, ephemeral ECDSA P-256 cert, mTLS):
//
//	ServerHello ~1.2 KiB (MLKEM768 key share ~1.1 KiB), ChangeCipherSpec ~6 B,
//	EncryptedExtensions ~10 B, CertificateRequest ~30 B, Certificate ~500–600 B,
//	CertificateVerify ~80 B, Finished ~45 B, + ~5 B record header each
//	  ≈ 2 KiB raw; + ~200 B EC/Frame/Pack/TransferFrame proto wrap ≈ 2.2 KiB
//
// Rounded up to 4 KiB to absorb ASN.1 cert-size jitter, a future larger
// post-quantum key share, and protobuf field-tag drift. Production transports
// default well above this; tests and embedded callers should plumb it through
// their framer caps (and matching receive-side limits):
//
//	settings.FramerSettings.MaxMessageLen = max(yourValue, int(client.MinimumMessageLenLimit()))
func (self *ClientSettings) MinimumMessageLenLimit() ByteCount {
	return ByteCount(4 * 1024)
}

// note all callbacks are wrapped to check for nil and recover from errors
type Client struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientId  Id
	clientTag string
	clientOob OutOfBandControl

	log Logger

	settings *ClientSettings

	receiveCallbacks *CallbackList[ReceiveFunction]
	forwardCallbacks *CallbackList[ForwardFunction]

	loopback chan *SendPack

	routeManager             *RouteManager
	contractManager          *ContractManager
	webRtcManager            *WebRtcManager
	streamManager            *StreamManager
	sendBuffer               *SendBuffer
	receiveBuffer            *ReceiveBuffer
	forwardBuffer            *ForwardBuffer
	clientKeyManager         *ClientKeyManager
	encryptionSessionManager *EncryptionSessionManager

	// ready is closed by NewClientWithTag right before it returns, once every
	// manager, buffer, callback, and the `run` loop are wired up. See
	// ReadyNotify for the gating contract.
	ready chan struct{}

	// contractManagerUnsub func()
	webRtcManagerUnsub func()
	streamManagerUnsub func()
}

func NewClientWithDefaults(
	ctx context.Context,
	clientId Id,
	clientOob OutOfBandControl,
) *Client {
	return NewClient(
		ctx,
		clientId,
		clientOob,
		DefaultClientSettings(),
	)
}

func NewClient(
	ctx context.Context,
	clientId Id,
	clientOob OutOfBandControl,
	settings *ClientSettings,
) *Client {
	clientTag := clientId.String()
	return NewClientWithTag(ctx, clientId, clientTag, clientOob, settings)
}

func NewClientWithTag(
	ctx context.Context,
	clientId Id,
	clientTag string,
	clientOob OutOfBandControl,
	settings *ClientSettings,
) *Client {
	cancelCtx, cancel := context.WithCancel(ctx)
	log := loggerOrDefault(settings.Log)
	client := &Client{
		ctx:              cancelCtx,
		cancel:           cancel,
		clientId:         clientId,
		clientTag:        clientTag,
		clientOob:        clientOob,
		log:              log,
		settings:         settings,
		receiveCallbacks: NewCallbackList[ReceiveFunction](),
		forwardCallbacks: NewCallbackList[ForwardFunction](),
		loopback:         make(chan *SendPack),
		ready:            make(chan struct{}),
	}

	routeManager := NewRouteManagerWithLogger(ctx, clientTag, log)
	contractManager := NewContractManager(ctx, client, settings.ContractManagerSettings)
	webRtcManager := NewWebRtcManager(ctx, NewClientSignalSender(client), settings.WebRtcSettings)
	streamManager := NewStreamManager(ctx, client, webRtcManager, settings.StreamManagerSettings)
	// ClientKeyManager must precede EncryptionSessionManager — the latter holds
	// a reference to sign the published TLS cert
	// (`EncryptedKey.ClientKeySignedTlsCertificate`) and per-peer identity proofs.
	clientKeyManager, err := NewClientKeyManager(client.ctx, client)
	if err != nil {
		log.Errorf("[key]%s could not initialize client key: %s\n", client.ClientTag(), err)
		clientKeyManager = nil
	}
	encryptionSessionManager := NewEncryptionSessionManager(client.ctx, client, clientKeyManager, client.settings.EncryptionSettings)

	// client.contractManagerUnsub = client.AddReceiveCallback(contractManager.Receive)
	client.webRtcManagerUnsub = ReceiveSignalsFromClient(client, webRtcManager)
	client.streamManagerUnsub = client.AddReceiveCallback(streamManager.Receive)

	client.initBuffers(routeManager, contractManager, webRtcManager, streamManager, clientKeyManager, encryptionSessionManager)

	go HandleError(client.run, cancel)

	// Mark the client fully constructed: manager goroutines started above (e.g.
	// `publishEncryptedKey`, `providePing`) gate their first send on this so they
	// don't race the wiring above.
	close(client.ready)

	return client
}

// ReadyNotify returns a channel closed once `NewClientWithTag` has finished
// wiring the client (managers, callbacks, buffers, `run` loop). Any goroutine
// launched during construction must wait on it (or `ctx.Done()`) before its
// first send into the client's send path.
func (self *Client) ReadyNotify() <-chan struct{} {
	return self.ready
}

// Log is the logger used by this client and its nested components.
func (self *Client) Log() Logger {
	return self.log
}

// initBuffers wires the six managers (route, contract, WebRTC, stream, key,
// encryption session) into the client and allocates the send, receive, and
// forward buffers from their settings. It runs at the end of
// `NewClientWithTag`, before `ready` is closed: the send path must exist
// before manager goroutines gated on `ReadyNotify` (such as the encryption
// key publish) make their first send.
func (self *Client) initBuffers(
	routeManager *RouteManager,
	contractManager *ContractManager,
	webRtcManager *WebRtcManager,
	streamManager *StreamManager,
	clientKeyManager *ClientKeyManager,
	encryptionSessionManager *EncryptionSessionManager,
) {
	self.routeManager = routeManager
	self.contractManager = contractManager
	self.webRtcManager = webRtcManager
	self.streamManager = streamManager
	self.clientKeyManager = clientKeyManager
	self.encryptionSessionManager = encryptionSessionManager

	// sendBuffer / receiveBuffer / forwardBuffer come first because
	// `EncryptionSessionManager` publishes its cert (via `EncryptedKey`)
	// at construction time, and the publish path goes through
	// `sendBuffer.Pack`.
	self.sendBuffer = NewSendBuffer(self.ctx, self, self.settings.SendBufferSettings)
	self.receiveBuffer = NewReceiveBuffer(self.ctx, self, self.settings.ReceiveBufferSettings)
	self.forwardBuffer = NewForwardBuffer(self.ctx, self, self.settings.ForwardBufferSettings)
}

// EncryptionSessionManager returns the client's per-peer encryption session
// manager, which owns the TLS sessions used to wrap and unwrap frames to and
// from each peer. Fixed after construction and never nil; receive paths still
// guard a nil manager defensively, treating it as encryption disabled.
func (self *Client) EncryptionSessionManager() *EncryptionSessionManager {
	return self.encryptionSessionManager
}

// unwrapFrame opens an outer-wrapped TransferFrame from `sourceId`. `roleHint`
// (the sender's session role; may be `SequenceRoleUnknown`) selects the
// complement local session to try first; `companionHint` (the sender's session
// companion; nil when the sender omitted it) further pins the exact companion
// session. With a role hint but no companion hint, both companion sessions of
// the complement role are tried; with no role hint, every per-peer session is
// tried. Each candidate session's ciphers are tried (established plus, briefly
// during a rekey, the prior established) until one authenticates. Returns the
// plaintext inner bytes and the local session role and companion that
// decrypted them (used as the receive sequence's role/companion). Wait-free:
// it never blocks the receive loop.
func (self *Client) unwrapFrame(sourceId Id, roleHint protocol.SequenceRole, companionHint *bool, wrapped []byte) ([]byte, sequenceTlsRole, bool, error) {
	if self.encryptionSessionManager == nil {
		return nil, sequenceTlsRoleServer, false, fmt.Errorf("encryption disabled")
	}
	// A wrapped frame can only be opened by the complement of the sender's
	// session role (the other local session is the opposite TLS direction
	// with a different key), so a present role hint narrows us to that role —
	// and a present companion hint pins exactly one session (Option 1). A role
	// hint without a companion hint leaves both companion sessions as
	// candidates. With no role hint — the sender omitted it for on-wire
	// anonymity — trial-decrypt against every per-peer session (Option 2).
	var ordered []*peerEncryptionSession
	if senderRole, ok := sequenceTlsRoleFromProtobuf(roleHint); ok {
		complement := senderRole.complement()
		if companionHint != nil {
			if s := self.encryptionSessionManager.Lookup(sourceId, complement, *companionHint); s != nil {
				ordered = append(ordered, s)
			}
		} else {
			ordered = self.encryptionSessionManager.sessionsForPeerRole(sourceId, complement)
		}
	} else {
		ordered = self.encryptionSessionManager.sessionsForPeer(sourceId)
	}
	if len(ordered) == 0 {
		return nil, sequenceTlsRoleServer, false, fmt.Errorf("no encryption session for peer %s", sourceId)
	}
	for _, session := range ordered {
		for _, cipher := range session.decryptCiphers() {
			if plaintext, err := cipher.Open(wrapped); err == nil {
				return plaintext, session.role, session.companion, nil
			}
		}
	}
	return nil, sequenceTlsRoleServer, false, fmt.Errorf("no encryption session for peer %s could decrypt", sourceId)
}

// ClientKeyManager returns the client's Ed25519 key manager, which signs the
// per-peer TLS identity proofs and provides the client's public key. It may
// be nil: when key initialization fails, `NewClientWithTag` logs the error
// and continues without one.
func (self *Client) ClientKeyManager() *ClientKeyManager {
	return self.clientKeyManager
}

// RouteManager returns the client's route manager, which owns the multi-route
// readers and writers used by the run loop and by the send and forward
// sequences. Fixed after construction and never nil.
func (self *Client) RouteManager() *RouteManager {
	return self.routeManager
}

// ContractManager returns the client's contract manager, which tracks the
// bandwidth contracts opened toward each destination and gates sends on
// contract availability. Fixed after construction and never nil.
func (self *Client) ContractManager() *ContractManager {
	return self.contractManager
}

// ClientId returns the client's identity Id, fixed at construction. A send
// addressed to it is looped back locally instead of leaving the process, and
// the run loop treats it as the destination to receive.
func (self *Client) ClientId() Id {
	return self.clientId
}

// ClientTag returns the client's tag, used as the log prefix by the client
// and its nested components. It defaults to the string form of the client Id
// and is fixed at construction.
func (self *Client) ClientTag() string {
	return self.clientTag
}

// ClientOob returns the client's out-of-band control channel to the
// platform, used to deliver peer audits and control frames outside the
// regular send/contract path. Fixed at construction.
func (self *Client) ClientOob() OutOfBandControl {
	return self.clientOob
}

// ReportAbuse flags the peer that sent from `source` as abusive in a peer
// audit sent to the platform. It is called by the security layer when a
// peer's traffic trips an incident policy (for example a blocklisted
// protocol): the audit marks the source's abuse flag and ships it
// out-of-band.
func (self *Client) ReportAbuse(source TransferPath) {
	peerAudit := NewSequencePeerAudit(self, source, 0)
	peerAudit.Update(func(peerAudit *PeerAudit) {
		peerAudit.Abuse = true
	})
	peerAudit.Complete()
}

// ForwardWithTimeout forwards pre-marshalled `TransferFrame` bytes toward the
// destination encoded in their TransferPath, waiting up to `timeout` for the
// frame to be accepted into the forward queue. It reports only the enqueue:
// `true` with no error means accepted, and the forward buffer then takes
// ownership of `transferFrameBytes` (returning them to the message pool after
// the write).
func (self *Client) ForwardWithTimeout(transferFrameBytes []byte, timeout time.Duration, opts ...any) bool {
	success, err := self.ForwardWithTimeoutDetailed(transferFrameBytes, timeout, opts...)
	return success && err == nil
}

// ForwardWithTimeoutDetailed is the Detailed form of ForwardWithTimeout: it
// parses `transferFrameBytes` for its TransferPath (a bad protobuf returns
// `false` with an error), packs the frame into the forward buffer for the
// destination mask, and reports whether it was accepted within `timeout`. A
// `Ctx` option overrides the client context gating the enqueue. On success
// the forward buffer takes ownership of `transferFrameBytes`.
func (self *Client) ForwardWithTimeoutDetailed(transferFrameBytes []byte, timeout time.Duration, opts ...any) (bool, error) {
	select {
	case <-self.ctx.Done():
		return false, errors.New("Done")
	default:
	}

	path, err := FilteredTransferPath(transferFrameBytes)
	if err != nil {
		// bad protobuf
		return false, err
	}

	destination := path.DestinationMask()

	ctx := self.ctx
	for _, opt := range opts {
		switch v := opt.(type) {
		case transferCtx:
			ctx = v.Ctx
		}
	}

	forwardPack := &ForwardPack{
		Destination:        destination,
		TransferFrameBytes: transferFrameBytes,
		Ctx:                ctx,
	}

	return self.forwardBuffer.Pack(forwardPack, timeout)
}

// Forward is ForwardWithTimeout with no timeout: it waits indefinitely for
// the frame to be accepted into the forward queue.
func (self *Client) Forward(transferFrameBytes []byte, opts ...any) bool {
	return self.ForwardWithTimeout(transferFrameBytes, -1, opts...)
}

// SendWithTimeout sends `frame` to `destination`, waiting up to `timeout`
// for the frame to be accepted into the send queue, and reports whether it
// was accepted (SendWithTimeoutDetailed with the error discarded). The
// acknowledgment is delivered asynchronously to `ackCallback`.
func (self *Client) SendWithTimeout(
	frame *protocol.Frame,
	destination TransferPath,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	success, err := self.SendWithTimeoutDetailed(frame, destination, ackCallback, timeout, opts...)
	return success && err == nil
}

// SendWithTimeoutDetailed is the Detailed form of SendWithTimeout: it sends
// `frame` to `destination` with no intermediary hops, waiting up to `timeout`
// for the frame to be accepted into the send queue, and returns whether it
// was accepted. A negative `timeout` waits indefinitely; zero is a
// non-blocking attempt. `opts` may carry `TransferOptions`, `NoAck()`,
// `ForceStream()`, `CompanionContract()`, and `Ctx()` overrides. On
// acceptance the send sequence takes ownership of `frame.MessageBytes`, and
// `ackCallback` (may be nil) is invoked asynchronously: once with nil when
// the message is acknowledged, or with a non-nil error if the send is
// abandoned (no contract, sequence closed).
func (self *Client) SendWithTimeoutDetailed(
	frame *protocol.Frame,
	destination TransferPath,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) (bool, error) {
	return self.sendWithTimeoutDetailed(
		frame,
		destination,
		MultiHopId{},
		ackCallback,
		timeout,
		opts...,
	)
}

// SendMultiHopWithTimeout sends `frame` through one or more intermediary
// hops toward the final destination in `destination`, waiting up to `timeout`
// for acceptance into the send queue. It is SendMultiHopWithTimeoutDetailed
// with the error discarded; the ack arrives asynchronously via
// `ackCallback`.
func (self *Client) SendMultiHopWithTimeout(
	frame *protocol.Frame,
	destination MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	success, err := self.SendMultiHopWithTimeoutDetailed(frame, destination, ackCallback, timeout, opts...)
	return success && err == nil
}

// SendMultiHopWithTimeoutDetailed is the Detailed form of
// SendMultiHopWithTimeout: it sends `frame` along the hop chain in
// `destination` — all ids but the last are intermediaries and the last is the
// final destination — waiting up to `timeout` for acceptance into the send
// queue. An empty chain is an error. Ownership, ack, and timeout semantics
// match SendWithTimeoutDetailed.
func (self *Client) SendMultiHopWithTimeoutDetailed(
	frame *protocol.Frame,
	destination MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) (bool, error) {
	if destination.Len() == 0 {
		return false, errors.New("Must have at least one destination id.")
	}
	intermediaryIds, destinationId := destination.SplitTail()
	// note we do not force stream here
	// legacy no-intermediary will not use streams by default
	return self.sendWithTimeoutDetailed(
		frame,
		DestinationId(destinationId),
		intermediaryIds,
		ackCallback,
		timeout,
		opts...,
	)
}

// sendWithTimeout is the intermediary-carrying form of the send path behind
// a bool-returning wrapper: it sends `frame` toward `destination` through
// `intermediaryIds`, waiting up to `timeout` for acceptance into the send
// queue. See SendWithTimeoutDetailed for the full semantics.
func (self *Client) sendWithTimeout(
	frame *protocol.Frame,
	destination TransferPath,
	intermediaryIds MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) bool {
	success, err := self.sendWithTimeoutDetailed(frame, destination, intermediaryIds, ackCallback, timeout, opts...)
	return success && err == nil
}

// sendWithTimeoutDetailed is the core send path shared by
// SendWithTimeoutDetailed and SendMultiHopWithTimeoutDetailed. It requires
// `destination` to be a destination mask and not a stream (it panics
// otherwise), applies the option overrides, and then either enqueues the
// frame into the send buffer for the destination/intermediary chain or —
// when the destination is this client itself — hands it to the loopback
// channel. It returns whether the frame was accepted within `timeout`; a
// negative timeout waits indefinitely and zero is non-blocking. On
// acceptance the send sequence owns `frame.MessageBytes` and `ackCallback`
// fires asynchronously (nil on ack, a non-nil error if the send is
// abandoned).
func (self *Client) sendWithTimeoutDetailed(
	frame *protocol.Frame,
	destination TransferPath,
	intermediaryIds MultiHopId,
	ackCallback AckFunction,
	timeout time.Duration,
	opts ...any,
) (bool, error) {
	if !destination.IsDestinationMask() {
		panic(fmt.Errorf("Destination required for send: %s", destination))
	}
	if destination.IsStream() {
		panic(fmt.Errorf("Destination must not be a stream: %s", destination))
	}

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done")
	default:
	}

	ctx := self.ctx
	var transferOpts TransferOptions
	transferOpts = self.settings.DefaultTransferOpts
	for _, opt := range opts {
		switch v := opt.(type) {
		case TransferOptions:
			transferOpts = v
		case transferOptionsSetAck:
			transferOpts.Ack = v.Ack
		case transferOptionsSetForceStream:
			transferOpts.ForceStream = v.ForceStream
		case transferOptionsSetCompanionContract:
			transferOpts.CompanionContract = v.CompanionContract
		case transferCtx:
			ctx = v.Ctx
		}
	}

	messageByteCount := ByteCount(len(frame.MessageBytes))
	sendPack := &SendPack{
		TransferOptions:  transferOpts,
		Frame:            frame,
		Destination:      destination,
		IntermediaryIds:  intermediaryIds,
		AckCallback:      ackCallback,
		MessageByteCount: messageByteCount,
		Ctx:              ctx,
		// Ordinary application data: the session identity companion is the
		// sequence's own contract-companion bit (no client/server split here).
		EncryptionCompanion: transferOpts.CompanionContract,
	}

	if sendPack.Destination.DestinationId == self.clientId {
		// loopback
		if timeout < 0 {
			select {
			case <-ctx.Done():
				return false, errors.New("Done")
			case <-self.ctx.Done():
				return false, errors.New("Done")
			case self.loopback <- sendPack:
				return true, nil
			}
		} else if timeout == 0 {
			select {
			case <-ctx.Done():
				return false, errors.New("Done")
			case <-self.ctx.Done():
				return false, errors.New("Done")
			case self.loopback <- sendPack:
				return true, nil
			default:
				return false, nil
			}
		} else {
			select {
			case <-ctx.Done():
				return false, errors.New("Done")
			case <-self.ctx.Done():
				return false, errors.New("Done")
			case self.loopback <- sendPack:
				return true, nil
			case <-time.After(timeout):
				return false, nil
			}
		}
	} else {
		return self.sendBuffer.Pack(sendPack, timeout)
	}
}

// SendControlWithTimeout sends `frame` to the platform control channel
// (`ControlId`), waiting up to `timeout` for acceptance into the send queue.
// Ack and ownership semantics match SendWithTimeoutDetailed.
func (self *Client) SendControlWithTimeout(frame *protocol.Frame, ackCallback AckFunction, timeout time.Duration) bool {
	return self.SendWithTimeout(
		frame,
		DestinationId(ControlId),
		ackCallback,
		timeout,
	)
}

// Send sends `frame` to `destination`, waiting indefinitely for acceptance
// into the send queue; the ack arrives asynchronously via `ackCallback`. It
// is SendWithTimeout with no timeout.
func (self *Client) Send(frame *protocol.Frame, destination TransferPath, ackCallback AckFunction) bool {
	return self.SendWithTimeout(frame, destination, ackCallback, -1)
}

// SendControl sends `frame` to the platform control channel (`ControlId`),
// waiting indefinitely for acceptance into the send queue. It is
// SendControlWithTimeout with no timeout.
func (self *Client) SendControl(frame *protocol.Frame, ackCallback AckFunction) bool {
	return self.Send(
		frame,
		DestinationId(ControlId),
		ackCallback,
	)
}

// SendMultiHop sends `frame` along the hop chain in `destination`, waiting
// indefinitely for acceptance into the send queue. It is
// SendMultiHopWithTimeout with no timeout.
func (self *Client) SendMultiHop(frame *protocol.Frame, destination MultiHopId, ackCallback AckFunction) bool {
	return self.SendMultiHopWithTimeout(frame, destination, ackCallback, -1)
}

// ReceiveFunction
func (self *Client) receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		c := func() any {
			return HandleError(func() {
				receiveCallback(source, frames, peer)
			})
		}
		if self.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[c]receive callback %s %s", self.clientTag, CallbackName(receiveCallback)),
				c,
			)
		} else {
			c()
		}
	}
}

// ForwardFunction
func (self *Client) forward(path TransferPath, transferFrameBytes []byte) {
	for _, forwardCallback := range self.forwardCallbacks.Get() {
		c := func() any {
			return HandleError(func() {
				forwardCallback(path, transferFrameBytes)
			})
		}
		if self.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[c]forward callback %s %s", self.clientTag, CallbackName(forwardCallback)),
				c,
			)
		} else {
			c()
		}
	}
}

// AddReceiveCallback registers `receiveCallback` to be invoked for every
// in-order pack delivered to this client: `source` is the peer path the
// frames came from, `peer` its provide mode, and the `frames` slice is valid
// only for the duration of the call. Callbacks stack (any number may be
// registered) and run in registration order on the delivering sequence's
// goroutine (or the loopback goroutine) — never on the caller's goroutine —
// each wrapped to recover panics. The returned function removes the
// callback and is safe to call more than once.
func (self *Client) AddReceiveCallback(receiveCallback ReceiveFunction) func() {
	callbackId := self.receiveCallbacks.Add(receiveCallback)
	return func() {
		self.receiveCallbacks.Remove(callbackId)
	}
}

// AddForwardCallback registers `forwardCallback` to be invoked for every
// message this client receives that is addressed to a destination other than
// itself (relay traffic): `path` is its TransferPath and `transferFrameBytes`
// the raw frame bytes. Callbacks stack, run in registration order on the
// run-loop goroutine, and are each wrapped to recover panics. The returned
// function removes the callback. The run loop does not return
// `transferFrameBytes` to the message pool after dispatching, so a callback
// must not retain them past the call.
func (self *Client) AddForwardCallback(forwardCallback ForwardFunction) func() {
	callbackId := self.forwardCallbacks.Add(forwardCallback)
	return func() {
		self.forwardCallbacks.Remove(callbackId)
	}
}

// run is the client's main loop, launched by NewClientWithTag. It reads
// inbound messages addressed to this client through a multi-route reader and
// for each: drops stream-addressed or unparseable frames, dispatches acks to
// the send buffer, and delivers packs to the receive buffer (which surfaces
// their frames to the receive callbacks); messages addressed to another
// destination are dispatched to the forward callbacks for relaying. Before
// the loop it also starts a control ping goroutine — uniform random interval
// with mean `ControlPingTimeout`, skipped when this client is the control
// client or the timeout is zero — and a loopback goroutine that delivers
// loopback sends in order. The loop exits when the client context is done;
// exiting (or panicking, via the HandleError wrapper) cancels the client
// context, tearing down the managers, buffers, and sequences.
func (self *Client) run() {
	defer self.cancel()

	// receive
	multiRouteReader := self.routeManager.OpenMultiRouteReader(DestinationId(self.clientId))
	defer self.routeManager.CloseMultiRouteReader(multiRouteReader)

	updatePeerAudit := func(source TransferPath, callback func(*PeerAudit)) {
		// immediately send peer audits at this level
		peerAudit := NewSequencePeerAudit(self, source, 0)
		peerAudit.Update(callback)
		peerAudit.Complete()
	}

	// control ping
	if self.clientId != ControlId && 0 < self.settings.ControlPingTimeout {
		go HandleError(func() {
			for {
				// uniform timeout with mean `ControlPingTimeout`
				timeout := time.Duration(mathrand.Int63n(int64(2 * self.settings.ControlPingTimeout)))
				select {
				case <-self.ctx.Done():
					return
				case <-WakeupAfter(timeout, self.settings.ControlPingTimeout):
				}

				ack := make(chan error)
				frame, err := ToFrame(&protocol.ControlPing{}, self.settings.ProtocolVersion)
				if err != nil {
					self.log.Errorf("[c]could not create ping frame = %s", err)
					continue
				}

				self.SendControl(frame, func(err error) {
					select {
					case ack <- err:
					case <-self.ctx.Done():
					}
				})
				// wait for the ack before sending another ping
				select {
				case err := <-ack:
					if err == nil {
						if ok, suppressed := pingLogThrottle.Allow(time.Now()); ok {
							if suppressed > 0 {
								self.log.Infof("[c]ping (%d suppressed)\n", suppressed)
							} else {
								self.log.Infof("[c]ping\n")
							}
						}
					} else {
						if ok, suppressed := pingErrLogThrottle.Allow(time.Now()); ok {
							if suppressed > 0 {
								self.log.Infof("[c]ping err = %s (%d suppressed)\n", err, suppressed)
							} else {
								self.log.Infof("[c]ping err = %s\n", err)
							}
						}
					}
				case <-self.ctx.Done():
					return
				}
			}
		})
	}

	// loopback messages must be serialized
	go HandleError(func() {
		for {
			select {
			case <-self.ctx.Done():
				return
			case sendPack := <-self.loopback:
				HandleError(func() {
					self.receive(
						SourceId(self.clientId),
						[]*protocol.Frame{sendPack.Frame},
						Peer{ProvideMode: protocol.ProvideMode_Network},
					)
					safeAck(sendPack.AckCallback, nil)
					MessagePoolReturn(sendPack.Frame.MessageBytes)
				}, func(err error) {
					safeAck(sendPack.AckCallback, err)
				})
			}
		}
	}, self.cancel)

	for {
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		var transferFrameBytes []byte
		var err error
		c := func() error {
			transferFrameBytes, err = multiRouteReader.Read(self.ctx, self.settings.ReadTimeout)
			return err
		}
		if self.log.V(2).Enabled() {
			TraceWithReturn(
				fmt.Sprintf("[c]multi route read %s<-", self.clientTag),
				c,
			)
		} else {
			c()
		}
		if err != nil {
			continue
		}

		// at this point, the route is expected to have already parsed the transfer frame
		// and applied basic validation and source/destination checks
		// because of this, errors in parsing the `FilteredTransferFrame` are not expected
		// decode a minimal subset of the full message needed to make a routing decision
		path, err := FilteredTransferPath(transferFrameBytes)
		if err != nil {
			// bad protobuf (unexpected, see route note above)
			MessagePoolReturn(transferFrameBytes)
			continue
		}
		if path.IsStream() {
			if v := self.log.V(1); v.Enabled() {
				v.Infof("[cr] %s cannot route message with stream\n", self.clientTag)
			}
			MessagePoolReturn(transferFrameBytes)
			continue
		}

		source := path.SourceMask()

		if v := self.log.V(1); v.Enabled() {
			v.Infof("[cr] %s %s<-%s s(%s)\n", self.clientTag, path.DestinationId, path.SourceId, path.StreamId)
		}

		if path.DestinationId == self.clientId {
			// the transports have typically not parsed the full `TransferFrame`
			// on error, discard the message and report the peer
			transferFrame := &protocol.TransferFrame{}
			if !unmarshalTransferFrame(transferFrameBytes, transferFrame, false) {
				// bad protobuf
				updatePeerAudit(source, func(a *PeerAudit) {
					a.badMessage(ByteCount(len(transferFrameBytes)))
				})
				MessagePoolReturn(transferFrameBytes)
				continue
			}

			// unwrapped tracks whether the frame arrived on the wire as
			// plaintext (true) or wrapped (false). Propagated through the
			// ReceivePack → receiveItem → ack path so an ack mirrors the
			// wrap state of the messages it acknowledges. Mirroring keeps
			// acks legible to peers whose ciphers haven't come up yet.
			unwrapped := true

			// receiveRole is the local per-peer session role that owns this
			// inbound stream, handed to the ReceiveBuffer so the
			// ReceiveSequence holds the right session. Default server: normal
			// peer data (the peer is the TLS client) decrypts under our
			// server session. Adjusted below to the role that actually
			// decrypted a wrapped frame, and to client for a plaintext
			// EncryptedControl carrier (the peer's server-role stream).
			receiveRole := sequenceTlsRoleServer
			// receiveCompanion is the local session identity companion owning
			// this inbound stream (not complemented). Default false; set below
			// from the decrypting session, the plaintext companion hint, or the
			// EncryptedControl.
			receiveCompanion := false

			// outer encrypted wrap: the inner bytes are themselves a
			// `TransferFrame`. A per-peer session for `source` carries the
			// cipher. Forwarders never see this branch — they only look at
			// the outer TransferPath, which is plaintext.
			if 0 < len(transferFrame.EncryptedTransferFrame) {
				unwrapped = false
				// Unwrap is fully non-blocking: if no session can decrypt
				// yet, drop the frame and let the sender's resend recover. A
				// client-role send sequence restarts the handshake on its
				// next burst, so a peer that lost (or never built) its
				// responder session rebuilds it — the drop is transient, not
				// a wedge. Keeping the unwrap path wait-free means no single
				// peer can park the single-threaded, all-peers receive loop.
				unwrappedTransferFrameBytes, decryptRole, decryptCompanion, err := self.unwrapFrame(
					path.SourceId, transferFrame.GetSessionRole(), transferFrame.SessionCompanion, transferFrame.EncryptedTransferFrame)
				if err != nil {
					if v := self.log.V(1); v.Enabled() {
						v.Infof("[cr]unwrap err = %s\n", err)
					}
					MessagePoolReturn(transferFrameBytes)
					continue
				}
				receiveRole = decryptRole
				receiveCompanion = decryptCompanion
				unwrappedTransferFrame := &protocol.TransferFrame{}
				if !unmarshalTransferFrame(unwrappedTransferFrameBytes, unwrappedTransferFrame, true) {
					updatePeerAudit(source, func(a *PeerAudit) {
						a.badMessage(ByteCount(len(transferFrameBytes)))
					})
					MessagePoolReturn(transferFrameBytes)
					MessagePoolReturn(unwrappedTransferFrameBytes)
					continue
				}
				// the inner TransferPath is AEAD-authenticated; the outer
				// is only the routing hint. A mismatch implies tampering
				// in flight or a routing/sender bug. Drop and audit.
				unwrappedPath, err := TransferPathFromProtobuf(unwrappedTransferFrame.TransferPath)
				if err != nil || unwrappedPath != path {
					if v := self.log.V(1); v.Enabled() {
						v.Infof("[cr] %s outer/inner TransferPath mismatch from %s\n", self.clientTag, path.SourceId)
					}
					updatePeerAudit(source, func(a *PeerAudit) {
						a.badMessage(ByteCount(len(transferFrameBytes)))
					})
					MessagePoolReturn(transferFrameBytes)
					MessagePoolReturn(unwrappedTransferFrameBytes)
					continue
				}
				MessagePoolReturn(transferFrameBytes)
				transferFrameBytes = unwrappedTransferFrameBytes
				transferFrame = unwrappedTransferFrame
			}

			// A plaintext pack with a sender-role hint is the peer's
			// EncryptedControl carrier (its server-role stream). Map the whole
			// sequence to the complement local session — across both the EC packs
			// and the non-EC open/contract packs — so they share one receive
			// sequence; deriving the role per-pack (from the EC frames below)
			// would split the open pack off and gap the handshake. Wrapped packs
			// use the decrypt role from above; the no-hint default is server. The
			// companion hint, when present, pins the companion session (shared by
			// both peers, so taken as-is, not complemented).
			if unwrapped {
				if senderRole, ok := sequenceTlsRoleFromProtobuf(transferFrame.GetSessionRole()); ok {
					receiveRole = senderRole.complement()
				}
				if transferFrame.SessionCompanion != nil {
					receiveCompanion = transferFrame.GetSessionCompanion()
				}
			}

			ack := transferFrame.Ack
			pack := transferFrame.Pack

			if frame := transferFrame.GetFrame(); frame != nil {

				switch frame.GetMessageType() {
				case protocol.MessageType_TransferAck:
					ack = &protocol.Ack{}
					if err := ProtoUnmarshal(frame.GetMessageBytes(), ack); err != nil {
						// bad protobuf
						updatePeerAudit(source, func(a *PeerAudit) {
							a.badMessage(ByteCount(len(transferFrameBytes)))
						})
						MessagePoolReturn(transferFrameBytes)
						continue
					}

				case protocol.MessageType_TransferPack:
					pack = &protocol.Pack{}
					if err := ProtoUnmarshal(frame.GetMessageBytes(), pack); err != nil {
						// bad protobuf
						updatePeerAudit(source, func(a *PeerAudit) {
							a.badMessage(ByteCount(len(transferFrameBytes)))
						})
						MessagePoolReturn(transferFrameBytes)
						continue
					}

				default:
					updatePeerAudit(source, func(a *PeerAudit) {
						a.badMessage(ByteCount(len(transferFrameBytes)))
					})
					MessagePoolReturn(transferFrameBytes)
					continue
				}
			}

			if ack != nil {
				c := func() bool {
					defer MessagePoolReturn(transferFrameBytes)
					return self.sendBuffer.Ack(
						source.Reverse(),
						ack,
						self.settings.BufferTimeout,
					)
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[cr]ack %s %s<-%s s(%s)", self.clientTag, path.DestinationId, path.SourceId, path.SourceId),
						c,
					)
				} else {
					c()
				}
			}
			if pack != nil {
				sequenceId, err := IdFromBytes(pack.SequenceId)
				if err != nil {
					// bad protobuf
					MessagePoolReturn(transferFrameBytes)
					continue
				}
				// Optimistic EC apply: deliver EncryptedControl frames straight to
				// the per-peer session from the receive loop, bypassing the in-order
				// ReceiveSequence drain (which can stall on a sequence gap from a
				// transport reform or loss). EC frames only piggyback that ordering
				// to reuse the retransmit/route plumbing; each handler below is safe
				// to invoke off-order:
				//   - Handshake: gated on `IsAwaitingClientFinished` + a record-prefix
				//     check in `OptimisticallyDeliverHandshake` that rejects
				//     ClientHello-shaped retransmits, so no duplicate bytes reach the
				//     TLS state machine.
				//   - IdentityProof: `receivePeerIdentityProof` short-circuits once
				//     verified, failed, or already buffered — safe to re-deliver.
				// The ReceiveSequence's later in-order delivery still runs and
				// short-circuits in both handlers (just a re-unmarshal). Gated on
				// `unwrapped` (EC packs are always ForceUnwrapped) to skip the
				// wrapped app-data hot path.
				if unwrapped && self.encryptionSessionManager != nil {
					for _, frame := range pack.Frames {
						if frame == nil || frame.MessageType != protocol.MessageType_TransferEncryptedControl {
							continue
						}
						ec := &protocol.EncryptedControl{}
						if err := ProtoUnmarshal(frame.MessageBytes, ec); err != nil {
							continue
						}
						senderRole, ok := sequenceTlsRoleFromProtobuf(ec.SessionRole)
						if !ok {
							continue
						}
						// This stream maps to the complement local session —
						// the one the EncryptedControl drives — keyed by the
						// EC's echoed identity companion. The receive sequence
						// holds it (keeping it alive through the handshake),
						// matching where the EC routes below.
						receiveRole = senderRole.complement()
						receiveCompanion = ec.GetCompanion()
						// Optimistically apply to the complement local session
						// if it already exists; the ReceiveSequence's in-order
						// delivery getOrCreates it otherwise.
						session := self.encryptionSessionManager.Lookup(path.SourceId, senderRole.complement(), ec.GetCompanion())
						if session == nil {
							continue
						}
						switch ec.ControlType {
						case protocol.EncryptedControlType_EncryptedControlHandshake:
							if session.IsAwaitingClientFinished() {
								session.OptimisticallyDeliverHandshake(ec.Payload)
							}
						case protocol.EncryptedControlType_EncryptedControlIdentityProof:
							// Optimistic path must not create epoch state from a
							// stale/reordered/retransmitted proof; only deliver
							// against an epoch that already exists. The in-order
							// path (DeliverEncryptedControl) still handles a proof
							// that races ahead of the local handshake by creating
							// the epoch to buffer it.
							if session.currentEpoch() != nil {
								session.receivePeerIdentityProof(ec.Payload)
							}
						}
					}
				}
				messageByteCount := MessageByteCount(pack.Frames)
				c := func() bool {
					success, err := self.receiveBuffer.Pack(&ReceivePack{
						Source:              source,
						SequenceId:          sequenceId,
						Pack:                pack,
						ReceiveCallback:     self.receive,
						MessageByteCount:    messageByteCount,
						TransferFrameBytes:  transferFrameBytes,
						Unwrapped:           unwrapped,
						EncryptionRole:      receiveRole,
						EncryptionCompanion: receiveCompanion,
					}, self.settings.BufferTimeout)
					if !success {
						MessagePoolReturn(transferFrameBytes)
					}
					return success && err == nil
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf("[cr]pack %s %s<-%s s(%s)", self.clientTag, path.DestinationId, path.SourceId, path.StreamId),
						c,
					)
				} else {
					c()
				}
			}
		} else {
			c := func() {
				self.forward(
					path,
					transferFrameBytes,
				)
			}
			if self.log.V(1).Enabled() {
				Trace(
					fmt.Sprintf("[cr]forward %s %s<-%s s(%s)", self.clientTag, path.DestinationId, path.SourceId, path.StreamId),
					c,
				)
			} else {
				c()
			}
		}
	}
}

// ResendQueueSize reports the size of the resend queue of the send sequence
// addressed by `destination`, `intermediaryIds`, `companionContract`, and
// `forceStream`: the number of items, their total byte count, and the
// sequence's Id. If no such sequence exists (or no send buffer is
// configured), it reports zero with the zero Id.
func (self *Client) ResendQueueSize(destination TransferPath, intermediaryIds MultiHopId, companionContract bool, forceStream bool) (int, ByteCount, Id) {
	count, byteSize, sequenceId, _ := self.ResendQueueSizeAndMessageTypes(destination, intermediaryIds, companionContract, forceStream)
	return count, byteSize, sequenceId
}

// ResendQueueSizeAndMessageTypes is ResendQueueSize with the protocol
// message types of the queued items appended to the result; the types are
// unmarshalled from each queued transfer frame, so an item that fails to
// unmarshal contributes no types. If no matching sequence exists (or no send
// buffer is configured), the message-type slice is nil.
func (self *Client) ResendQueueSizeAndMessageTypes(
	destination TransferPath,
	intermediaryIds MultiHopId,
	companionContract bool,
	forceStream bool,
) (
	int,
	ByteCount,
	Id,
	[]protocol.MessageType,
) {
	if self.sendBuffer == nil {
		return 0, 0, Id{}, nil
	} else {
		return self.sendBuffer.ResendQueueSizeAndMessageTypes(destination, intermediaryIds, companionContract, forceStream)
	}
}

// ReceiveQueueSize reports the size of the receive queue of the receive
// sequence addressed by `source` and `sequenceId`: the number of items and
// their total byte count. If no such sequence exists (or no receive buffer
// is configured), it reports zero.
func (self *Client) ReceiveQueueSize(source TransferPath, sequenceId Id) (int, ByteCount) {
	count, byteSize, _ := self.ReceiveQueueSizeAndMessageTypes(source, sequenceId)
	return count, byteSize
}

// ReceiveQueueSizeAndMessageTypes is ReceiveQueueSize with the protocol
// message types of the queued items appended to the result. If no such
// sequence exists (or no receive buffer is configured), the message-type
// slice is nil.
func (self *Client) ReceiveQueueSizeAndMessageTypes(source TransferPath, sequenceId Id) (int, ByteCount, []protocol.MessageType) {
	if self.receiveBuffer == nil {
		return 0, 0, nil
	} else {
		return self.receiveBuffer.ReceiveQueueSizeAndMessageTypes(source, sequenceId)
	}
}

// IsDone reports whether the client is done, that is, whether its context
// has been canceled (see Close and Cancel). It is non-blocking.
func (self *Client) IsDone() bool {
	select {
	case <-self.ctx.Done():
		return true
	default:
		return false
	}
}

// Done returns the channel that is closed when the client is done; it is
// the client context's Done channel, closed by Close, Cancel, or an
// external cancel of the client context.
func (self *Client) Done() <-chan struct{} {
	return self.ctx.Done()
}

// Ctx returns the client's context, which gates the client's goroutines and
// channel operations and is canceled by Close, Cancel, or an external
// cancel.
func (self *Client) Ctx() context.Context {
	return self.ctx
}

// Close tears the client down: it cancels the client context, closes the
// send, receive, and forward buffers (canceling every open sequence), closes
// the encryption session manager, and unsubscribes from the WebRTC and
// stream managers.
// this does not need to be called if `Cancel` is called
func (self *Client) Close() {
	self.cancel()

	self.sendBuffer.Close()
	self.receiveBuffer.Close()
	self.forwardBuffer.Close()
	if self.encryptionSessionManager != nil {
		self.encryptionSessionManager.Close()
	}

	// self.contractManagerUnsub()
	self.webRtcManagerUnsub()
	self.streamManagerUnsub()
}

// Cancel cancels the client context and cancels the send, receive, and
// forward buffers, which cancels every open sequence so their run loops
// shut down and drain their queues. It is the lightweight form of Close:
// it does not close the encryption session manager or unsubscribe from the
// WebRTC and stream managers.
func (self *Client) Cancel() {
	self.cancel()

	self.sendBuffer.Cancel()
	self.receiveBuffer.Cancel()
	self.forwardBuffer.Cancel()
}

// Flush shuts down all pending transfers: it cancels every open sequence in
// the send, receive, and forward buffers, discarding their queued items, and
// flushes the contract manager's queued contracts without resetting the
// used-contract tracking. It is the shutdown path used when pending data
// should be discarded rather than delivered.
func (self *Client) Flush() {
	self.sendBuffer.Flush()
	self.receiveBuffer.Flush()
	self.forwardBuffer.Flush()

	self.contractManager.Flush(false)
}

type SendBufferSettings struct {
	CreateContractTimeout       time.Duration
	CreateContractRetryInterval time.Duration

	// resend timeout is the initial time between successive send attempts. Does linear backoff
	MinResendInterval time.Duration
	MaxResendInterval time.Duration
	// ResendBackoffScale float32

	RttScale         float32
	RttWindowSize    int
	RttWindowTimeout time.Duration

	// on ack timeout, no longer attempt to retransmit and notify of ack failure
	AckTimeout  time.Duration
	IdleTimeout time.Duration

	SelectiveAckTimeout time.Duration

	SequenceBufferSize int
	AckBufferSize      int

	MinMessageByteCount ByteCount

	WriteTimeout time.Duration

	ResendQueueMaxByteCount ByteCount
	ResendQueueMinByteCount ByteCount
	ResendQueueBudget       *TransferMemoryBudget

	// as this ->1, there is more risk that noack messages will get dropped due to out of sync contracts
	ContractFillFraction float32

	ProtocolVersion int

	// MaxResendCount caps the number of times a packet is retransmitted before
	// being dropped from the resend queue. 0 means unlimited (legacy behavior).
	MaxResendCount int
}

type sendSequenceId struct {
	Destination       TransferPath
	IntermediaryIds   MultiHopId
	CompanionContract bool
	ForceStream       bool
	// EncryptionRole separates the client-role send sequence (normal
	// application data, which restarts the handshake) from the server-role
	// send sequence (EncryptedControl carriers + server replies, which never
	// restart). Zero value is client.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion is the per-peer session identity companion, distinct
	// from `CompanionContract`: a server-role reply carrier echoes the
	// initiator's bit while riding EncryptionControlUseCompanion, so it must key
	// the sequence separately to keep each session's carrier distinct.
	EncryptionCompanion bool
}

type SendBuffer struct {
	ctx    context.Context
	client *Client
	log    Logger

	sendBufferSettings *SendBufferSettings

	mutex                      sync.Mutex
	sendSequences              map[sendSequenceId]*SendSequence
	sendSequencesByDestination map[TransferPath]map[*SendSequence]bool
	sendSequenceDestinations   map[*SendSequence]map[TransferPath]bool
}

func NewSendBuffer(ctx context.Context,
	client *Client,
	sendBufferSettings *SendBufferSettings) *SendBuffer {
	return &SendBuffer{
		ctx:                        ctx,
		client:                     client,
		log:                        client.log,
		sendBufferSettings:         sendBufferSettings,
		sendSequences:              map[sendSequenceId]*SendSequence{},
		sendSequencesByDestination: map[TransferPath]map[*SendSequence]bool{},
		sendSequenceDestinations:   map[*SendSequence]map[TransferPath]bool{},
	}
}

// Pack enqueues `sendPack` for delivery on the send sequence keyed by its
// destination, intermediary ids, companion-contract and force-stream flags,
// and encryption role and companion. It creates the sequence if none exists,
// starts its run loop goroutine, and delegates to SendSequence.Pack. If the
// sequence shuts down between selection and enqueue, Pack retries once
// against a fresh sequence. On success the sequence takes ownership of the
// pack's frame message bytes (they are returned to the message pool once the
// pack is sent or the sequence shuts down); on failure the caller retains
// ownership.
func (self *SendBuffer) Pack(sendPack *SendPack, timeout time.Duration) (bool, error) {
	sendSequenceId := sendSequenceId{
		Destination:         sendPack.Destination,
		IntermediaryIds:     sendPack.IntermediaryIds,
		CompanionContract:   sendPack.TransferOptions.CompanionContract,
		ForceStream:         sendPack.TransferOptions.ForceStream,
		EncryptionRole:      sendPack.EncryptionRole,
		EncryptionCompanion: sendPack.EncryptionCompanion,
	}

	initSendSequence := func(skip *SendSequence) *SendSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		sendSequence, ok := self.sendSequences[sendSequenceId]
		if ok {
			if skip == nil || skip != sendSequence {
				return sendSequence
			} else {
				sendSequence.Cancel()
				// delete(self.sendSequences, sendSequenceId)
			}
		}
		sendSequence = NewSendSequence(
			self.ctx,
			self.client,
			self,
			sendPack.Destination,
			sendPack.IntermediaryIds,
			sendPack.TransferOptions.CompanionContract,
			sendPack.TransferOptions.ForceStream,
			sendPack.EncryptionRole,
			sendPack.EncryptionCompanion,
			self.sendBufferSettings,
		)
		self.sendSequences[sendSequenceId] = sendSequence
		// note we do not associate destination here
		// the sequence will call `AssociateDestination` before it writes
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				sendSequence.Close()
				// clean up
				if sendSequence == self.sendSequences[sendSequenceId] {
					delete(self.sendSequences, sendSequenceId)
				}
				if destinations, ok := self.sendSequenceDestinations[sendSequence]; ok {
					for destination, _ := range destinations {
						if sendSequences, ok := self.sendSequencesByDestination[destination]; ok {
							delete(sendSequences, sendSequence)
							if len(sendSequences) == 0 {
								delete(self.sendSequencesByDestination, destination)
							}
						}
					}
					delete(self.sendSequenceDestinations, sendSequence)
				}
			}()
			sendSequence.Run()
		})
		return sendSequence
	}

	var sendSequence *SendSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		sendSequence = initSendSequence(sendSequence)
		if success, err = sendSequence.Pack(sendPack, timeout); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

// SendEncryptedControl enqueues an `EncryptedControl` to `destination` as a
// regular Pack Frame (`MessageType = TransferEncryptedControl`); routing,
// retransmit, and in-order delivery reuse the sequence machinery, and the
// destination's ReceiveSequence intercepts these frames into the per-peer
// session.
//
// `ctx` gates whether the spawned goroutine may enqueue (it bails if done). The
// pack uses the SendBuffer's ctx — the session ctx must not propagate into
// `SendPack.Ctx`, since SendBuffer.Pack treats a canceled `SendPack.Ctx` as a
// sequence problem and cancels the SendSequence.
func (self *SendBuffer) SendEncryptedControl(ctx context.Context, peerId Id, role sequenceTlsRole, ec *protocol.EncryptedControl, encryptionCompanion bool, contractCompanion bool) bool {
	select {
	case <-ctx.Done():
		return false
	default:
	}
	ecBytes, err := ProtoMarshal(ec)
	if err != nil {
		return false
	}
	frame := &protocol.Frame{
		MessageType:  protocol.MessageType_TransferEncryptedControl,
		MessageBytes: ecBytes,
	}
	// Mirror the client's default TransferOptions — especially
	// `ForceStream` — so the SendSequence chosen by `SendBuffer.Pack`
	// matches the one the application's `Client.Send` chooses for this
	// destination.
	//
	// The carrier rides one send sequence per (peer, companion, role).
	// `contractCompanion` (the session's carrierCompanion) is which contract it
	// rides; `encryptionCompanion` (the session identity bit) keys the
	// sequence/session. They differ only for a server reply, where the identity
	// is the initiator's echoed bit but the contract is
	// EncryptionControlUseCompanion. Symmetric config: both false.
	opts := self.client.settings.DefaultTransferOpts
	opts.Ack = true
	opts.CompanionContract = contractCompanion
	// V(2) diagnostic: in symmetric mode no encryption-control carrier should
	// be a companion. Log the decision so a companion carrier (whose Stream-mode
	// contract the platform rejects → handshake stalls) can be caught.
	self.log.V(2).Infof(
		"[sb][enc-ctrl]%s peer=%s role=%v companion=%t contract-companion=%t\n",
		self.client.ClientTag(), peerId, role, encryptionCompanion, contractCompanion,
	)
	sendPack := &SendPack{
		TransferOptions:  opts,
		Frame:            frame,
		Destination:      DestinationId(peerId),
		AckCallback:      func(error) {},
		MessageByteCount: ByteCount(len(ecBytes)),
		Ctx:              self.ctx,
		// Pin to plaintext on every (re)send. These frames bootstrap the
		// per-peer cipher; sending them wrapped would deadlock the
		// handshake whenever the local cipher becomes available before
		// the peer's side completes its half. See writeMaybeWrappedBytes.
		ForceUnwrapped: true,
		// Carry EncryptedControl on the send sequence of the originating
		// session's role (client-session handshake bytes on the (peer,client)
		// sequence, server-session bytes on the (peer,server) one). For the
		// client role this is the same sequence the application data uses, so
		// the ClientHello produced by that sequence's own restart rides it
		// without spawning a second sequence (no recursion); the restart is a
		// no-op while a handshake is already in flight. The EncryptedControl's
		// `session_role` + `companion` tell the receiver which complement
		// session to route each frame to.
		EncryptionRole:      role,
		EncryptionCompanion: encryptionCompanion,
	}
	for {
		if success, _ := self.Pack(sendPack, self.client.settings.BufferTimeout); success {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-self.ctx.Done():
			return false
		default:
		}
	}
}

// Ack delivers an incoming `ack` to the send sequences associated with
// `destination`, stopping at the first sequence that accepts it into its
// ack queue. It reports whether any sequence accepted the ack, and logs at
// V(1) when no sequence is associated with the destination.
func (self *SendBuffer) Ack(destination TransferPath, ack *protocol.Ack, timeout time.Duration) bool {
	sendSequences := func() []*SendSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if sendSequences, ok := self.sendSequencesByDestination[destination]; ok {
			return maps.Keys(sendSequences)
		} else {
			return []*SendSequence{}
		}
	}

	anyFound := false
	anySuccess := false
	for _, seq := range sendSequences() {
		anyFound = true
		if success, err := seq.Ack(ack, timeout); success && err == nil {
			anySuccess = true
			break
		}
	}
	if !anyFound {
		self.log.V(1).Infof("[sb]ack miss sequence does not exist %s\n", destination)
	}
	return anySuccess
}

// ResendQueueSizeAndMessageTypes reports the resend queue of the send
// sequence keyed by `destination`, `intermediaryIds`, `companionContract`,
// and `forceStream`: item count, byte count, sequence Id, and the message
// types of the queued items. If no such sequence exists, it reports zeros
// with a nil type slice.
func (self *SendBuffer) ResendQueueSizeAndMessageTypes(destination TransferPath, intermediaryIds MultiHopId, companionContract bool, forceStream bool) (int, ByteCount, Id, []protocol.MessageType) {
	sendSequence := func() *SendSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		return self.sendSequences[sendSequenceId{
			Destination:       destination,
			IntermediaryIds:   intermediaryIds,
			CompanionContract: companionContract,
			ForceStream:       forceStream,
		}]
	}

	if seq := sendSequence(); seq != nil {
		return seq.ResendQueueSizeAndMessageTypes()
	}
	return 0, 0, Id{}, nil
}

// called before a send sequence writes a transfer frame with a stream id,
// once per destination
func (self *SendBuffer) AssociateDestination(sendSequence *SendSequence, destination TransferPath) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	sendSequences, ok := self.sendSequencesByDestination[destination]
	if !ok {
		sendSequences = map[*SendSequence]bool{}
		self.sendSequencesByDestination[destination] = sendSequences
	}
	sendSequences[sendSequence] = true

	destinations, ok := self.sendSequenceDestinations[sendSequence]
	if !ok {
		destinations = map[TransferPath]bool{}
		self.sendSequenceDestinations[sendSequence] = destinations
	}
	destinations[destination] = true
}

// Close cancels every open send sequence. Each sequence's run loop performs
// its own teardown — closing open contracts, draining the resend queue with
// error acks, and closing its multi-route writer — and the sequence removes
// itself from the buffer afterwards.
func (self *SendBuffer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	// the control of the sequence will close it
	for _, sendSequence := range self.sendSequences {
		sendSequence.Cancel()
	}
}

// Cancel is identical to Close: it cancels every open send sequence. The
// sequences shut down through their run-loop teardown and remove themselves
// from the buffer afterwards.
func (self *SendBuffer) Cancel() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, sendSequence := range self.sendSequences {
		sendSequence.Cancel()
	}
}

// Flush cancels every open send sequence, so queued and in-flight items are
// dropped: the resend queue is drained with error acks and pending packs
// are drained when each sequence's run loop finishes. It is called by
// Client.Flush to discard pending sends.
func (self *SendBuffer) Flush() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, sendSequence := range self.sendSequences {
		// if !sendSequenceId.Destination.IsControlDestination() {
		sendSequence.Cancel()
		// }
	}
}

type SendSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	client     *Client
	sendBuffer *SendBuffer
	log        Logger

	destination       TransferPath
	intermediaryIds   MultiHopId
	companionContract bool
	forceStream       bool
	// encryptionRole is the per-peer session role this send sequence uses:
	// client for normal application data (the default), server for
	// EncryptedControl carriers and server-session replies.
	encryptionRole sequenceTlsRole
	// encryptionCompanion is the per-peer session identity companion this
	// sequence uses (distinct from `companionContract`). Keys the acquired
	// session and is stamped on every pack as the `session_companion` wire hint.
	encryptionCompanion bool
	sequenceId          Id

	sendBufferSettings *SendBufferSettings

	// the head contract. this contract is also in `openSendContracts`
	sendContract      *sequenceContract
	sendContractAcked bool
	// contracts are closed when the data are acked
	// these contracts are waiting for acks to close
	openSendContracts map[Id]*sequenceContract

	packMutex sync.Mutex
	packs     chan *SendPack
	ackMutex  sync.Mutex
	acks      chan *protocol.Ack

	resendQueue        *resendQueue
	sendItems          []*sendItem
	nextSequenceNumber uint64

	idleCondition *IdleCondition

	rttWindow *RttWindow

	contractMultiRouteWriter            MultiRouteWriter
	contractMultiRouteWriterDestination TransferPath

	contractSeqIndex uint64

	// session is the per-peer TLS session shared by every local SendSequence
	// and ReceiveSequence to the same peer/stream. Acquired from the
	// `EncryptionSessionManager` at construction; released when the sequence
	// terminates. Nil when encryption is disabled on this client.
	session *peerEncryptionSession
}

func NewSendSequence(
	ctx context.Context,
	client *Client,
	sendBuffer *SendBuffer,
	destination TransferPath,
	intermediaryIds MultiHopId,
	companionContract bool,
	forceStream bool,
	encryptionRole sequenceTlsRole,
	encryptionCompanion bool,
	sendBufferSettings *SendBufferSettings) *SendSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	rttWindow := NewRttWindow(
		client.log,
		sendBufferSettings.RttWindowSize,
		sendBufferSettings.RttWindowTimeout,
		sendBufferSettings.RttScale,
		sendBufferSettings.MinResendInterval,
		sendBufferSettings.MaxResendInterval,
	)

	seq := &SendSequence{
		ctx:                 cancelCtx,
		cancel:              cancel,
		client:              client,
		sendBuffer:          sendBuffer,
		log:                 client.log,
		destination:         destination,
		intermediaryIds:     intermediaryIds,
		companionContract:   companionContract,
		forceStream:         forceStream,
		encryptionRole:      encryptionRole,
		encryptionCompanion: encryptionCompanion,
		sequenceId:          NewId(),
		sendBufferSettings:  sendBufferSettings,
		sendContract:        nil,
		sendContractAcked:   false,
		openSendContracts:   map[Id]*sequenceContract{},
		packs:               make(chan *SendPack, sendBufferSettings.SequenceBufferSize),
		acks:                make(chan *protocol.Ack, sendBufferSettings.AckBufferSize),
		resendQueue:         newResendQueue(sendBufferSettings.ResendQueueBudget, sendBufferSettings.ResendQueueMinByteCount),
		sendItems:           []*sendItem{},
		nextSequenceNumber:  0,
		idleCondition:       NewIdleCondition(),
		rttWindow:           rttWindow,
		contractSeqIndex:    0,
	}
	// Never encrypt control-plane traffic. A SendSequence's data source is
	// always this client (sourceId == client.ClientId()) and its destination
	// is destination.DestinationId; when `SendNoSession` holds for either
	// endpoint, no session is acquired and traffic flows in plaintext.
	if client != nil && client.encryptionSessionManager != nil &&
		!client.encryptionSessionManager.SendNoSession(destination.DestinationId) {
		// Acquire the (peer, encryptionRole) session. A client-role send
		// sequence restarts the handshake (recovery: every new client send
		// re-initiates, rebuilding a peer's lost responder session); a
		// server-role send sequence (EncryptedControl carrier / server
		// reply) never restarts.
		seq.session = client.encryptionSessionManager.AcquireForSend(destination.DestinationId, encryptionRole, encryptionCompanion)
	}
	return seq
}

// ResendQueueSizeAndMessageTypes reports the size of this sequence's resend
// queue: the number of items, their total byte count, this sequence's Id,
// and the protocol message types of the queued items (unmarshalled from
// each item's transfer frame; an item that fails to unmarshal contributes
// no types).
func (self *SendSequence) ResendQueueSizeAndMessageTypes() (int, ByteCount, Id, []protocol.MessageType) {
	unpackMessageTypes := func(item *sendItem) any {
		var messageTypes []protocol.MessageType
		var transferFrame protocol.TransferFrame
		err := proto.Unmarshal(item.transferFrameBytes, &transferFrame)
		if err == nil && transferFrame.Pack != nil {
			for _, frame := range transferFrame.Pack.Frames {
				messageTypes = append(messageTypes, frame.MessageType)
			}
		}
		return messageTypes
	}
	count, byteSize, summary := self.resendQueue.QueueSizeAndSummary(unpackMessageTypes)
	var messageTypes []protocol.MessageType
	for _, summaryMessageTypes := range summary {
		messageTypes = append(messageTypes, summaryMessageTypes.([]protocol.MessageType)...)
	}
	return count, byteSize, self.sequenceId, messageTypes
}

// Pack enqueues `sendPack` on the sequence's pack queue for its run loop to
// send, waiting up to `timeout` for room in the queue. A negative timeout
// waits indefinitely; a zero timeout enqueues only if room is available
// immediately. It returns false when the send pack's context, the sequence
// context, or the timeout fires before the enqueue, or when the sequence's
// idle condition is closed (the run loop is exiting), and true with no
// error once the pack is accepted — at which point the sequence takes
// ownership of the pack's frame message bytes, returning them to the message
// pool once they are sent or the sequence shuts down.
// success, error
func (self *SendSequence) Pack(sendPack *SendPack, timeout time.Duration) (bool, error) {
	self.packMutex.Lock()
	defer self.packMutex.Unlock()

	select {
	case <-sendPack.Ctx.Done():
		return false, errors.New("Done.")
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	if timeout < 0 {
		select {
		case <-sendPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- sendPack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-sendPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- sendPack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-sendPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- sendPack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

// Ack enqueues an incoming `ack` on the sequence's ack queue for its run
// loop to apply, waiting up to `timeout` for room in the queue (a negative
// timeout waits indefinitely, a zero timeout enqueues only if room is
// available immediately). An ack whose SequenceId does not match this
// sequence is rejected without being queued. It returns false when the ack
// is rejected or the sequence context or timeout fires before the enqueue,
// and true with no error once the ack is queued.
func (self *SendSequence) Ack(ack *protocol.Ack, timeout time.Duration) (bool, error) {
	self.ackMutex.Lock()
	defer self.ackMutex.Unlock()

	sequenceId, err := IdFromBytes(ack.SequenceId)
	if err != nil {
		return false, err
	}
	if self.sequenceId != sequenceId {
		// ack is for a different send sequence that no longer exists
		return false, nil
	}

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.acks <- ack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.acks <- ack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.acks <- ack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

// resendBackoff computes the multiplicative resend timeout for an item that
// has been sent sendCount times. The first transmission (sendCount == 1) uses
// the plain scaled RTT; each repeated resend shifts it left by one more bit,
// capped at maxResendInterval so a large shift cannot overflow. Ported from
// upstream (urnetwork/connect): when acks are delayed (not lost) by queueing,
// a flat timeout re-sends the whole in-flight window every interval, and the
// duplicates feed the congestion that delayed the acks in the first place.
//
// The shift saturates at 16 rather than growing until the cap binds, so for a
// scaled RTT below roughly 122µs (maxResendInterval >> 16 at the 8s default)
// the interval plateaus BELOW maxResendInterval and stops growing, even though
// MaxResendCount == 0 permits unlimited retransmissions. That is upstream's
// behavior and is kept deliberately: this helper exists to align with upstream,
// and a saturating loop would fork the exact code the alignment is for. Real
// relay paths do not produce scaled RTTs that small, and the 8s cap was tuned
// against this shift design. If a transport ever does land in that range,
// changing it is an upstream conversation, not a local patch.
func resendBackoff(scaledRtt time.Duration, sendCount int, maxResendInterval time.Duration) time.Duration {
	if shift := uint(min(sendCount-1, 16)); 0 < shift {
		return min(scaledRtt<<shift, maxResendInterval)
	}
	return scaledRtt
}

// Run is the sequence's main loop, started once per SendSequence by
// SendBuffer.Pack and blocking until the sequence terminates. Each iteration
// applies the pending acks (see receiveAck), retransmits items whose resend
// time has arrived (backing off multiplicatively, see resendBackoff), and
// then waits for an ack, a new pack, or the idle/ack timer. It exits when
// the sequence context is canceled (Cancel or Close), when the head item
// exceeds its ack timeout, when the idle timeout elapses with an empty
// resend queue, or when no contract can be created for an incoming pack.
// On exit the deferred teardown cancels the context, closes every open
// contract with its acked and unacked byte counts, drains the resend queue
// (invoking each item's ack callback with an error and returning its bytes
// to the message pool), flushes the contract queue, closes the multi-route
// writer, and releases the peer encryption session.
func (self *SendSequence) Run() {
	defer func() {
		if r := recover(); r != nil {
			self.log.Errorf("[s]%s->%s...%s s(%s) abnormal exit =  %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, r)
			panic(r)
		}
	}()
	defer func() {
		self.cancel()

		// close contract
		for _, sendContract := range self.openSendContracts {
			self.client.ContractManager().CloseContract(
				sendContract.contractId,
				sendContract.ackedByteCount,
				sendContract.unackedByteCount,
			)
			// flush queued contracts for already sent contracts
			// contractKey = ContractKey{
			// 	Destination:       sendContract.path.DestinationMask(),
			// 	IntermediaryIds:   self.intermediaryIds,
			// 	CompanionContract: self.companionContract,
			// 	ForceStream:       self.forceStream,
			// }
			// self.client.ContractManager().FlushContractQueue(contractKey, true)
		}

		// drain the buffer
		for _, item := range self.resendQueue.Clear() {
			safeAck(item.ackCallback, errors.New("Send sequence closed."))
			item.messagePoolReturn()
		}

		// flush queued contracts (used ids were closed above). Keyed by
		// (EncryptionRole, EncryptionCompanion) so this exit-flush doesn't discard
		// a peer-paired sequence's pending contracts — the EC carrier and normal
		// data are separate sequences to the same destination.
		contractKey := ContractKey{
			Destination:         self.destination,
			IntermediaryIds:     self.intermediaryIds,
			CompanionContract:   self.companionContract,
			ForceStream:         self.forceStream,
			EncryptionRole:      self.encryptionRole,
			EncryptionCompanion: self.encryptionCompanion,
		}
		self.client.ContractManager().FlushContractQueue(contractKey, true)

		self.closeContractMultiRouteWriter()

		if self.session != nil {
			// No explicit close: a closing SendSequence must not tear down
			// the shared session (a concurrent ReceiveSequence may still be
			// using it) and must not emit anything on the wire. A future
			// initiator SendSequence resets the handshake when it resumes.
			self.session.Release()
		}
	}()

	ackWindow := newSequenceAckWindow()
	go HandleError(func() {
		defer self.cancel()

		for {
			select {
			case <-self.ctx.Done():
				return
			case ack, ok := <-self.acks:
				if !ok {
					return
				}
				if messageId, err := IdFromBytes(ack.MessageId); err == nil {
					if sequenceNumber, ok := self.resendQueue.ContainsMessageId(messageId); ok {
						ack := &sequenceAck{
							messageId:      messageId,
							sequenceNumber: sequenceNumber,
							selective:      ack.Selective,
							tag:            ack.Tag,
						}
						ackWindow.Update(ack)
					}
				}
			}
		}
	}, self.cancel)

	// reusable idle/resend timer: a per-iteration time.After would allocate a
	// timer per packet on this hot loop. created already-fired; the Reset before
	// each blocking select arms it (go1.23+ delivers no stale fire after Reset).
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		// apply the acks
		ackSnapshot := ackWindow.Snapshot(true)
		if 0 < ackSnapshot.ackUpdateCount {
			self.receiveAck(ackSnapshot.headAck.messageId, false, ackSnapshot.headAck.tag)
		}
		for messageId, ack := range ackSnapshot.selectiveAcks {
			self.receiveAck(messageId, true, ack.tag)
		}

		sendTime := time.Now()
		var timeout time.Duration

		if self.resendQueue.Len() == 0 {
			timeout = self.sendBufferSettings.IdleTimeout
		} else {
			timeout = self.sendBufferSettings.AckTimeout

			for {
				item := self.resendQueue.PeekFirst()
				if item == nil {
					break
				}

				itemAckTimeout := item.sendTime.Add(self.sendBufferSettings.AckTimeout).Sub(sendTime)
				if itemAckTimeout <= 0 {
					// message took too long to ack
					// close the sequence
					self.log.V(1).Infof("[s]%s->%s...%s s(%s) exit ack timeout (%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, self.sendBufferSettings.AckTimeout)
					return
				}
				if itemAckTimeout < timeout {
					timeout = itemAckTimeout
				}

				if sendTime.Before(item.resendTime) {
					itemResendTimeout := item.resendTime.Sub(sendTime)
					if itemResendTimeout < timeout {
						timeout = itemResendTimeout
					}
					break
				}

				self.resendQueue.RemoveByMessageId(item.messageId)

				// resend
				var transferFrameBytes []byte
				if self.sendItems[0].sequenceNumber == item.sequenceNumber && !item.head {
					// set `head=true`
					var err error
					transferFrameBytes, err = self.setHead(item)
					if err != nil {
						self.log.Errorf("[s]%s->%s...%s s(%s) exit could not set head = %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, err)
						return
					}
					MessagePoolReturn(item.transferFrameBytes)
					item.head = true
					item.transferFrameBytes = transferFrameBytes
				} else {
					// var err error
					// transferFrameBytes, err = self.setTag(item)
					// if err != nil {
					// 	glog.Errorf("[s]%s->%s...%s s(%s) exit could not set tag = %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, err)
					// 	return
					// }
					transferFrameBytes = item.transferFrameBytes
				}

				// resend uses the same path the item was originally sent on
				resendPath := self.destination.AddSource(self.client.ClientId())
				resendBytes := transferFrameBytes
				resendForceUnwrapped := item.forceUnwrapped
				c := func() error {
					return self.writeMaybeWrappedBytes(resendBytes, resendPath, resendForceUnwrapped)
				}
				if self.log.V(2).Enabled() {
					TraceWithReturn(
						fmt.Sprintf(
							"[s]resend %d multi route write %s->%s...%s s(%s)",
							item.sequenceNumber,
							self.client.ClientTag(),
							self.intermediaryIds,
							self.destination.DestinationId,
							self.destination.StreamId,
						),
						c,
					)
				} else {
					err := c()
					if err != nil {
						self.log.V(1).Infof("[s]resend drop = %s", err)
					}
				}

				item.sendCount += 1
				maxResendCount := self.sendBufferSettings.MaxResendCount
				if maxResendCount > 0 && item.sendCount >= maxResendCount {
					self.log.V(1).Infof("[s]resend cap park after %d sends %s->%s...%s s(%s)\n", item.sendCount, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
					item.resendTime = sendTime.Add(self.sendBufferSettings.AckTimeout)
					self.resendQueue.Add(item)
					continue
				}
				// back off the resend timeout multiplicatively with each resend
				// of the same item, up to `MaxResendInterval`. When acks are
				// delayed (not lost) by queueing, a flat timeout re-sends the
				// whole in-flight window every interval, and the duplicates
				// feed the congestion that delayed the acks in the first place.
				itemResendTimeout := resendBackoff(
					self.rttWindow.ScaledRtt(),
					item.sendCount,
					self.sendBufferSettings.MaxResendInterval,
				)
				if itemAckTimeout <= itemResendTimeout {
					item.resendTime = sendTime.Add(itemAckTimeout)
				} else {
					item.resendTime = sendTime.Add(itemResendTimeout)
				}
				self.resendQueue.Add(item)
			}
		}

		checkpointId := self.idleCondition.Checkpoint()

		// approximate since this cannot consider the next message byte size
		canQueue := func() bool {
			return self.resendQueue.CanAdd(0, self.sendBufferSettings.ResendQueueMaxByteCount)
		}
		if !canQueue() {
			// wait for acks
			idleTimer.Reset(timeout)
			select {
			case <-self.ctx.Done():
				return
			case <-ackSnapshot.ackNotify:
			case <-idleTimer.C:
				if 0 == self.resendQueue.Len() {
					done := false
					func() {
						self.packMutex.Lock()
						defer self.packMutex.Unlock()
						// idle timeout
						if self.idleCondition.Close(checkpointId) {
							done = true
						}
						// else there are pending updates
					}()
					if done {
						// close the sequence
						self.log.V(2).Infof("[s]%s->%s...%s s(%s) exit idle timeout\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
						return
					}
				}
			}
		} else {
			idleTimer.Reset(timeout)
			select {
			case <-self.ctx.Done():
				return
			case <-ackSnapshot.ackNotify:
			case sendPack, ok := <-self.packs:
				if !ok {
					return
				}

				// note messages of `size < MinMessageByteCount` get counted as `MinMessageByteCount` against the contract
				if self.updateContract(sendPack.MessageByteCount) {
					self.send(sendPack.Frame, sendPack.AckCallback, sendPack.Ack, sendPack.ForceUnwrapped)
					// ignore the error since there will be a retry
				} else {
					// no contract
					// close the sequence
					self.log.Errorf("[s]%s->%s...%s s(%s) exit could not create contract.\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
					safeAck(sendPack.AckCallback, errors.New("No contract"))
					MessagePoolReturn(sendPack.Frame.MessageBytes)
					return
				}
			case <-idleTimer.C:
				if 0 == self.resendQueue.Len() {
					done := false
					func() {
						self.packMutex.Lock()
						defer self.packMutex.Unlock()
						// idle timeout
						if self.idleCondition.Close(checkpointId) {
							done = true
						}
						// else there are pending updates
					}()
					if done {
						// close the sequence
						self.log.V(2).Infof("[s]%s->%s...%s s(%s) exit idle timeout\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
						return
					}
				}
			}
		}
	}
}

func computeFillFraction(meanRtt time.Duration, fallback float32) float32 {
	if meanRtt == 0 {
		return fallback
	}
	ms := float64(meanRtt / time.Millisecond)
	const high = 0.85
	const low = 0.7
	if ms <= 100 {
		return float32(high)
	}
	if ms >= 1000 {
		return float32(low)
	}
	return float32(high - (high-low)*(ms-100)/900)
}

// contractFillFraction returns the fill fraction used to size this
// sequence's contracts: the mean-RTT-based value from computeFillFraction
// (0.85 at RTTs up to 100ms, decaying to 0.7 at 1000ms), or the configured
// SendBufferSettings.ContractFillFraction when the RTT window has no data.
// The fraction sizes the contract's transfer byte count so a contract
// remains open while its follow-up is being created; as it approaches 1,
// no-ack messages are more likely to be dropped by out-of-sync contracts
// (see SendBufferSettings).
func (self *SendSequence) contractFillFraction() float32 {
	meanRtt := self.rttWindow.MeanRtt()
	return computeFillFraction(meanRtt, self.sendBufferSettings.ContractFillFraction)
}

// updateContract reports whether a message of `messageByteCount` fits under
// the current contract, debiting the bytes against it (a message below
// MinMessageByteCount is counted as MinMessageByteCount). When the current
// contract is full or absent, it takes the queued contract from the contract
// manager — retrying with CreateContractRetryInterval backoff until
// CreateContractTimeout, or until the context is canceled — installs it via
// setContract, and queues a contract-carrying pack whose ack marks the
// contract acked. It returns false when no contract could be established; a
// message larger than a standard contract can never be sent and panics. When
// the contract manager is configured to require no contract (both sides must
// agree), it always returns true without touching contracts.
func (self *SendSequence) updateContract(messageByteCount ByteCount) bool {
	// `sendNoContract` is a mutual configuration
	// both sides must configure themselves to require no contract from each other
	if self.client.ContractManager().SendNoContract(self.destination.DestinationId) {
		return true
	}
	if self.sendContract != nil && self.sendContract.update(messageByteCount) {
		return true
	}

	createContract := func() bool {
		// the max overhead of the pack frame
		// this is needed because the size of the contract pack is counted against the contract
		// maxContractMessageByteCount := ByteCount(256)

		effectiveContractTransferByteCount := ByteCount(float32(self.client.ContractManager().StandardContractTransferByteCount()) * self.contractFillFraction())
		if effectiveContractTransferByteCount < messageByteCount+self.sendBufferSettings.MinMessageByteCount /*+ maxContractMessageByteCount*/ {
			// this pack does not fit into a standard contract
			// TODO allow requesting larger contracts
			panic(fmt.Errorf("Message too large for contract. It can never be sent (%d).", messageByteCount))
		}

		setNextContract := func(contract *protocol.Contract) bool {
			nextSendContract, err := newSequenceContract(
				self.log,
				"s",
				contract,
				self.sendBufferSettings.MinMessageByteCount,
				self.contractFillFraction(),
			)
			if err != nil {
				// malformed
				self.log.Errorf("[s]%s->%s...%s s(%s) exit next contract malformed error = %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, err)
				return false
			}

			if _, ok := self.openSendContracts[nextSendContract.contractId]; ok {
				return false
			}

			// note `update(0)` will use `MinMessageByteCount` byte count
			// the min message byte count is used to avoid spam
			if nextSendContract.update(0) && nextSendContract.update(messageByteCount) {
				self.setContract(nextSendContract)

				// append the contract to the sequence
				self.sendWithSetContract(nil, func(error) {
					self.setContractAcked(nextSendContract, true)
				}, true, true, false)

				// FIXME
				self.log.Infof("[s]%s->%s...%s s(%s) contract set %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, nextSendContract.contractId)

				return true

			} else {
				// this contract doesn't fit the message
				// the contract was requested with the correct size, so this is an error somewhere
				// just close it and let the platform time out the other side
				self.log.Errorf("[s]%s->%s...%s s(%s) contract too small %s\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId, nextSendContract.contractId)
				self.client.ContractManager().CloseContract(nextSendContract.contractId, 0, 0)
				return false
			}
		}

		nextContract := func(timeout time.Duration) bool {
			contractKey := ContractKey{
				Destination:         self.destination,
				IntermediaryIds:     self.intermediaryIds,
				CompanionContract:   self.companionContract,
				ForceStream:         self.forceStream,
				EncryptionRole:      self.encryptionRole,
				EncryptionCompanion: self.encryptionCompanion,
			}
			if contract := self.client.ContractManager().TakeContract(self.ctx, contractKey, timeout); contract != nil && setNextContract(contract) {
				self.contractSeqIndex += 1
				// async queue up the next contract
				self.client.ContractManager().CreateContract(
					contractKey,
					self.contractSeqIndex,
					ByteCount(32+float32(messageByteCount+self.sendBufferSettings.MinMessageByteCount)/self.contractFillFraction()),
				)
				return true
			} else {
				return false
			}
		}
		traceNextContract := func(timeout time.Duration) bool {
			if self.log.V(2).Enabled() {
				return TraceWithReturn(
					fmt.Sprintf("[s]%s->%s...%s s(%s) next contract", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId),
					func() bool {
						return nextContract(timeout)
					},
				)
			} else {
				return nextContract(timeout)
			}
		}

		endTime := time.Now().Add(self.sendBufferSettings.CreateContractTimeout)

		// back off contract retries when the backend is unreachable to reduce API storm
		contractRetryInterval := self.sendBufferSettings.CreateContractRetryInterval
		if isBackendDegraded() {
			contractRetryInterval = 30 * time.Second
		}

		if self.sendContract != nil {
			// there should be a queued up contract
			if traceNextContract(min(self.sendBufferSettings.CreateContractTimeout, contractRetryInterval)) {
				return true
			}
		}

		for {
			select {
			case <-self.ctx.Done():
				return false
			default:
			}

			timeout := endTime.Sub(time.Now())
			if timeout <= 0 {
				return false
			}

			// async queue up the next contract; skip when backend is degraded
			// to avoid launching goroutines against a dead API
			if !isBackendDegraded() {
				contractKey := ContractKey{
					Destination:         self.destination,
					IntermediaryIds:     self.intermediaryIds,
					CompanionContract:   self.companionContract,
					ForceStream:         self.forceStream,
					EncryptionRole:      self.encryptionRole,
					EncryptionCompanion: self.encryptionCompanion,
				}
				self.client.ContractManager().CreateContract(
					contractKey,
					self.contractSeqIndex,
					ByteCount(32+float32(messageByteCount+messageByteCount+self.sendBufferSettings.MinMessageByteCount)/self.contractFillFraction()),
				)
			}

			if traceNextContract(min(timeout, contractRetryInterval)) {
				return true
			}
		}
	}

	if self.log.V(2).Enabled() {
		return TraceWithReturn(
			fmt.Sprintf("[s]create contract c=%t %s->%s...%s s(%s)", self.companionContract, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId),
			createContract,
		)
	} else {
		return createContract()
	}
}

// setContract makes `nextSendContract` the sequence's head contract: it is
// registered in openSendContracts, becomes the sendContract, its contract
// stats are registered, and its destination TLS identity (client public key
// and certificate chain commitment) is folded into the peer session's
// trusted set when a session exists. The previous contract is closed
// immediately only when it carries no unacked data; otherwise it stays in
// openSendContracts and is closed by ackItem once its data is acked.
func (self *SendSequence) setContract(nextSendContract *sequenceContract) {
	// do not close the current contract unless it has no pending data
	// the contract is tracked in `openSendContracts` and will be closed on ack
	if self.sendContract != nil && self.sendContract.unackedByteCount == 0 {
		self.client.ContractManager().CloseContract(
			self.sendContract.contractId,
			self.sendContract.ackedByteCount,
			self.sendContract.unackedByteCount,
		)
		delete(self.openSendContracts, self.sendContract.contractId)
	}
	self.openSendContracts[nextSendContract.contractId] = nextSendContract
	self.sendContract = nextSendContract
	self.sendContractAcked = false
	nextSendContract.contractStatsEntry = self.client.ContractManager().registerContractStats(
		nextSendContract.contractId,
		false,
		self.companionContract,
		nextSendContract.path,
		nextSendContract.transferByteCount,
	)
	// The contract carries the destination's `ProvideTlsCertificate`
	// commitment (possibly empty). Fold the chain into the session's
	// trusted-peer-cert set so the peer's TLS-handshake cert can be matched
	// against any cert version the destination has ever published — both
	// the cert in this contract and any cert in a previously seen contract.
	// An empty chain turns off verification entirely (the destination is
	// not committing to a TLS identity).
	//
	// In addition, contracts carry the destination's long-lived client
	// public identity key plus the destination's signature over the cert
	// chain by that key. Pass the public key to the session so it can
	// (a) verify the cert chain before trusting it (defeats a platform
	// MITM that substitutes the cert), and (b) verify the post-handshake
	// identity proof exchanged inside the per-peer TLS session (defeats
	// an active MITM that re-handshakes TLS on each leg).
	if self.session != nil {
		if 0 < len(nextSendContract.destinationClientPublicKey) {
			self.session.SetPeerClientPublicKey(ed25519.PublicKey(nextSendContract.destinationClientPublicKey))
		}
		self.session.AddTrustedPeerCertChain(
			nextSendContract.provideTlsCertificate,
			nextSendContract.destinationClientKeySignedTlsCertificate,
		)
	}
}

// setContractAcked records whether `nextSendContract` has been
// acknowledged by the receiver, but only while it is still the current head
// contract. Until it is acked, sendWithSetContract forces ack=true on new
// items so no-ack messages cannot race ahead of the contract itself.
func (self *SendSequence) setContractAcked(nextSendContract *sequenceContract, ack bool) {
	if self.sendContract == nextSendContract {
		self.sendContractAcked = ack
	}
}

// send builds and writes one transfer frame carrying `frame` to the
// sequence's destination and tracks the item for ack (see
// sendWithSetContract). `ackCallback` fires with nil once the item is
// acked, or with an error when the item is dropped or the sequence shuts
// down. It is invoked by the run loop for each pack it dequeues.
func (self *SendSequence) send(
	frame *protocol.Frame,
	ackCallback AckFunction,
	ack bool,
	forceUnwrapped bool,
) {
	self.sendWithSetContract(frame, ackCallback, ack, false, forceUnwrapped)
}

// sendWithSetContract builds a transfer frame for one message, writes it
// through the contract multi-route writer, and tracks it for retransmission
// and ack. An acked item gets a fresh sequence number and becomes the head
// item when it is the first in the sequence; a no-ack (nack) item gets no
// sequence number and is acked locally on a successful write instead of
// being tracked for resend. When `setContract` is set, or the item is the
// head with a contract in place, a TransferContract frame carrying the
// current contract is attached to the pack. While the head contract is not
// yet acked, a requested no-ack item is sent as an ack so it cannot race
// ahead of the contract. The message frame bytes are returned to the message
// pool once the wire frame is built; the resulting transfer frame bytes are
// owned by the send item until it is acked or dropped. Write errors on
// acked items are ignored — the run loop retransmits them.
func (self *SendSequence) sendWithSetContract(
	frame *protocol.Frame,
	ackCallback AckFunction,
	ack bool,
	setContract bool,
	forceUnwrapped bool,
) {
	sendTime := time.Now()
	messageId := NewId()

	var contractId *Id
	if self.sendContract != nil {
		contractId = &self.sendContract.contractId

		if !self.sendContractAcked {
			// (see note above about contracts and nack)
			// send nack messages as ack until the send contract is acked
			// this avoid racing the messages with the contract
			ack = true
		}
	}

	var head bool
	var sequenceNumber uint64
	if ack {
		head = (0 == len(self.sendItems))
		sequenceNumber = self.nextSequenceNumber
		self.nextSequenceNumber += 1
	} else {
		head = false
		sequenceNumber = 0
	}

	var contractFrame *protocol.Frame
	if (head || setContract) && self.sendContract != nil {
		contractMessageBytes, _ := ProtoMarshal(self.sendContract.contract)
		contractFrame = &protocol.Frame{
			MessageType:  protocol.MessageType_TransferContract,
			MessageBytes: contractMessageBytes,
		}
		defer MessagePoolReturn(contractMessageBytes)
	}

	frames := []*protocol.Frame{}
	if frame != nil {
		frames = append(frames, frame)
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()

	// var path TransferPath
	// if self.sendContract == nil {
	// 	path = self.destination.AddSource(self.client.ClientId())
	// } else {
	// 	path = self.sendContract.path.LocalMask()
	// }
	path := self.destination.AddSource(self.client.ClientId())
	messageByteCount := MessageByteCount(frames)

	// Session role/companion stamping (applies to both encodings below):
	// A server-role sequence is the peer's EncryptedControl carrier. Stamp its
	// role on every pack — including the non-EC open/contract packs that carry no
	// EC frame to derive it from — so the receiver maps the whole sequence to one
	// complement session; otherwise the open pack splits into a separate receive
	// sequence and the handshake bytes (ServerHello, identity proof) gap forever.
	// Only the server role is marked: a client-role stream is the unencrypted
	// default, already the receiver's complement, so it stays off the wire.
	// Companion mirrors the role stamp (for either role): stamped only when true,
	// since false is the receiver's default and stays off the wire.

	var transferFrameBytes []byte
	if 2 <= self.sendBufferSettings.ProtocolVersion {
		// hand-rolled marshal of the hot TransferFrame{Pack}: wire-identical to
		// the proto structs in the legacy branch below (verified byte-for-byte in
		// frame_protobuf_test.go), without the intermediate Pack/TransferFrame/Tag/
		// TransferPath structs, the Id.Bytes() escapes, or reflection.
		spf := sendPackFrame{
			path:           path,
			messageId:      messageId,
			sequenceId:     self.sequenceId,
			sequenceNumber: sequenceNumber,
			head:           head,
			nack:           !ack,
			frames:         frames,
			contractFrame:  contractFrame,
			tagSendTime:    uint64(sendTime.UnixMilli()),
		}
		if !ack && contractId != nil {
			spf.contractId = contractId
		}
		if self.encryptionRole == sequenceTlsRoleServer {
			spf.sessionRole = self.encryptionRole.toProtobuf()
			spf.sessionRoleSet = true
		}
		if self.encryptionCompanion {
			spf.companion = true
		}
		transferFrameBytes = marshalSendPackTransferFrame(&spf)
	} else {
		// legacy (<v2) path: build and marshal via the proto structs.
		pack := &protocol.Pack{
			MessageId:      messageId.Bytes(),
			SequenceId:     self.sequenceId.Bytes(),
			SequenceNumber: sequenceNumber,
			Head:           head,
			Frames:         frames,
			ContractFrame:  contractFrame,
			Nack:           !ack,
			Tag:            self.rttWindow.OpenTag(),
		}
		if !ack && contractId != nil {
			pack.ContractId = contractId.Bytes()
		}
		packBytes, _ := ProtoMarshal(pack)
		defer MessagePoolReturn(packBytes)
		transferFrame := &protocol.TransferFrame{
			TransferPath: path.ToProtobuf(),
			Frame: &protocol.Frame{
				MessageType:  protocol.MessageType_TransferPack,
				MessageBytes: packBytes,
			},
		}
		if self.encryptionRole == sequenceTlsRoleServer {
			sessionRole := self.encryptionRole.toProtobuf()
			transferFrame.SessionRole = &sessionRole
		}
		if self.encryptionCompanion {
			sessionCompanion := true
			transferFrame.SessionCompanion = &sessionCompanion
		}
		transferFrameBytes, _ = ProtoMarshal(transferFrame)
	}

	item := &sendItem{
		transferItem: transferItem{
			messageId:        messageId,
			sequenceNumber:   sequenceNumber,
			messageByteCount: messageByteCount,
		},
		contractId:         contractId,
		sendTime:           sendTime,
		resendTime:         sendTime.Add(self.rttWindow.ScaledRtt()),
		sendCount:          1,
		head:               head,
		hasContractFrame:   (contractFrame != nil),
		transferFrameBytes: transferFrameBytes,
		ackCallback:        ackCallback,
		forceUnwrapped:     forceUnwrapped,
	}

	c := func() error {
		return self.writeMaybeWrappedBytes(item.transferFrameBytes, path, item.forceUnwrapped)
	}
	var err error
	if self.log.V(2).Enabled() {
		err = TraceWithReturn(
			fmt.Sprintf("[s]multi route write %s->%s...%s s(%s)", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId),
			c,
		)
	} else {
		err = c()
		if err != nil {
			if v := self.log.V(1); v.Enabled() {
				v.Infof("[s]drop = %s", err)
			}
		}
	}

	if ack {
		self.sendItems = append(self.sendItems, item)
		self.resendQueue.Add(item)
		// ignore the write error since the item will be resent
	} else {
		// immediately ack
		if err == nil {
			self.ackItem(item)
		} else {
			safeAck(item.ackCallback, err)
			item.messagePoolReturn()
		}
	}
}

// setHead rebuilds the transfer frame of `item` for a head retransmission:
// it sets the pack's head flag, stamps a fresh RTT tag, and attaches the
// item's contract frame if the original send did not carry one. It returns
// the new marshalled transfer frame bytes; the caller (the run loop's resend
// path) replaces the item's bytes with them and returns the previous bytes
// to the message pool. The returned bytes are owned by the caller and are
// returned to the message pool with the item when it completes or is
// dropped.
func (self *SendSequence) setHead(item *sendItem) ([]byte, error) {
	if v := self.log.V(1); v.Enabled() {
		v.Infof("[s]set head %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}

	var transferFrame protocol.TransferFrame
	err := ProtoUnmarshal(item.transferFrameBytes, &transferFrame)
	if err != nil {
		return nil, err
	}

	var pack *protocol.Pack
	if transferFrame.Pack != nil {
		pack = transferFrame.Pack
	} else {
		pack = &protocol.Pack{}
		err = ProtoUnmarshal(transferFrame.Frame.MessageBytes, pack)
		if err != nil {
			return nil, err
		}
	}

	pack.Head = true
	pack.Tag = self.rttWindow.OpenTag()
	// attach the contract frame to the head
	if item.contractId != nil && !item.hasContractFrame {
		sendContract := self.openSendContracts[*item.contractId]
		contractMessageBytes, _ := ProtoMarshal(sendContract.contract)
		pack.ContractFrame = &protocol.Frame{
			MessageType:  protocol.MessageType_TransferContract,
			MessageBytes: contractMessageBytes,
		}
		defer MessagePoolReturn(contractMessageBytes)
	}

	if transferFrame.Pack != nil {
		transferFrame.Pack = pack
	} else {
		packBytes, err := ProtoMarshal(pack)
		if err != nil {
			return nil, err
		}
		defer MessagePoolReturn(packBytes)
		transferFrame.Frame.MessageBytes = packBytes
	}

	transferFrameBytesWithHead, err := ProtoMarshal(&transferFrame)
	if err != nil {
		return nil, err
	}

	return transferFrameBytesWithHead, nil
}

/*
func (self *SendSequence) setTag(item *sendItem) ([]byte, error) {
	if v := self.log.V(1); v.Enabled() {
		v.Infof("[s]set tag %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}

	var transferFrame protocol.TransferFrame
	err := proto.Unmarshal(item.transferFrameBytes, &transferFrame)
	if err != nil {
		return nil, err
	}

	var pack protocol.Pack
	err = proto.Unmarshal(transferFrame.Frame.MessageBytes, &pack)
	if err != nil {
		return nil, err
	}

	pack.Tag = self.rttWindow.OpenTag()

	packBytes, err := proto.Marshal(&pack)
	if err != nil {
		return nil, err
	}
	transferFrame.Frame.MessageBytes = packBytes

	transferFrameBytesWithTag, err := proto.Marshal(&transferFrame)
	if err != nil {
		return nil, err
	}

	return transferFrameBytesWithTag, nil
}
*/

// receiveAck applies an ack for `messageId` to the sequence's resend queue.
// An ack for a message not pending resend is ignored. A selective ack keeps
// the item pending but defers its retransmission by SelectiveAckTimeout,
// on the assumption that the receiver holds the message; a non-selective ack
// is cumulative, completing the item and every earlier item still in
// sendItems via ackItem. A non-nil `tag` is closed into the RTT window.
func (self *SendSequence) receiveAck(messageId Id, selective bool, tag *protocol.Tag) {
	item := self.resendQueue.GetByMessageId(messageId)
	if item == nil {
		if v := self.log.V(1); v.Enabled() {
			v.Infof("[s]ack miss %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
		}
		// message not pending ack
		return
	}

	if tag != nil {
		self.rttWindow.CloseTag(tag)
	}

	if selective {
		if v := self.log.V(1); v.Enabled() {
			v.Infof("[s]ack selective %s->%s...%s s(%s)\n", self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
		}
		removed := self.resendQueue.RemoveByMessageId(messageId)
		if removed == nil {
			panic(errors.New("Missing item"))
		}
		item.resendTime = time.Now().Add(self.sendBufferSettings.SelectiveAckTimeout)
		item.sendTime = time.Now()
		self.resendQueue.Add(item)
		return
	}

	if v := self.log.V(1); v.Enabled() {
		v.Infof("[s]ack %d %s->%s...%s s(%s)\n", item.sequenceNumber, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}

	// acks are cumulative
	// implicitly ack all earlier items in the sequence
	i := 0
	for ; i < len(self.sendItems); i += 1 {
		implicitItem := self.sendItems[i]
		if item.sequenceNumber < implicitItem.sequenceNumber {
			if v := self.log.V(2); v.Enabled() {
				v.Infof("[s]ack %d <> %d/%d (stop) %s->%s...%s s(%s)\n", item.sequenceNumber, implicitItem.sequenceNumber, self.nextSequenceNumber-1, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
			}
			break
		}

		var a int
		var b ByteCount
		if self.log.V(2).Enabled() {
			a, b = self.resendQueue.QueueSize()
		}

		// self.ackedSequenceNumbers[implicitItem.sequenceNumber] = true
		removed := self.resendQueue.RemoveByMessageId(implicitItem.messageId)
		if removed == nil {
			panic(errors.New("Missing item"))
		}

		self.ackItem(implicitItem)
		self.sendItems[i] = nil

		if self.log.V(2).Enabled() {
			c, d := self.resendQueue.QueueSize()
			self.log.Infof("[s]ack %d <> %d/%d (pass %d->%d %dB->%dB) %s->%s...%s s(%s)\n", item.sequenceNumber, implicitItem.sequenceNumber, self.nextSequenceNumber-1, a, c, b, d, self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
		}
	}
	self.sendItems = self.sendItems[i:]
	if self.log.V(2).Enabled() {
		a, b := self.resendQueue.QueueSize()
		self.log.Infof("[s]ack %d/%d (stop %d %dB %d) %s->%s...%s s(%s)\n", item.sequenceNumber, self.nextSequenceNumber-1, a, b, len(self.sendItems), self.client.ClientTag(), self.intermediaryIds, self.destination.DestinationId, self.destination.StreamId)
	}
}

// ackItem completes an item whose data has been acknowledged: it accounts
// the item's bytes against its contract and updates the contract stats,
// closes the contract when it is not the current head contract and has no
// unacked data left, invokes the item's ack callback with a nil error, and
// returns the item's transfer frame bytes to the message pool.
func (self *SendSequence) ackItem(item *sendItem) {
	if item.contractId != nil {
		if itemSendContract, ok := self.openSendContracts[*item.contractId]; ok {
			itemSendContract.ack(item.messageByteCount)
			if itemSendContract.contractStatsEntry != nil {
				itemSendContract.contractStatsEntry.updateUsedByteCount(itemSendContract.ackedByteCount)
			}
			// not current and closed
			if self.sendContract != itemSendContract && itemSendContract.unackedByteCount == 0 {
				self.client.ContractManager().CloseContract(
					itemSendContract.contractId,
					itemSendContract.ackedByteCount,
					itemSendContract.unackedByteCount,
				)
				delete(self.openSendContracts, itemSendContract.contractId)
			}
		}
	}
	safeAck(item.ackCallback, nil)
	// MessagePoolReturn(item.transferFrameBytes)
	// for _, frame := range item.frames {
	// 	MessagePoolReturn(frame.MessageBytes)
	// }
	item.messagePoolReturn()
}

// writeMaybeWrappedBytes writes `transferFrameBytes` through the contract
// multi-route writer. When the per-peer session has a cipher, the bytes are
// outer-wrapped as `TransferFrame{TransferPath, encryptedTransferFrame:
// <ciphertext>}` before being written. Encryption is a binary property of
// the session: cipher set → wrap; cipher nil → pass-through. `path` is the
// wire TransferPath the outer wrap reproduces so forwarders see the same
// routing path either way.
//
// `forceUnwrapped` pins this frame to plaintext regardless of cipher
// state. TLS handshake EncryptedControl frames use this to keep the
// handshake bootstrap legible to the peer — including on retransmit,
// where the local cipher may have become available after the original
// send but the peer has not yet completed its half of the handshake.
//
// Before wrapping, the peer's TLS certificate is verified against the
// active contract's `ProvideTlsCertificate` commitment. A mismatch is a
// loud error: the frame is dropped (the SendSequence will retry, and
// eventually time out, rather than transmit application data sealed under
// the wrong identity).
func (self *SendSequence) writeMaybeWrappedBytes(transferFrameBytes []byte, path TransferPath, forceUnwrapped bool) error {
	writer := self.openContractMultiRouteWriter()
	var cipher *sequenceCipher
	if self.session != nil && !forceUnwrapped {
		cipher = self.session.Cipher()
	}
	if cipher == nil {
		if v := self.log.V(2); v.Enabled() {
			v.Infof(
				"[s]%s->%s s(%s) write plaintext %d bytes (forceUnwrapped=%t, session=%t, cipher=nil)\n",
				self.client.ClientTag(),
				self.destination.DestinationId,
				self.destination.StreamId,
				len(transferFrameBytes),
				forceUnwrapped,
				self.session != nil,
			)
		}
		bytes := transferFrameBytes
		if DebugTransferCopyOnWrite {
			bytes = MessagePoolCopy(transferFrameBytes)
		}
		return writer.Write(self.ctx, MessagePoolShareReadOnly(bytes), self.sendBufferSettings.WriteTimeout)
	}
	if err := self.verifyPeerCertAgainstContract(); err != nil {
		return err
	}
	ciphertext, err := cipher.Seal(transferFrameBytes)
	if err != nil {
		return fmt.Errorf("outer wrap seal: %w", err)
	}
	if cipher.ShouldRekey() {
		// bound the number of messages sealed under one AEAD key (see
		// sequenceCipherMaxSeals); non-disruptive, established cipher keeps
		// serving until the new epoch's handshake completes
		self.session.restartHandshake()
	}
	// Carry the wrapping session's role + companion as the destination's
	// decrypt hint; the destination routes to its complement role / matching
	// companion session.
	wrapped, err := buildEncryptedOuterFrameBytes(path, ciphertext, self.session.role.toProtobuf(), self.session.companion)
	if err != nil {
		return fmt.Errorf("outer wrap marshal: %w", err)
	}
	if v := self.log.V(2); v.Enabled() {
		v.Infof(
			"[s]%s->%s s(%s) write wrapped %d -> %d bytes\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			len(transferFrameBytes), len(wrapped),
		)
	}
	defer MessagePoolReturn(wrapped)
	return writer.Write(self.ctx, MessagePoolShareReadOnly(wrapped), self.sendBufferSettings.WriteTimeout)
}

// verifyPeerCertAgainstContract checks (and caches) that the peer's TLS cert
// matches a chain the destination committed to in some contract this session
// has seen. The trusted set is maintained by `AddTrustedPeerCertChain` (from
// `setContract`); every cert the peer has published is acceptable, so rotation
// is tolerated without breaking in-flight sessions. Skipped when:
//
//   - this is a companion-mode reply: the companion sender re-uses the session
//     cipher established by the original direction's handshake.
//   - the trusted set is empty (no contract seen, or all carried an empty
//     `ProvideTlsCertificate`): skip without latching, so a later contract with
//     a cert re-arms verification.
//
// Once matched, the result is cached and not re-run for this session.
func (self *SendSequence) verifyPeerCertAgainstContract() error {
	if self.session == nil {
		return nil
	}
	if self.companionContract {
		self.log.V(1).Infof(
			"[s]%s->%s s(%s) companion reply: reusing per-peer session cipher; skipping cert verification\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
		)
		return nil
	}
	verified, noCommitment := self.session.CertVerificationState()
	if verified || noCommitment {
		return nil
	}
	expected := self.session.trustedPeerCertSnapshot()
	// V(2) diagnostic: verify against the established epoch (whose cipher seals
	// this frame), not the in-flight currentEpoch() whose ConnectionState() blocks
	// on the running handshake. Logged so reaching this path (trusted set armed) is
	// observable.
	self.log.V(2).Infof(
		"[s][cert-verify]%s->%s s(%s) verifying established-epoch peer certs (non-blocking); trustedSet=%d companion=%t\n",
		self.client.ClientTag(),
		self.destination.DestinationId,
		self.destination.StreamId,
		len(expected),
		self.companionContract,
	)
	peerCerts := self.session.establishedPeerCertificates()
	ok, err := verifyPeerCertificateAgainstContract(peerCerts, expected)
	if err != nil {
		self.log.Errorf(
			"[s]%s->%s s(%s) sequence TLS cert verification failed: %s (peer presented %d cert(s); trusted set has %d)\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			err,
			len(peerCerts),
			len(expected),
		)
		return fmt.Errorf("sequence TLS cert verification failed: %w", err)
	}
	if !ok {
		self.log.Errorf(
			"[s]%s->%s s(%s) sequence TLS cert mismatch (peer presented %d cert(s); trusted set has %d)\n",
			self.client.ClientTag(),
			self.destination.DestinationId,
			self.destination.StreamId,
			len(peerCerts),
			len(expected),
		)
		return errors.New("sequence TLS cert verification failed")
	}
	self.session.MarkCertVerified()
	return nil
}

// openContractMultiRouteWriter returns the sequence's multi-route writer,
// opening it on first use and reopening it whenever the current head
// contract's destination mask changes (closing the previous writer). The
// writer routes to the head contract's destination, or to the sequence's own
// destination when there is no contract, and the destination is associated
// with the sequence so that acks for it route back. It is called on every
// write by writeMaybeWrappedBytes.
func (self *SendSequence) openContractMultiRouteWriter() MultiRouteWriter {
	var destination TransferPath
	if self.sendContract == nil {
		destination = self.destination
	} else {
		destination = self.sendContract.path.DestinationMask()
	}
	if self.contractMultiRouteWriter == nil || self.contractMultiRouteWriterDestination != destination {
		if self.contractMultiRouteWriter != nil {
			self.client.RouteManager().CloseMultiRouteWriter(self.contractMultiRouteWriter)
		}
		self.contractMultiRouteWriter = self.client.RouteManager().OpenMultiRouteWriter(destination)
		self.contractMultiRouteWriterDestination = destination

		// associate the destination with this sequence to receive acks
		self.sendBuffer.AssociateDestination(self, destination.LocalMask())
	}
	return self.contractMultiRouteWriter
}

// closeContractMultiRouteWriter closes the sequence's multi-route writer,
// if one is open, and clears the cached destination. It is called by the run
// loop's teardown when the sequence exits.
func (self *SendSequence) closeContractMultiRouteWriter() {
	if self.contractMultiRouteWriter != nil {
		self.client.RouteManager().CloseMultiRouteWriter(self.contractMultiRouteWriter)
		self.contractMultiRouteWriter = nil
		self.contractMultiRouteWriterDestination = TransferPath{}
	}
}

// Close shuts the sequence down: it cancels the sequence context, closes the
// pack and ack channels, and drains any packs still queued, invoking their
// ack callbacks with a "send sequence closed" error and returning their
// frame bytes to the message pool. It is called by the controller goroutine
// after the run loop has exited (see SendBuffer.Pack); Cancel alone skips
// the channel close and drain.
func (self *SendSequence) Close() {
	self.cancel()

	func() {
		self.packMutex.Lock()
		defer self.packMutex.Unlock()
		close(self.packs)
	}()

	func() {
		self.ackMutex.Lock()
		defer self.ackMutex.Unlock()
		close(self.acks)
	}()

	// drain the channel
	func() {
		for {
			select {
			case sendPack, ok := <-self.packs:
				if !ok {
					return
				}
				safeAck(sendPack.AckCallback, errors.New("Send sequence closed."))
				MessagePoolReturn(sendPack.Frame.MessageBytes)
			default:
				return
			}
		}
	}()
}

// Cancel cancels the sequence context, causing the run loop to exit and
// run its teardown: open contracts are closed, the resend queue is drained
// with error acks, and the multi-route writer is closed. Unlike Close it
// does not close the pack and ack channels or drain queued packs; the
// controller goroutine calls Close after the run loop exits to drain those.
func (self *SendSequence) Cancel() {
	self.cancel()
}

type sendItem struct {
	transferItem

	contractId         *Id
	head               bool
	hasContractFrame   bool
	sendTime           time.Time
	resendTime         time.Time
	sendCount          int
	transferFrameBytes []byte
	ackCallback        AckFunction
	// forceUnwrapped pins this item to plaintext on every (re)send, so the
	// outer wrap is skipped even if the per-peer cipher becomes available
	// between the initial send and a retransmit.
	forceUnwrapped bool

	// messageType protocol.MessageType
}

// messagePoolReturn returns the item's transfer frame bytes to the message
// pool, releasing the buffer the item held for the wire.
func (self *sendItem) messagePoolReturn() {
	MessagePoolReturn(self.transferFrameBytes)
}

// a send event queue which is the union of:
// - resend times
// - ack timeouts
type resendQueue = transferQueue[*sendItem]

func newResendQueue(budget *TransferMemoryBudget, minByteCount ByteCount) *resendQueue {
	q := newTransferQueue[*sendItem](func(a *sendItem, b *sendItem) int {
		if a.resendTime.Before(b.resendTime) {
			return -1
		} else if b.resendTime.Before(a.resendTime) {
			return 1
		} else {
			return 0
		}
	})
	q.setBudget(budget, minByteCount)
	return q
}

type ReceiveBufferSettings struct {
	GapTimeout  time.Duration
	IdleTimeout time.Duration

	SequenceBufferSize int
	// AckBufferSize int

	AckCompressTimeout time.Duration

	MinMessageByteCount ByteCount

	// min number of resends before checking abuse
	// ResendAbuseThreshold int
	// max legit fraction of sends that are resends
	// ResendAbuseMultiple float64

	MaxPeerAuditDuration time.Duration

	WriteTimeout time.Duration

	ReceiveQueueMaxByteCount ByteCount
	ReceiveQueueMinByteCount ByteCount
	ReceiveQueueBudget       *TransferMemoryBudget

	// whether to allow nacks without a contract_id
	AllowLegacyNack bool

	MaxOpenReceiveContract int

	ProtocolVersion int
}

type receiveSequenceId struct {
	Source     TransferPath
	SequenceId Id
	// EncryptionRole separates the inbound streams that map to our server
	// session (normal peer data — the default) from those that map to our
	// client session (the peer's EncryptedControl carrier + server replies).
	// SequenceId alone is already unique; the role makes the owning session
	// explicit and keys the per-role head tracking.
	EncryptionRole sequenceTlsRole
	// EncryptionCompanion separates the inbound streams owned by the companion
	// session from those owned by the regular session of the same role, so a
	// peer running both modes maps each stream to the right per-peer session.
	EncryptionCompanion bool
}

// receiveSequenceHeadKey identifies the head (newest) receive sequence for a
// given (source, companion, role). Supersession — drop-older / upgrade-newer
// by SequenceId — happens within a single (source, companion, role): the
// peer's client and server streams, and its companion and regular streams,
// reform independently, so they must not supersede each other.
type receiveSequenceHeadKey struct {
	Source              TransferPath
	EncryptionRole      sequenceTlsRole
	EncryptionCompanion bool
}

type ReceiveBuffer struct {
	ctx    context.Context
	client *Client
	log    Logger

	receiveBufferSettings *ReceiveBufferSettings

	mutex sync.Mutex
	// the head receive sequences
	// source id -> receive sequence
	receiveSequences       map[receiveSequenceId]*ReceiveSequence
	headReceiveSequenceIds map[receiveSequenceHeadKey]receiveSequenceId
}

func NewReceiveBuffer(ctx context.Context,
	client *Client,
	receiveBufferSettings *ReceiveBufferSettings) *ReceiveBuffer {
	return &ReceiveBuffer{
		ctx:                    ctx,
		client:                 client,
		log:                    client.log,
		receiveBufferSettings:  receiveBufferSettings,
		receiveSequences:       map[receiveSequenceId]*ReceiveSequence{},
		headReceiveSequenceIds: map[receiveSequenceHeadKey]receiveSequenceId{},
	}
}

// Pack routes an inbound pack to the receive sequence keyed by its source,
// sequence id, encryption role, and encryption companion, creating the
// sequence and starting its run-loop goroutine if none exists, then
// delegating to ReceiveSequence.Pack. Sequence versions are superseded per
// (source, companion, role): a pack from a sequence older than the current
// head is dropped (its frame bytes returned to the pool), and a pack from a
// newer sequence cancels the previous head sequence, waits for it to exit,
// and starts the replacement — so deliveries stay ordered across sequence
// versions. When the sequence shuts down between selection and enqueue, Pack
// retries once against a fresh sequence. On success the pack's frame bytes
// are owned by the sequence (or already returned to the pool for a
// superseded pack); on failure the caller retains ownership.
func (self *ReceiveBuffer) Pack(receivePack *ReceivePack, timeout time.Duration) (bool, error) {
	receiveSequenceId := receiveSequenceId{
		Source:              receivePack.Source,
		SequenceId:          receivePack.SequenceId,
		EncryptionRole:      receivePack.EncryptionRole,
		EncryptionCompanion: receivePack.EncryptionCompanion,
	}
	// Head/supersession is tracked per (source, companion, role): the peer's
	// client and server streams, and its companion and regular streams, reform
	// independently and must not supersede each other.
	headKey := receiveSequenceHeadKey{
		Source:              receiveSequenceId.Source,
		EncryptionRole:      receiveSequenceId.EncryptionRole,
		EncryptionCompanion: receiveSequenceId.EncryptionCompanion,
	}

	initReceiveSequence := func(skip *ReceiveSequence) *ReceiveSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		receiveSequence, ok := self.receiveSequences[receiveSequenceId]
		if ok {
			if skip == nil || skip != receiveSequence {
				return receiveSequence
			} else {
				receiveSequence.Cancel()
				// delete(self.receiveSequences, receiveSequenceId)
				// delete(self.headSequenceIds, headKey)
			}
			if headReceiveSequenceId := self.headReceiveSequenceIds[headKey]; headReceiveSequenceId != receiveSequenceId {
				panic(fmt.Errorf("[r]incorrect head sequence %s != %s\n", headReceiveSequenceId.SequenceId, receivePack.SequenceId))
			}
		} else if headReceiveSequenceId, ok := self.headReceiveSequenceIds[headKey]; ok {
			if receivePack.SequenceId.LessThan(headReceiveSequenceId.SequenceId) {
				// drop older sequences for source
				// this case happens when a client closes a sequence, then opens a new one,
				// before messages from the first are received
				if v := self.log.V(2); v.Enabled() {
					v.Infof("[r]drop older sequence %s < %s\n", receivePack.SequenceId, headReceiveSequenceId.SequenceId)
				}
				MessagePoolReturn(receivePack.TransferFrameBytes)
				return nil
			} else {
				// newer sequence for source
				if headReceiveSequenceId.SequenceId == receivePack.SequenceId {
					panic(fmt.Errorf("[r]upgrade older sequence %s = %s\n", headReceiveSequenceId.SequenceId, receivePack.SequenceId))
				}
				if v := self.log.V(2); v.Enabled() {
					v.Infof("[r]upgrade older sequence %s < %s\n", headReceiveSequenceId.SequenceId, receivePack.SequenceId)
				}
				headReceiveSequence := self.receiveSequences[headReceiveSequenceId]
				headReceiveSequence.Cancel()
				// wait for exit to ensure receives are correctly ordered across sequence versions
				headReceiveSequence.WaitForExit()
				delete(self.receiveSequences, headReceiveSequenceId)
			}
		}

		if v := self.log.V(2); v.Enabled() {
			v.Infof("[r]new sequence %s\n", receivePack.SequenceId)
		}

		receiveSequence = NewReceiveSequence(
			self.ctx,
			self.client,
			receivePack.Source,
			receivePack.SequenceId,
			receivePack.EncryptionRole,
			receivePack.EncryptionCompanion,
			self.receiveBufferSettings,
		)
		self.receiveSequences[receiveSequenceId] = receiveSequence
		self.headReceiveSequenceIds[headKey] = receiveSequenceId
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				receiveSequence.Close()
				// clean up
				if receiveSequence == self.receiveSequences[receiveSequenceId] {
					delete(self.receiveSequences, receiveSequenceId)
					// `headKey`/`receiveSequenceId` are values (no pointer to receivePack)
					delete(self.headReceiveSequenceIds, headKey)
				}
			}()
			receiveSequence.Run()
		})
		return receiveSequence
	}

	var receiveSequence *ReceiveSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		receiveSequence = initReceiveSequence(receiveSequence)
		if receiveSequence == nil {
			// drop
			return true, nil
		}
		if success, err = receiveSequence.Pack(receivePack, timeout); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

// ReceiveQueueSizeAndMessageTypes reports the number of items, their total
// byte count, and the protocol message types queued on the receive sequence
// addressed by `source` and `sequenceId`. The sequence id alone identifies
// the sequence, so every (role, companion) key is searched; if no sequence
// matches, it reports zeros and a nil message-type slice.
func (self *ReceiveBuffer) ReceiveQueueSizeAndMessageTypes(source TransferPath, sequenceId Id) (int, ByteCount, []protocol.MessageType) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// SequenceId already uniquely identifies the sequence; the caller does not
	// know the encryption role or companion, so check every per-(role,companion)
	// key.
	for _, role := range []sequenceTlsRole{sequenceTlsRoleClient, sequenceTlsRoleServer} {
		for _, companion := range []bool{false, true} {
			receiveSequenceId := receiveSequenceId{
				Source:              source,
				SequenceId:          sequenceId,
				EncryptionRole:      role,
				EncryptionCompanion: companion,
			}
			if receiveSequence, ok := self.receiveSequences[receiveSequenceId]; ok {
				return receiveSequence.ReceiveQueueSizeAndMessageTypes()
			}
		}
	}
	return 0, 0, nil
}

// Close cancels every open receive sequence. Each sequence's run loop
// performs its own teardown — closing or checkpointing contracts, draining
// the receive queue, completing the peer audit, releasing the peer session —
// and the controller goroutine removes the sequence from the buffer
// afterwards.
func (self *ReceiveBuffer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	// the control of the sequence will close it
	for _, receiveSequence := range self.receiveSequences {
		receiveSequence.Cancel()
	}
}

// Cancel is identical to Close: it cancels every open receive sequence. The
// sequences shut down through their run-loop teardown and remove themselves
// from the buffer afterwards.
func (self *ReceiveBuffer) Cancel() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, receiveSequence := range self.receiveSequences {
		receiveSequence.Cancel()
	}
}

// Flush cancels every open receive sequence, so queued and in-flight
// messages are dropped: each sequence's teardown drains the receive queue,
// returning the frame bytes to the message pool. It is called by
// Client.Flush to discard pending receives.
func (self *ReceiveBuffer) Flush() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, receiveSequence := range self.receiveSequences {
		// if !receiveSequenceId.Source.IsControlSource() {
		receiveSequence.Cancel()
		// }
	}
}

type ReceiveSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	client *Client
	log    Logger

	source     TransferPath
	sequenceId Id
	// encryptionRole is the local per-peer session role that owns this
	// inbound stream (complement of the sender's role): server for normal
	// peer data (the default), client for the peer's EncryptedControl
	// carrier + server replies.
	encryptionRole sequenceTlsRole
	// encryptionCompanion is the per-peer session identity companion that owns
	// this inbound stream (not complemented); with encryptionRole it selects
	// which session the sequence holds.
	encryptionCompanion bool

	receiveBufferSettings *ReceiveBufferSettings

	openReceiveContracts map[Id]*sequenceContract
	receiveContract      *sequenceContract

	packMutex sync.Mutex
	packs     chan *ReceivePack

	receiveQueue       *receiveQueue
	nextSequenceNumber uint64

	idleCondition *IdleCondition

	peerAudit *SequencePeerAudit

	ackWindow *sequenceAckWindow

	exit chan struct{}

	// session is the per-peer TLS session that decrypts this inbound stream,
	// of role `encryptionRole` (the complement of the sender's role).
	// Acquired from the `EncryptionSessionManager` at construction without
	// starting a handshake — a ReceiveSequence follows the peer's handshake,
	// it never initiates one. Holding it keeps the session (and its cipher)
	// alive for the stream's lifetime; released when the sequence terminates.
	// Nil when encryption is disabled or this is control-plane traffic.

	session *peerEncryptionSession
}

func NewReceiveSequence(
	ctx context.Context,
	client *Client,
	source TransferPath,
	sequenceId Id,
	encryptionRole sequenceTlsRole,
	encryptionCompanion bool,
	receiveBufferSettings *ReceiveBufferSettings) *ReceiveSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	seq := &ReceiveSequence{
		ctx:                   cancelCtx,
		cancel:                cancel,
		client:                client,
		log:                   client.log,
		source:                source,
		sequenceId:            sequenceId,
		encryptionRole:        encryptionRole,
		encryptionCompanion:   encryptionCompanion,
		receiveBufferSettings: receiveBufferSettings,
		openReceiveContracts:  map[Id]*sequenceContract{},
		receiveContract:       nil,
		packs:                 make(chan *ReceivePack, receiveBufferSettings.SequenceBufferSize),
		receiveQueue:          newReceiveQueue(receiveBufferSettings.ReceiveQueueBudget, receiveBufferSettings.ReceiveQueueMinByteCount),
		nextSequenceNumber:    0,
		idleCondition:         NewIdleCondition(),
		ackWindow:             newSequenceAckWindow(),
		exit:                  make(chan struct{}),
	}
	// Never encrypt control-plane traffic. A ReceiveSequence's data source is
	// the peer (source.SourceId) and its destination is always this client
	// (client.ClientId()); when `ReceiveNoSession` holds for either endpoint,
	// no session is acquired and inbound traffic is taken in plaintext.
	if client != nil && client.encryptionSessionManager != nil &&
		!client.encryptionSessionManager.ReceiveNoSession(source.SourceId) {
		seq.session = client.encryptionSessionManager.Acquire(source.SourceId, encryptionRole, encryptionCompanion)
	}
	return seq
}

// ReceiveQueueSizeAndMessageTypes reports the number of items, their total
// byte count, and the protocol message types queued on this sequence. The
// message types are recovered by unmarshalling each queued item's transfer
// frame; items that do not unmarshal contribute no message types.
func (self *ReceiveSequence) ReceiveQueueSizeAndMessageTypes() (int, ByteCount, []protocol.MessageType) {
	unpackMessageTypes := func(item *receiveItem) any {
		var messageTypes []protocol.MessageType
		var transferFrame protocol.TransferFrame
		err := proto.Unmarshal(item.transferFrameBytes, &transferFrame)
		if err == nil && transferFrame.Pack != nil {
			for _, frame := range transferFrame.Pack.Frames {
				messageTypes = append(messageTypes, frame.MessageType)
			}
		}
		return messageTypes
	}
	count, byteSize, summary := self.receiveQueue.QueueSizeAndSummary(unpackMessageTypes)
	var messageTypes []protocol.MessageType
	for _, summaryMessageTypes := range summary {
		messageTypes = append(messageTypes, summaryMessageTypes.([]protocol.MessageType)...)
	}
	return count, byteSize, messageTypes
}

// success, error
func (self *ReceiveSequence) Pack(receivePack *ReceivePack, timeout time.Duration) (bool, error) {
	self.packMutex.Lock()
	defer self.packMutex.Unlock()

	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	if timeout < 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- receivePack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- receivePack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- receivePack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

// Run is the sequence's main loop, started once per ReceiveSequence by
// ReceiveBuffer.Pack and blocking until the sequence terminates. It creates
// the peer audit, starts the ack-writer goroutine (which drains the ack
// window — see sendAck and sequenceAckWindow — compressing acks within
// AckCompressTimeout and writing them back to the source via a multi-route
// writer), and then services inbound packs until exit.
//
// Each iteration first delivers whatever the receive queue can make
// contiguous: queued items at or below nextSequenceNumber are dequeued, the
// item exactly at nextSequenceNumber is registered against its contract and
// delivered via receiveHead (advancing nextSequenceNumber), and items below
// it are resends of already-delivered messages, which are re-acked. If the
// queue holds a gap — an out-of-order item whose predecessor has not
// arrived — the wait is bounded by GapTimeout from the head item's receive
// time; when that deadline expires without the missing predecessor, the
// sequence exits. With an empty queue the wait is bounded by IdleTimeout.
//
// Inbound packs are dispatched to receiveNack (nacks) or receive (acks). A
// malformed or unverifiable pack exits the sequence with a bad-message
// audit; a pack the sequence chooses to drop is audited as a discard. The
// loop exits when the sequence context is canceled (Cancel or Close), the
// packs channel closes, a gap or idle timeout expires, or a bad message or
// contract is received.
//
// On exit the deferred teardown cancels the context, closes every open
// receive contract except the current one and checkpoints the current
// contract (both with their acked and unacked byte counts), drains the
// receive queue (auditing each discarded item and returning its frame bytes
// to the message pool), completes the peer audit out-of-band, releases the
// peer encryption session, and closes the exit channel that WaitForExit
// blocks on.
func (self *ReceiveSequence) Run() {
	defer func() {
		if r := recover(); r != nil {
			self.log.Errorf("[r]%s<-%s s(%s) abnormal exit =  %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, r)
			panic(r)
		}
	}()
	defer func() {
		self.cancel()

		// close previous contracts and checkpoint the current contract
		for _, receiveContract := range self.openReceiveContracts {
			if self.receiveContract != receiveContract {
				if receiveContract.unackedByteCount != 0 {
					self.log.Infof("[r]%s<-%s s(%s) close contract with unacked =  %d\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, receiveContract.unackedByteCount)
				}
				self.client.ContractManager().CloseContract(
					receiveContract.contractId,
					receiveContract.ackedByteCount,
					receiveContract.unackedByteCount,
				)
			}
		}
		if self.receiveContract != nil {
			// the sender may send again with this contract (set as head)
			// checkpoint the contract but do not close it
			if self.receiveContract.unackedByteCount != 0 {
				self.log.Infof("[r]%s<-%s s(%s) checkpoint contract with unacked =  %d\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, self.receiveContract.unackedByteCount)
			}
			self.client.ContractManager().CheckpointContract(
				self.receiveContract.contractId,
				self.receiveContract.ackedByteCount,
				self.receiveContract.unackedByteCount,
			)
		}

		// drain the buffer
		for _, item := range self.receiveQueue.Clear() {
			self.peerAudit.Update(func(a *PeerAudit) {
				a.discard(item.messageByteCount)
			})
			// MessagePoolReturn(item.transferFrameBytes)
			item.messagePoolReturn()
		}

		self.peerAudit.Complete()

		if self.session != nil {
			self.session.Release()
		}

		close(self.exit)
	}()

	self.peerAudit = NewSequencePeerAudit(
		self.client,
		self.source,
		self.receiveBufferSettings.MaxPeerAuditDuration,
	)

	// compress and send acks
	go HandleError(func() {
		defer self.cancel()

		multiRouteWriter := self.client.RouteManager().OpenMultiRouteWriter(self.source.Reverse())
		defer self.client.RouteManager().CloseMultiRouteWriter(multiRouteWriter)

		writeAck := func(sendAck *sequenceAck) {
			path := self.source.Reverse().AddSource(self.client.ClientId())

			var transferFrameBytes []byte
			if 2 <= self.receiveBufferSettings.ProtocolVersion {
				// hand-rolled marshal of the hot Ack TransferFrame; wire-identical
				// to the proto structs in the legacy branch (see frame_protobuf_test.go).
				saf := sendAckFrame{
					path:       path,
					messageId:  sendAck.messageId,
					sequenceId: self.sequenceId,
					selective:  sendAck.selective,
					tag:        sendAck.tag,
				}
				transferFrameBytes = marshalSendAckTransferFrame(&saf)
			} else {
				ack := &protocol.Ack{
					MessageId:  sendAck.messageId.Bytes(),
					SequenceId: self.sequenceId.Bytes(),
					Selective:  sendAck.selective,
					Tag:        sendAck.tag,
				}
				ackBytes, _ := ProtoMarshal(ack)
				defer MessagePoolReturn(ackBytes)
				transferFrame := &protocol.TransferFrame{
					TransferPath: path.ToProtobuf(),
					Frame: &protocol.Frame{
						MessageType:  protocol.MessageType_TransferAck,
						MessageBytes: ackBytes,
					},
				}
				transferFrameBytes, _ = ProtoMarshal(transferFrame)
			}
			defer MessagePoolReturn(transferFrameBytes)
			c := func() error {
				// outer-wrap the ack TransferFrame with the per-peer
				// session cipher when available. Mirror the wrap state
				// of the acked pack: if any pack covered by this ack
				// arrived plaintext, send the ack plaintext too — the
				// sender's cipher may not yet be established (it sent
				// plaintext because it had no cipher at send time), so
				// a wrapped ack would be unreadable on arrival.
				var cipher *sequenceCipher
				if self.session != nil && !sendAck.unwrapped {
					cipher = self.session.Cipher()
				}
				if cipher == nil {
					return multiRouteWriter.Write(
						self.ctx,
						MessagePoolShareReadOnly(transferFrameBytes),
						self.receiveBufferSettings.WriteTimeout,
					)
				}
				ciphertext, sealErr := cipher.Seal(transferFrameBytes)
				if sealErr != nil {
					return fmt.Errorf("ack outer wrap seal: %w", sealErr)
				}
				if cipher.ShouldRekey() {
					// bound the number of messages sealed under one AEAD key
					// (see sequenceCipherMaxSeals); non-disruptive
					self.session.restartHandshake()
				}
				// Carry our receive session's role + companion as the peer's
				// decrypt hint (it routes to the complement of our role / the
				// matching companion on its side).
				wrapped, marshalErr := buildEncryptedOuterFrameBytes(path, ciphertext, self.session.role.toProtobuf(), self.session.companion)
				if marshalErr != nil {
					return fmt.Errorf("ack outer wrap marshal: %w", marshalErr)
				}
				defer MessagePoolReturn(wrapped)
				return multiRouteWriter.Write(
					self.ctx,
					MessagePoolShareReadOnly(wrapped),
					self.receiveBufferSettings.WriteTimeout,
				)
			}
			if self.log.V(2).Enabled() {
				TraceWithReturn(
					fmt.Sprintf(
						"[r]multi route write (ack %d) %s->%s s(%s)",
						sendAck.sequenceNumber,
						self.client.ClientTag(),
						self.source.SourceId,
						self.source.StreamId,
					),
					c,
				)
			} else {
				err := c()
				if err != nil {
					if ok, suppressed := dropErrLogThrottle.Allow(time.Now()); ok {
						if suppressed > 0 {
							self.log.Infof("[r]drop = %s (%d suppressed)", err, suppressed)
						} else {
							self.log.Infof("[r]drop = %s", err)
						}
					}
				}
			}
		}

		// reusable ack-compress timer (avoids a per-iteration time.After alloc on
		// the ack hot path). created already-fired; Reset before the blocking
		// select arms it (go1.23+ delivers no stale fire after Reset).
		ackCompressTimer := time.NewTimer(0)
		defer ackCompressTimer.Stop()

		for {
			select {
			case <-self.ctx.Done():
				return
			default:
			}

			ackSnapshot := self.ackWindow.Snapshot(false)
			if ackSnapshot.ackUpdateCount == 0 && len(ackSnapshot.selectiveAcks) == 0 {
				// wait for one ack
				select {
				case <-self.ctx.Done():
					return
				case <-ackSnapshot.ackNotify:
				}
			}

			if 0 < self.receiveBufferSettings.AckCompressTimeout {
				ackCompressTimer.Reset(self.receiveBufferSettings.AckCompressTimeout)
				select {
				case <-self.ctx.Done():
					return
				case <-ackCompressTimer.C:
				}
			}

			ackSnapshot = self.ackWindow.Snapshot(true)
			if 0 < ackSnapshot.ackUpdateCount {
				writeAck(ackSnapshot.headAck)
			}
			for messageId, ack := range ackSnapshot.selectiveAcks {
				writeAck(&sequenceAck{
					messageId:      messageId,
					sequenceNumber: ack.sequenceNumber,
					selective:      true,
					tag:            ack.tag,
					unwrapped:      ack.unwrapped,
				})
			}
		}
	}, self.cancel)

	// reusable idle/gap timer (avoids a per-iteration time.After alloc on the
	// receive hot path). created already-fired; Reset before the blocking select
	// arms it (go1.23+ delivers no stale fire after Reset).
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		receiveTime := time.Now()
		var timeout time.Duration

		if queueSize, _ := self.receiveQueue.QueueSize(); 0 == queueSize {
			timeout = self.receiveBufferSettings.IdleTimeout
		} else {
			timeout = self.receiveBufferSettings.GapTimeout
			for {
				item := self.receiveQueue.PeekFirst()
				if item == nil {
					break
				}

				itemGapTimeout := item.receiveTime.Add(self.receiveBufferSettings.GapTimeout).Sub(receiveTime)
				if itemGapTimeout < 0 {
					self.log.Errorf("[r]%s<-%s s(%s) exit gap timeout\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					// did not receive a preceding message in time
					return
				}

				if self.nextSequenceNumber < item.sequenceNumber {
					if itemGapTimeout < timeout {
						timeout = itemGapTimeout
					}
					break
				}
				// item.sequenceNumber <= self.nextSequenceNumber

				self.receiveQueue.RemoveByMessageId(item.messageId)

				if self.nextSequenceNumber == item.sequenceNumber {
					// this item is the head of sequence
					if err := self.registerContracts(item); err != nil {
						self.log.Errorf("[r]%s<-%s s(%s) exit could not register contracts = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
						return
					}
					if self.updateContract(item) {
						self.log.V(1).Infof("[r]seq+ %d->%d (queue) %s<-%s s(%s)\n", self.nextSequenceNumber, self.nextSequenceNumber+1, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
						self.nextSequenceNumber = self.nextSequenceNumber + 1
						self.receiveHead(item)
					} else {
						// no valid contract. it should have been attached to the head
						self.log.Errorf("[r]drop head no contract %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
						return
					}
				} else {
					// this item is a resend of a previous item
					if item.ack {
						self.sendAck(item.sequenceNumber, item.messageId, false, nil, item.unwrapped)
					}
				}
			}
		}

		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(timeout)
		select {
		case <-self.ctx.Done():
			return
		case receivePack, ok := <-self.packs:
			if !ok {
				return
			}

			if receivePack.Pack.Nack {
				received, err := self.receiveNack(receivePack)
				if err != nil {
					// bad message
					// close the sequence
					self.log.Infof("[r]%s<-%s s(%s) exit could not receive nack = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
					self.peerAudit.Update(func(a *PeerAudit) {
						a.badMessage(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
					return
				} else if !received {
					self.log.V(1).Infof("[r]drop nack %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					// drop the message
					self.peerAudit.Update(func(a *PeerAudit) {
						a.discard(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
				}

				// note messages of `size < MinMessageByteCount` get counted as `MinMessageByteCount` against the contract
			} else {
				received, err := self.receive(receivePack)
				if err != nil {
					// bad message
					// close the sequence
					self.log.Errorf("[r]%s<-%s s(%s) exit could not receive ack = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
					self.peerAudit.Update(func(a *PeerAudit) {
						a.badMessage(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
					return
				} else if !received {
					self.log.V(1).Infof("[r]drop ack %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					// drop the message
					self.peerAudit.Update(func(a *PeerAudit) {
						a.discard(receivePack.MessageByteCount)
					})
					MessagePoolReturn(receivePack.TransferFrameBytes)
				}
			}
		case <-idleTimer.C:
			if 0 == self.receiveQueue.Len() {
				done := false
				func() {
					self.packMutex.Lock()
					defer self.packMutex.Unlock()
					// idle timeout
					if self.idleCondition.Close(checkpointId) {
						done = true
					}
					// else there are pending updates
				}()
				if done {
					// close the sequence
					self.log.V(2).Infof("[r]%s<-%s s(%s) exit idle timeout\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
					return
				}
			}
		}
	}
}

// sendAck records an ack in the sequence's ack window for the ack-writer
// goroutine to send; it never writes to the wire itself. A non-selective ack
// is a cumulative head ack advancing the sequence position; a selective ack
// acknowledges a specific out-of-order message by id so the sender stops
// resending it.
func (self *ReceiveSequence) sendAck(sequenceNumber uint64, messageId Id, selective bool, tag *protocol.Tag, unwrapped bool) {
	ack := &sequenceAck{
		sequenceNumber: sequenceNumber,
		messageId:      messageId,
		selective:      selective,
		tag:            tag,
		unwrapped:      unwrapped,
	}
	self.ackWindow.Update(ack)
}

// receive handles an inbound ack (non-nack) pack. It parses the message id,
// replaces any queued item with the same sequence number or message id
// (auditing the replaced item as a resend and returning its frame bytes to
// the pool), and then dispatches on the pack's position:
//
//   - At the head (sequence number == nextSequenceNumber): the sequence
//     number advances, the item's contracts are registered, the message is
//     debited against a contract, and the item is delivered via receiveHead;
//     returns true. Failure to register or debit the contract returns an
//     error, and the sequence exits.
//   - Past the head: a resend of an already-delivered message — re-acked and
//     dropped, returning false with no error.
//   - Future: the item is queued up to ReceiveQueueMaxByteCount (evicting
//     later queued items to make room) and acknowledged with a selective
//     ack; returns true. When the queue cannot fit the message it is
//     dropped, returning false with no error.
//
// A pack marked head with a sequence number beyond nextSequenceNumber
// advances the sequence to it before dispatch — the case where the receiver
// reformed and lost state. On a true return the frame bytes have been
// consumed: returned to the pool by receiveHead on the delivered path, or
// owned by the receive queue until the item is dequeued. On false or error
// the caller retains ownership.
func (self *ReceiveSequence) receive(receivePack *ReceivePack) (bool, error) {
	receiveTime := time.Now()

	sequenceNumber := receivePack.Pack.SequenceNumber
	// var contractId *Id
	// if self.receiveContract != nil {
	// 	contractId = &self.receiveContract.contractId
	// }
	messageId, err := IdFromBytes(receivePack.Pack.MessageId)
	if err != nil {
		return false, errors.New("Bad message_id")
	}

	// note the receive contract is the contract active when this is at the head of the queue
	item := &receiveItem{
		transferItem: transferItem{
			messageId:        messageId,
			sequenceNumber:   sequenceNumber,
			messageByteCount: receivePack.MessageByteCount,
		},

		// contractId:      contractId,
		receiveTime:        receiveTime,
		frames:             receivePack.Pack.Frames,
		contractFrame:      receivePack.Pack.ContractFrame,
		receiveCallback:    receivePack.ReceiveCallback,
		head:               receivePack.Pack.Head,
		ack:                !receivePack.Pack.Nack,
		tag:                receivePack.Pack.Tag,
		transferFrameBytes: receivePack.TransferFrameBytes,
		unwrapped:          receivePack.Unwrapped,
	}

	// this case happens when the receiver is reformed or loses state.
	// the sequence id guarantees the sender is the same for the sequence
	// past head items are retransmits. Future head items depend on previous ack,
	// which represent some state the sender has that the receiver is missing
	// advance the receiver state to the latest from the sender
	if item.head && self.nextSequenceNumber < item.sequenceNumber {
		self.log.V(2).Infof("[r]seq= %d->%d %s<-%s s(%s)\n", self.nextSequenceNumber, item.sequenceNumber, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		self.nextSequenceNumber = item.sequenceNumber
		// the head must have a contract frame to reset the contract
	}

	if removedItem := self.receiveQueue.RemoveBySequenceNumber(sequenceNumber); removedItem != nil {
		self.peerAudit.Update(func(a *PeerAudit) {
			a.resend(removedItem.messageByteCount)
		})
		removedItem.messagePoolReturn()
	}

	// replace with the latest value (check both messageId and sequenceNumber)
	if removedItem := self.receiveQueue.RemoveByMessageId(messageId); removedItem != nil {
		self.peerAudit.Update(func(a *PeerAudit) {
			a.resend(removedItem.messageByteCount)
		})
		removedItem.messagePoolReturn()
	}

	if sequenceNumber <= self.nextSequenceNumber {
		if self.nextSequenceNumber == sequenceNumber {
			// this item is the head of sequence
			self.log.V(2).Infof("[r]seq+ %d->%d %s<-%s s(%s)\n", self.nextSequenceNumber, self.nextSequenceNumber+1, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			self.nextSequenceNumber = self.nextSequenceNumber + 1

			if err := self.registerContracts(item); err != nil {
				self.log.Errorf("[r]%s<-%s s(%s) ack could not register contracts = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
				return false, err
			}
			if self.updateContract(item) {
				self.receiveHead(item)
				return true, nil
			} else {
				// no valid contract. it should have been attached to the head
				self.log.Errorf("[r]drop queue head no contract %s<-%s s(%s): head=%t, contract=%t, rcontract=%t\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, item.head, item.contractFrame != nil, self.receiveContract != nil)
				return false, errors.New("No contract")
			}
		} else {
			self.log.V(1).Infof("[r]drop past sequence number %d <> %d ack=%t %s<-%s s(%s)\n", sequenceNumber, self.nextSequenceNumber, item.ack, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			// this item is a resend of a previous item
			if item.ack {
				self.sendAck(sequenceNumber, messageId, false, nil, item.unwrapped)
			}
			return false, nil
		}
	} else {
		// store only up to a max size in the receive queue
		canQueue := func(byteCount ByteCount) bool {
			return self.receiveQueue.CanAdd(byteCount, self.receiveBufferSettings.ReceiveQueueMaxByteCount)
		}

		// remove later items to fit
		for !canQueue(receivePack.MessageByteCount) {
			lastItem := self.receiveQueue.PeekLast()
			if receivePack.Pack.SequenceNumber < lastItem.sequenceNumber {
				self.receiveQueue.RemoveByMessageId(lastItem.messageId)
				lastItem.messagePoolReturn()
			} else {
				break
			}
		}

		if canQueue(receivePack.MessageByteCount) {
			self.receiveQueue.Add(item)
			self.sendAck(sequenceNumber, messageId, true, item.tag, item.unwrapped)
			return true, nil
		} else {
			self.log.V(1).Infof("[r]drop ack cannot queue %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			return false, nil
		}
	}
}

// receiveNack handles an inbound nack pack — a message sent with the NoAck
// transfer option, which the sender will not retransmit and expects no ack
// for. It is applied immediately rather than queued for in-order delivery:
// the item's contracts are registered, the message is debited against its
// contract, and it is delivered via receiveHead without an ack. A contract
// id is required unless AllowLegacyNack is set, and it must name an open
// contract; when the pack carries no usable contract the message is dropped
// (false, nil). Malformed messages return an error, and the sequence exits.
func (self *ReceiveSequence) receiveNack(receivePack *ReceivePack) (bool, error) {

	receiveTime := time.Now()

	sequenceNumber := receivePack.Pack.SequenceNumber
	// var contractId *Id
	// if self.receiveContract != nil {
	// 	contractId = &self.receiveContract.contractId
	// }
	messageId, err := IdFromBytes(receivePack.Pack.MessageId)
	if err != nil {
		return false, errors.New("Bad message_id")
	}

	var contractId *Id
	if receivePack.Pack.ContractId != nil {
		contractId_, err := IdFromBytes(receivePack.Pack.ContractId)
		if err != nil {
			return false, errors.New("Bad contract_id")
		}
		contractId = &contractId_
	}

	if contractId == nil && !self.receiveBufferSettings.AllowLegacyNack {
		self.log.Infof("[r]drop nack required contract id %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		return false, nil
	}

	item := &receiveItem{
		transferItem: transferItem{
			messageId:        messageId,
			sequenceNumber:   sequenceNumber,
			messageByteCount: receivePack.MessageByteCount,
		},
		contractId:         contractId,
		receiveTime:        receiveTime,
		frames:             receivePack.Pack.Frames,
		contractFrame:      receivePack.Pack.ContractFrame,
		receiveCallback:    receivePack.ReceiveCallback,
		head:               receivePack.Pack.Head,
		ack:                !receivePack.Pack.Nack,
		tag:                receivePack.Pack.Tag,
		transferFrameBytes: receivePack.TransferFrameBytes,
	}

	if err := self.registerContracts(item); err != nil {
		self.log.Errorf("[r]%s<-%s s(%s) nack could not register contracts = %s\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, err)
		return false, err
	}

	if contractId != nil {
		if _, ok := self.openReceiveContracts[*contractId]; !ok {
			self.log.Infof("[r]drop nack contract mismatch %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
			return false, nil
		}
	}

	if self.updateContract(item) {
		self.receiveHead(item)
		return true, nil
	} else {
		// no valid contract
		// drop the message. since this is a nack it will not block the sequence
		self.log.Infof("[r]drop nack no contract %s<-%s s(%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
		return false, nil
	}
}

// receiveHead delivers the head item: it records the message in the peer
// audit, debits the message byte count against the item's open contract and
// adopts the contract's provide mode (an item without a usable contract is
// treated as ProvideMode_Network), routes any EncryptedControl frames into
// the per-peer session, invokes the receive callback with the remaining
// application frames, records a cumulative ack for acked items, and returns
// the item's frame bytes to the message pool.
func (self *ReceiveSequence) receiveHead(item *receiveItem) {
	frameMessageTypes := []string{}
	for _, frame := range item.frames {
		frameMessageTypes = append(frameMessageTypes, fmt.Sprintf("%v", frame.MessageType))
	}
	frameMessageTypesStr := strings.Join(frameMessageTypes, ", ")
	if item.ack {
		self.log.V(1).Infof("[r]head %d (%s) %s<-%s s(%s)\n", item.sequenceNumber, frameMessageTypesStr, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
	} else {
		self.log.V(1).Infof("[r]head nack (%s) %s<-%s s(%s)\n", frameMessageTypesStr, self.client.ClientTag(), self.source.SourceId, self.source.StreamId)
	}
	self.peerAudit.Update(func(a *PeerAudit) {
		a.received(item.messageByteCount)
	})
	var provideMode protocol.ProvideMode

	if item.contractId != nil {
		if receiveContract, ok := self.openReceiveContracts[*item.contractId]; ok {
			receiveContract.ack(item.messageByteCount)
			self.updateContractStats(receiveContract)
			provideMode = receiveContract.provideMode
		} else {
			provideMode = protocol.ProvideMode_Network
		}
	} else {
		// no contract peers are considered in network
		provideMode = protocol.ProvideMode_Network
	}
	// EncryptedControl frames are routed into the per-peer session instead
	// of bubbling up to the receive callback. They carry the TLS handshake
	// bytes that bootstrap the per-peer cipher; the application shouldn't
	// see them.
	appFrames := item.frames
	if self.session != nil {
		appFrames = self.deliverEncryptedControlFrames(item.frames)
	}
	if 0 < len(appFrames) {
		item.receiveCallback(
			self.source,
			appFrames,
			Peer{ProvideMode: provideMode},
		)
	}
	if item.ack {
		self.sendAck(item.sequenceNumber, item.messageId, false, item.tag, item.unwrapped)
	}
	item.messagePoolReturn()
}

// deliverEncryptedControlFrames splits an incoming Pack's frames: any
// `TransferEncryptedControl` frames are decoded and routed into the per-peer
// session of the complement of the sender's role (a client-role control —
// the peer's ClientHello — drives our server session, and vice versa),
// creating that session if needed. The remaining application frames are
// returned for delivery to the receive callback.
func (self *ReceiveSequence) deliverEncryptedControlFrames(frames []*protocol.Frame) []*protocol.Frame {
	var passthrough []*protocol.Frame
	for _, frame := range frames {
		if frame == nil {
			continue
		}
		if frame.MessageType != protocol.MessageType_TransferEncryptedControl {
			passthrough = append(passthrough, frame)
			continue
		}
		if self.client == nil || self.client.encryptionSessionManager == nil {
			continue
		}
		ec := &protocol.EncryptedControl{}
		if err := ProtoUnmarshal(frame.MessageBytes, ec); err != nil {
			self.log.V(1).Infof("[r]%s<-%s bad encrypted control = %s\n", self.client.ClientTag(), self.source.SourceId, err)
			continue
		}
		senderRole, ok := sequenceTlsRoleFromProtobuf(ec.SessionRole)
		if !ok {
			self.log.V(1).Infof("[r]%s<-%s encrypted control with no session role — dropped\n", self.client.ClientTag(), self.source.SourceId)
			continue
		}
		self.client.encryptionSessionManager.DeliverEncryptedControl(self.source.SourceId, senderRole.complement(), ec)
	}
	return passthrough
}

// registerContracts parses, verifies, and activates the contract carried by
// the item's contract frame, if any. The Contract is unmarshalled, its
// stored-contract HMAC is verified against the local provider secret via
// ContractManager.Verify, and the contract is built and activated by
// setContract. On failure the failure is audited — a badMessage for an
// unparsable contract, badContract for a verification failure — and the
// error is returned so the sequence exits. An item with no contract frame is
// a no-op.
func (self *ReceiveSequence) registerContracts(item *receiveItem) error {
	if item.contractFrame == nil {
		return nil
	}

	var contract protocol.Contract
	err := ProtoUnmarshal(item.contractFrame.MessageBytes, &contract)
	if err != nil {
		// bad message
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badMessage(item.messageByteCount)
		})
		return err
	}

	// check the hmac with the local provider secret key
	if !self.client.ContractManager().Verify(
		contract.StoredContractHmac,
		contract.StoredContractBytes,
		contract.ProvideMode) {
		self.log.Errorf("[r]%s<-%s s(%s) exit contract verification failed (%s)\n", self.client.ClientTag(), self.source.SourceId, self.source.StreamId, contract.ProvideMode)
		// bad contract
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badContract()
		})
		return errors.New("Contract verification failed.")
	}

	nextReceiveContract, err := newSequenceContract(
		self.log,
		"r",
		&contract,
		self.receiveBufferSettings.MinMessageByteCount,
		1.0,
	)
	if err != nil {
		// bad contract
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badContract()
		})
		return err
	}

	if err := self.setContract(nextReceiveContract); err != nil {
		// the next contract has already been used
		// bad contract
		// close sequence
		self.peerAudit.Update(func(a *PeerAudit) {
			a.badContract()
		})
		return err
	}

	return nil
}

// setContract activates `nextReceiveContract` as the sequence's current
// receive contract. A contract already open by id is adopted by pointer
// switch; a new contract is added to the open set and registered with the
// contract manager's per-contract stats. When the open set exceeds
// MaxOpenReceiveContract, the least-recently-added contracts are closed and
// evicted, keeping the current contract. It always returns nil — the error
// result is currently unused.
func (self *ReceiveSequence) setContract(nextReceiveContract *sequenceContract) error {
	// contract already set
	if self.receiveContract != nil && self.receiveContract.contractId == nextReceiveContract.contractId {
		return nil
	}

	if receiveContract, ok := self.openReceiveContracts[nextReceiveContract.contractId]; ok {
		// switch to the current contract
		self.receiveContract = receiveContract
		return nil
	}

	self.openReceiveContracts[nextReceiveContract.contractId] = nextReceiveContract
	self.receiveContract = nextReceiveContract
	nextReceiveContract.contractStatsEntry = self.client.ContractManager().registerContractStats(
		nextReceiveContract.contractId,
		true,
		false,
		nextReceiveContract.path,
		nextReceiveContract.transferByteCount,
	)

	if d := len(self.openReceiveContracts) - self.receiveBufferSettings.MaxOpenReceiveContract; 0 < d {
		// remove the least recently added
		orderedReceiveContracts := maps.Values(self.openReceiveContracts)
		// ascending where earliest created are first
		slices.SortFunc(orderedReceiveContracts, func(a *sequenceContract, b *sequenceContract) int {
			return a.localId.Cmp(b.localId)
		})
		for _, receiveContract := range orderedReceiveContracts[:d] {
			if receiveContract != self.receiveContract {
				self.client.ContractManager().CloseContract(
					receiveContract.contractId,
					receiveContract.ackedByteCount,
					receiveContract.unackedByteCount,
				)
				delete(self.openReceiveContracts, receiveContract.contractId)
			}
		}
	}

	return nil
}

// updateContract debits the item's message byte count against a valid
// contract, in order of preference: the item's own contract id when it names
// an open contract, otherwise the sequence's current receive contract (the
// item's contract id is updated to it), otherwise the mutual no-contract
// configuration when ContractManager.ReceiveNoContract holds for the source.
// It returns true when the message is covered by a contract and false when
// it is not — the message should then be dropped.
func (self *ReceiveSequence) updateContract(item *receiveItem) bool {
	// always use a contract if present
	// the sender may send contracts even if `receiveNoContract` is set locally
	if item.contractId != nil {
		if receiveContract, ok := self.openReceiveContracts[*item.contractId]; ok && receiveContract.update(item.messageByteCount) {
			return true
		}
	} else if self.receiveContract != nil && self.receiveContract.update(item.messageByteCount) {
		item.contractId = &self.receiveContract.contractId
		return true
	}
	// `receiveNoContract` is a mutual configuration
	// both sides must configure themselves to require no contract from each other
	if self.client.ContractManager().ReceiveNoContract(self.source.SourceId) {
		return true
	}
	return false
}

// updateContractStats publishes the contract's acked byte count to its
// registered contract-manager stats entry, which the contract stats epoch
// worker consumes. No-op when the contract has no stats entry.
func (self *ReceiveSequence) updateContractStats(receiveContract *sequenceContract) {
	if receiveContract.contractStatsEntry != nil {
		receiveContract.contractStatsEntry.updateUsedByteCount(receiveContract.ackedByteCount)
	}
}

// Close shuts the sequence down: it cancels the sequence context, closes the
// pack channel, and drains any packs still queued, returning their frame
// bytes to the message pool. It is called by the controller goroutine after
// the run loop has exited (see ReceiveBuffer.Pack); Cancel alone skips the
// channel close and drain.
func (self *ReceiveSequence) Close() {
	self.cancel()

	func() {
		self.packMutex.Lock()
		defer self.packMutex.Unlock()
		close(self.packs)
	}()

	// drain the channel
	func() {
		for {
			select {
			case receivePack, ok := <-self.packs:
				if !ok {
					return
				}
				MessagePoolReturn(receivePack.TransferFrameBytes)
			default:
				return
			}
		}
	}()
}

// Cancel cancels the sequence context, causing the run loop to exit and run
// its teardown: contracts are closed or checkpointed, the receive queue is
// drained, the peer audit is completed, and the peer session is released.
// Unlike Close it does not close the pack channel or drain queued packs; the
// controller goroutine calls Close after the run loop exits to drain those.
func (self *ReceiveSequence) Cancel() {
	self.cancel()
}

// WaitForExit blocks until the run loop has fully exited, that is, until
// its deferred teardown has closed the exit channel. It is safe to call
// after Cancel (the teardown always runs) and is used by ReceiveBuffer.Pack
// to order deliveries across a superseding sequence version.
func (self *ReceiveSequence) WaitForExit() {
	select {
	case <-self.exit:
	}
}

type receiveItem struct {
	transferItem

	contractId         *Id
	head               bool
	receiveTime        time.Time
	frames             []*protocol.Frame
	contractFrame      *protocol.Frame
	receiveCallback    ReceiveFunction
	ack                bool
	tag                *protocol.Tag
	transferFrameBytes []byte
	// unwrapped is true when the originating TransferFrame arrived on
	// the wire as plaintext (no outer encrypted wrap). Propagated into
	// the sequenceAck so the ack format mirrors the incoming pack.
	unwrapped bool
}

// messagePoolReturn returns the item's transfer frame bytes to the message
// pool. The item's frames and contract frame are slices into those same
// bytes, so only the top-level buffer is returned.
func (self *receiveItem) messagePoolReturn() {
	MessagePoolReturn(self.transferFrameBytes)
	// note frames and contractFrame are slices/shared bytes of the transfer frame bytes
	// we expect these both to be false
	// for _, frame := range self.frames {
	// 	r := MessagePoolReturn(frame.MessageBytes)
	// 	if r {
	// 		glog.Warningf("[ri]frame was not shared]\n")
	// 	}
	// }
	// if self.contractFrame != nil {
	// 	r := MessagePoolReturn(self.contractFrame.MessageBytes)
	// 	if r {
	// 		glog.Warningf("[ri]contract frame was not shared]\n")
	// 	}
	// }
}

// ordered by sequenceNumber
type receiveQueue = transferQueue[*receiveItem]

func newReceiveQueue(budget *TransferMemoryBudget, minByteCount ByteCount) *receiveQueue {
	q := newTransferQueue[*receiveItem](func(a *receiveItem, b *receiveItem) int {
		if a.sequenceNumber < b.sequenceNumber {
			return -1
		} else if b.sequenceNumber < a.sequenceNumber {
			return 1
		} else {
			return 0
		}
	})
	q.setBudget(budget, minByteCount)
	return q
}

type sequenceAck struct {
	sequenceNumber uint64
	messageId      Id
	selective      bool
	tag            *protocol.Tag
	// unwrapped is true when any pack covered by this ack arrived on
	// the wire as plaintext. The ack writer mirrors that state — a
	// plaintext-acked window emits a plaintext ack — so peers whose
	// ciphers haven't been established yet can read the ack. Cumulative
	// head acks or-in the bit across every absorbed lower ack.
	unwrapped bool
}

type sequenceAckWindowSnapshot struct {
	ackNotify      <-chan struct{}
	headAck        *sequenceAck
	ackUpdateCount int
	selectiveAcks  map[Id]*sequenceAck
}

type sequenceAckWindow struct {
	ackMonitor     *Monitor
	ackLock        sync.Mutex
	headAck        *sequenceAck
	ackUpdateCount int
	selectiveAcks  map[Id]*sequenceAck
}

func newSequenceAckWindow() *sequenceAckWindow {
	return &sequenceAckWindow{
		ackMonitor:     NewMonitor(),
		headAck:        nil,
		ackUpdateCount: 0,
		selectiveAcks:  map[Id]*sequenceAck{},
	}
}

// Update records an ack in the window: an ack newer than the head advances
// the head (non-selective) or is stored by message id (selective), and an
// ack at or below the head — a late duplicate of an already-acked message —
// is folded into the head copy-on-write, since a concurrent Snapshot may
// have published the head pointer. The update count is bumped and the
// monitor channel notified so the ack-writer goroutine wakes.
func (self *sequenceAckWindow) Update(ack *sequenceAck) {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()

	if self.headAck == nil || self.headAck.sequenceNumber < ack.sequenceNumber {
		if ack.selective {
			if prior, ok := self.selectiveAcks[ack.messageId]; ok && prior.unwrapped {
				// coalesced selective ack for the same message: preserve
				// any prior plaintext bit so a single late wrapped resend
				// doesn't upgrade the ack format past the sender's reach.
				ack.unwrapped = true
			}
			self.selectiveAcks[ack.messageId] = ack
		} else {
			// cumulative head ack: or-in the prior head's plaintext bit
			// (and any absorbed selective acks below the new head) so a
			// single plaintext pack anywhere under the head keeps the
			// ack plaintext. Selective acks at or below the new head are
			// already dropped by the Snapshot pass.
			if self.headAck != nil && self.headAck.unwrapped {
				ack.unwrapped = true
			}
			if !ack.unwrapped {
				for _, sel := range self.selectiveAcks {
					if sel.unwrapped && sel.sequenceNumber <= ack.sequenceNumber {
						ack.unwrapped = true
						break
					}
				}
			}
			self.ackUpdateCount += 1
			self.headAck = ack
			// no need to clean up `selectiveAcks` here
			// selective acks with sequence number <= head are ignored in a final pass during update
		}
	} else {
		// past the head
		// resend the head — fold this late ack's plaintext bit into the
		// head so the resend covers it. Copy-on-write: a prior Snapshot
		// may have published the current `headAck` pointer to writeAck,
		// which reads `unwrapped` without holding ackLock, so mutating
		// the struct in place would race. Swap in a fresh copy with the
		// bit set instead.
		if ack.unwrapped && self.headAck != nil && !self.headAck.unwrapped {
			updated := *self.headAck
			updated.unwrapped = true
			self.headAck = &updated
		}
		self.ackUpdateCount += 1
	}

	self.ackMonitor.NotifyAll()
}

// Snapshot returns a point-in-time view of the window: the head ack (nil
// until the first update), the update count, a channel notified on the next
// update, and the selective acks above the head. When `reset` is true the
// update count and selective acks are cleared — the head ack is kept so the
// writer can resend it — and a subsequent snapshot carries only new updates.
// The returned head ack pointer is shared with the window: the caller must
// treat it as read-only (Update folds in late acks copy-on-write).
func (self *sequenceAckWindow) Snapshot(reset bool) sequenceAckWindowSnapshot {
	self.ackLock.Lock()
	defer self.ackLock.Unlock()

	// build the selective-ack copy lazily so the common in-order case (a
	// cumulative head ack with no selective acks) allocates no map.
	var selectiveAcksAfterHead map[Id]*sequenceAck
	if 0 < self.ackUpdateCount {
		for messageId, ack := range self.selectiveAcks {
			if self.headAck.sequenceNumber < ack.sequenceNumber {
				if selectiveAcksAfterHead == nil {
					selectiveAcksAfterHead = map[Id]*sequenceAck{}
				}
				selectiveAcksAfterHead[messageId] = ack
			}
		}
	} else if 0 < len(self.selectiveAcks) {
		selectiveAcksAfterHead = maps.Clone(self.selectiveAcks)
	}

	snapshot := sequenceAckWindowSnapshot{
		ackNotify:      self.ackMonitor.NotifyChannel(),
		headAck:        self.headAck,
		ackUpdateCount: self.ackUpdateCount,
		selectiveAcks:  selectiveAcksAfterHead,
	}

	if reset {
		// keep the head ack in place. clear() reuses the live map's storage
		// instead of allocating a fresh map; the caller holds only a copy.
		self.ackUpdateCount = 0
		clear(self.selectiveAcks)
	}

	return snapshot
}

type sequenceContract struct {
	log                        Logger
	localId                    Id
	tag                        string
	contract                   *protocol.Contract
	contractId                 Id
	transferByteCount          ByteCount
	effectiveTransferByteCount ByteCount
	provideMode                protocol.ProvideMode

	minUpdateByteCount ByteCount

	path TransferPath

	// per-contract stats entry. Stored here (not on the owning sequence)
	// because a sequence can have multiple open contracts in flight during
	// contract rollover; keeping the entry on the sequence would misattribute
	// acks/updates for a non-current contract to whichever contract is
	// currently active.
	contractStatsEntry *contractStatsEntry

	ackedByteCount   ByteCount
	unackedByteCount ByteCount

	// provideTlsCertificate is the PEM-encoded X.509 chain (leaf first)
	// that the destination committed to as its server TLS identity for
	// this contract. Empty when the destination did not publish a
	// certificate via `ContractManager.SetProvideTlsCertificate`. The
	// SendSequence uses this to verify the peer presented during the
	// per-peer TLS handshake against the platform-signed contract.
	provideTlsCertificate [][]byte
	// destinationClientPublicKey is the peer's 32-byte Ed25519
	// long-lived public identity key, as committed by the platform in
	// `Contract.destination_client_public_key`. The sender uses it to
	// (a) verify `destinationClientKeySignedTlsCertificate` against
	// `provideTlsCertificate` — only then is the cert chain admitted
	// to the per-peer session's trusted set — and (b) verify the
	// peer's post-handshake identity proof exchanged inside the per-
	// peer TLS session. Empty when the contract carries no key.
	destinationClientPublicKey []byte
	// destinationClientKeySignedTlsCertificate is the peer's Ed25519
	// signature over the canonical concatenation of every PEM block
	// in `provideTlsCertificate`. The signing key is the peer's
	// long-lived client identity key (private half held only by the
	// peer); the verifier is `destinationClientPublicKey`. Empty when
	// the contract carries no signature.
	destinationClientKeySignedTlsCertificate []byte
}

func newSequenceContract(log Logger, tag string, contract *protocol.Contract, minUpdateByteCount ByteCount, contractFillFraction float32) (*sequenceContract, error) {
	storedContract := &protocol.StoredContract{}
	err := ProtoUnmarshal(contract.StoredContractBytes, storedContract)
	if err != nil {
		return nil, err
	}

	contractId, err := IdFromBytes(storedContract.ContractId)
	if err != nil {
		return nil, err
	}

	path, err := TransferPathFromBytes(
		storedContract.SourceId,
		storedContract.DestinationId,
		storedContract.StreamId,
	)
	if err != nil {
		return nil, err
	}

	// The platform-signed `StoredContract.ProvideTlsCertificate` is the
	// authoritative cert commitment (signed under `storedContractHmac`); the
	// outer `Contract.ProvideTlsCertificate` is a convenience copy for
	// clients that don't unmarshal the stored bytes. Prefer the stored value;
	// fall back to the outer value only when the inner is missing.
	provideTlsCertificate := storedContract.ProvideTlsCertificate
	if len(provideTlsCertificate) == 0 && contract != nil {
		provideTlsCertificate = contract.ProvideTlsCertificate
	}

	// Same prefer-stored-fallback-to-outer convention for the destination's
	// client-identity public key and the destination's signature over the
	// cert chain (Option 1 of the long-lived-identity verification design).
	destinationClientPublicKey := storedContract.DestinationClientPublicKey
	if len(destinationClientPublicKey) == 0 && contract != nil {
		destinationClientPublicKey = contract.DestinationClientPublicKey
	}
	destinationClientKeySignedTlsCertificate := storedContract.DestinationClientKeySignedTlsCertificate
	if len(destinationClientKeySignedTlsCertificate) == 0 && contract != nil {
		destinationClientKeySignedTlsCertificate = contract.DestinationClientKeySignedTlsCertificate
	}

	return &sequenceContract{
		log:                                      log,
		localId:                                  NewId(),
		tag:                                      tag,
		contract:                                 contract,
		contractId:                               contractId,
		transferByteCount:                        ByteCount(storedContract.TransferByteCount),
		effectiveTransferByteCount:               ByteCount(float32(storedContract.TransferByteCount) * contractFillFraction),
		provideMode:                              contract.ProvideMode,
		minUpdateByteCount:                       minUpdateByteCount,
		path:                                     path,
		ackedByteCount:                           ByteCount(0),
		unackedByteCount:                         ByteCount(0),
		provideTlsCertificate:                    provideTlsCertificate,
		destinationClientPublicKey:               destinationClientPublicKey,
		destinationClientKeySignedTlsCertificate: destinationClientKeySignedTlsCertificate,
	}, nil
}

// update debits `byteCount` against the contract's remaining capacity,
// floored up to the contract's min update byte count, and adds it to the
// unacked count. It returns true when the contract can still cover the
// message and false when the contract is full — the message must not be
// accepted against it.
func (self *sequenceContract) update(byteCount ByteCount) bool {
	effectiveByteCount := max(self.minUpdateByteCount, byteCount)

	if self.effectiveTransferByteCount < self.ackedByteCount+self.unackedByteCount+effectiveByteCount {
		// doesn't fit in contract
		// if self.log.V(1).Enabled() {
		self.log.Infof(
			"[%s]debit contract %s failed +%d->%d (%d/%d total %.1f%% full)\n",
			self.tag,
			self.contractId,
			effectiveByteCount,
			self.ackedByteCount+self.unackedByteCount+effectiveByteCount,
			self.ackedByteCount+self.unackedByteCount,
			self.effectiveTransferByteCount,
			100.0*float32(self.ackedByteCount+self.unackedByteCount)/float32(self.effectiveTransferByteCount),
		)
		// }
		return false
	}
	self.unackedByteCount += effectiveByteCount
	if self.log.V(1).Enabled() {
		self.log.Infof(
			"[%s]debit contract %s passed +%d->%d (%d/%d total %.1f%% full)\n",
			self.tag,
			self.contractId,
			effectiveByteCount,
			self.ackedByteCount+self.unackedByteCount,
			self.ackedByteCount+self.unackedByteCount,
			self.effectiveTransferByteCount,
			100.0*float32(self.ackedByteCount+self.unackedByteCount)/float32(self.effectiveTransferByteCount),
		)
	}
	return true
}

// ack credits `byteCount` against the contract, floored up to the min update
// byte count, moving it from the unacked to the acked count. It panics when
// the unacked count does not cover the credit — an accounting violation that
// must never occur.
func (self *sequenceContract) ack(byteCount ByteCount) {
	effectiveByteCount := max(self.minUpdateByteCount, byteCount)

	if self.unackedByteCount < effectiveByteCount {
		// debug.PrintStack()
		panic(fmt.Errorf("Bad accounting %d <> %d", self.unackedByteCount, byteCount))
	}

	self.unackedByteCount -= effectiveByteCount
	self.ackedByteCount += effectiveByteCount
}

type ForwardBufferSettings struct {
	IdleTimeout time.Duration

	SequenceBufferSize int

	WriteTimeout time.Duration
}

type ForwardBuffer struct {
	ctx    context.Context
	client *Client

	forwardBufferSettings *ForwardBufferSettings

	mutex sync.Mutex
	// destination -> forward sequence
	forwardSequences map[TransferPath]*ForwardSequence
}

func NewForwardBuffer(ctx context.Context,
	client *Client,
	forwardBufferSettings *ForwardBufferSettings) *ForwardBuffer {
	return &ForwardBuffer{
		ctx:                   ctx,
		client:                client,
		forwardBufferSettings: forwardBufferSettings,
		forwardSequences:      map[TransferPath]*ForwardSequence{},
	}
}

// Pack enqueues `forwardPack` for delivery on the forward sequence keyed by
// its destination, creating the sequence and starting its run-loop goroutine
// if none exists, then delegating to ForwardSequence.Pack. If the sequence
// shuts down between selection and enqueue, Pack retries once against a
// fresh sequence. On success the sequence takes ownership of the pack's
// frame bytes (they are returned to the message pool once the pack is
// forwarded or the sequence shuts down); on failure the caller retains
// ownership.
func (self *ForwardBuffer) Pack(forwardPack *ForwardPack, timeout time.Duration) (bool, error) {
	initForwardSequence := func(skip *ForwardSequence) *ForwardSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		forwardSequence, ok := self.forwardSequences[forwardPack.Destination]
		if ok {
			if skip == nil || skip != forwardSequence {
				return forwardSequence
			} else {
				forwardSequence.Cancel()
				// delete(self.forwardSequences, forwardPack.Destination)
			}
		}
		forwardSequence = NewForwardSequence(
			self.ctx,
			self.client,
			forwardPack.Destination,
			self.forwardBufferSettings,
		)
		self.forwardSequences[forwardPack.Destination] = forwardSequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				forwardSequence.Close()
				// clean up
				if forwardSequence == self.forwardSequences[forwardPack.Destination] {
					delete(self.forwardSequences, forwardPack.Destination)
				}
			}()
			forwardSequence.Run()
		})
		return forwardSequence
	}

	var forwardSequence *ForwardSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		forwardSequence = initForwardSequence(forwardSequence)
		if success, err = forwardSequence.Pack(forwardPack, timeout); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

// Close cancels every open forward sequence. Each sequence's run loop
// performs its own teardown — closing its multi-route writer — and the
// controller goroutine removes the sequence from the buffer afterwards.
func (self *ForwardBuffer) Close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	// the control of the sequence will close it
	for _, forwardSequence := range self.forwardSequences {
		forwardSequence.Cancel()
	}
}

// Cancel is identical to Close: it cancels every open forward sequence. The
// sequences shut down through their run-loop teardown and remove themselves
// from the buffer afterwards.
func (self *ForwardBuffer) Cancel() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, forwardSequence := range self.forwardSequences {
		forwardSequence.Cancel()
	}
}

// Flush cancels every open forward sequence, shutting down their run loops
// and closing their multi-route writers; buffered packs the run loop has not
// already forwarded are dropped when it exits. It is called by Client.Flush
// to discard pending forwards.
func (self *ForwardBuffer) Flush() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	// cancel all open sequences
	for _, forwardSequence := range self.forwardSequences {
		// if !destination.IsControlDestination() {
		forwardSequence.Cancel()
		// }
	}
}

type ForwardSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	client    *Client
	clientId  Id
	clientTag string
	log       Logger

	destination TransferPath

	forwardBufferSettings *ForwardBufferSettings

	packMutex sync.Mutex
	packs     chan *ForwardPack

	idleCondition *IdleCondition

	multiRouteWriter MultiRouteWriter
}

func NewForwardSequence(
	ctx context.Context,
	client *Client,
	destination TransferPath,
	forwardBufferSettings *ForwardBufferSettings) *ForwardSequence {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &ForwardSequence{
		ctx:                   cancelCtx,
		cancel:                cancel,
		client:                client,
		log:                   client.log,
		destination:           destination,
		forwardBufferSettings: forwardBufferSettings,
		packs:                 make(chan *ForwardPack, forwardBufferSettings.SequenceBufferSize),
		idleCondition:         NewIdleCondition(),
	}
}

// success, error
func (self *ForwardSequence) Pack(forwardPack *ForwardPack, timeout time.Duration) (bool, error) {
	self.packMutex.Lock()
	defer self.packMutex.Unlock()

	select {
	case <-forwardPack.Ctx.Done():
		return false, errors.New("Done.")
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	if timeout < 0 {
		select {
		case <-forwardPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- forwardPack:
			return true, nil
		}
	} else if timeout == 0 {
		select {
		case <-forwardPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- forwardPack:
			return true, nil
		default:
			return false, nil
		}
	} else {
		select {
		case <-forwardPack.Ctx.Done():
			return false, errors.New("Done.")
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		case self.packs <- forwardPack:
			return true, nil
		case <-time.After(timeout):
			return false, nil
		}
	}
}

// Run is the sequence's main loop, started once per ForwardSequence by
// ForwardBuffer.Pack and blocking until the sequence terminates. It opens a
// multi-route writer to the destination and forwards each pack from the
// queue over it, returning the pack's frame bytes to the message pool after
// each write. It exits when the sequence context is canceled, the pack
// channel closes, or the idle timeout elapses with no pending packs; on exit
// the multi-route writer is closed.
func (self *ForwardSequence) Run() {
	defer self.cancel()

	self.multiRouteWriter = self.client.RouteManager().OpenMultiRouteWriter(self.destination)
	defer self.client.RouteManager().CloseMultiRouteWriter(self.multiRouteWriter)

	// reusable idle timer (avoids a per-iteration time.After alloc on the
	// forward hot path). created already-fired; Reset before the blocking select
	// arms it (go1.23+ delivers no stale fire after Reset).
	idleTimer := time.NewTimer(0)
	defer idleTimer.Stop()

	for {
		checkpointId := self.idleCondition.Checkpoint()
		idleTimer.Reset(self.forwardBufferSettings.IdleTimeout)
		select {
		case <-self.ctx.Done():
			return
		case forwardPack, ok := <-self.packs:
			if !ok {
				return
			}
			c := func() error {
				transferFrameBytes := forwardPack.TransferFrameBytes
				if DebugTransferCopyOnWrite {
					transferFrameBytes = MessagePoolCopy(transferFrameBytes)
				}
				defer MessagePoolReturn(transferFrameBytes)
				return self.multiRouteWriter.Write(
					self.ctx,
					MessagePoolShareReadOnly(transferFrameBytes),
					self.forwardBufferSettings.WriteTimeout,
				)
			}
			if self.log.V(2).Enabled() {
				TraceWithReturn(
					fmt.Sprintf("[f]multi route write %s->%s s(%s)", self.clientTag, self.destination.DestinationId, self.destination.StreamId),
					c,
				)
			} else {
				err := c()
				if err != nil {
					self.log.V(2).Infof("[f]drop = %s", err)
				}
			}
		case <-idleTimer.C:
			done := false
			func() {
				self.packMutex.Lock()
				defer self.packMutex.Unlock()
				// idle timeout
				if self.idleCondition.Close(checkpointId) {
					done = true
				}
				// else there are pending updates
			}()
			if done {
				// close the sequence
				self.log.V(2).Infof("[f]exit idle timeout %s->%s s(%s)", self.clientTag, self.destination.DestinationId, self.destination.StreamId)
				return
			}
		}
	}
}

// Close shuts the sequence down: it cancels the sequence context and closes
// the pack channel, after which the run loop exits once the channel drains.
// It is called by the controller goroutine after the run loop has exited
// (see ForwardBuffer.Pack); Cancel alone skips the channel close.
func (self *ForwardSequence) Close() {
	self.cancel()

	func() {
		self.packMutex.Lock()
		defer self.packMutex.Unlock()
		close(self.packs)
	}()
}

// Cancel cancels the sequence context, causing the run loop to exit and
// close its multi-route writer. Unlike Close it does not close the pack
// channel.
func (self *ForwardSequence) Cancel() {
	self.cancel()
}

type PeerAudit struct {
	startTime           time.Time
	lastModifiedTime    time.Time
	Abuse               bool
	BadContractCount    int
	DiscardedByteCount  ByteCount
	DiscardedCount      int
	BadMessageByteCount ByteCount
	BadMessageCount     int
	SendByteCount       ByteCount
	SendCount           int
	ResendByteCount     ByteCount
	ResendCount         int
}

func NewPeerAudit(startTime time.Time) *PeerAudit {
	return &PeerAudit{
		startTime:           startTime,
		lastModifiedTime:    startTime,
		BadContractCount:    0,
		DiscardedByteCount:  ByteCount(0),
		DiscardedCount:      0,
		BadMessageByteCount: ByteCount(0),
		BadMessageCount:     0,
		SendByteCount:       ByteCount(0),
		SendCount:           0,
		ResendByteCount:     ByteCount(0),
		ResendCount:         0,
	}
}

// badMessage records a message that could not be processed — for example an
// unparsable or unverifiable pack — incrementing the bad-message count and
// byte count.
func (self *PeerAudit) badMessage(byteCount ByteCount) {
	self.BadMessageCount += 1
	self.BadMessageByteCount += byteCount
}

// discard records a message the sequence dropped without delivery,
// incrementing the discarded count and byte count.
func (self *PeerAudit) discard(byteCount ByteCount) {
	self.DiscardedCount += 1
	self.DiscardedByteCount += byteCount
}

// badContract records a contract that failed verification or could not be
// activated, incrementing the bad-contract count.
func (self *PeerAudit) badContract() {
	self.BadContractCount += 1
}

// received records a message delivered to the application, incrementing the
// send count and byte count — from the audit's per-source perspective, a
// message the peer sent.
func (self *PeerAudit) received(byteCount ByteCount) {
	self.SendCount += 1
	self.SendByteCount += byteCount
}

// resend records a retransmitted message — one the peer sent again after a
// first delivered or queued copy — incrementing the resend count and byte
// count.
func (self *PeerAudit) resend(byteCount ByteCount) {
	self.ResendCount += 1
	self.ResendByteCount += byteCount
}

type SequencePeerAudit struct {
	client           *Client
	log              Logger
	source           TransferPath
	maxAuditDuration time.Duration

	peerAudit *PeerAudit
}

func NewSequencePeerAudit(client *Client, source TransferPath, maxAuditDuration time.Duration) *SequencePeerAudit {
	return &SequencePeerAudit{
		client:           client,
		log:              client.log,
		source:           source,
		maxAuditDuration: maxAuditDuration,
		peerAudit:        nil,
	}
}

// Update applies `callback` to the peer audit for the sequence's source,
// creating a fresh audit if none is active, or completing and sending the
// current one first when it has exceeded `maxAuditDuration`. The callback
// runs synchronously and may read or mutate the audit's counters; the
// audit's last-modified time is refreshed on every update.
func (self *SequencePeerAudit) Update(callback func(*PeerAudit)) {
	auditTime := time.Now()

	if self.peerAudit != nil && self.maxAuditDuration <= auditTime.Sub(self.peerAudit.startTime) {
		self.Complete()
	}
	if self.peerAudit == nil {
		self.peerAudit = NewPeerAudit(auditTime)
	}

	callback(self.peerAudit)
	self.peerAudit.lastModifiedTime = auditTime
	// TODO auto complete the peer audit after timeout
}

// Complete ships the accumulated peer audit for the sequence's source to the
// platform as an out-of-band TransferPeerAudit frame (via ClientOob), then
// resets the active audit so the next Update starts fresh. It is a no-op
// when no audit is active. The frame carries the counters recorded through
// Update — messages received, discarded, resent, bad messages, bad
// contracts, and the abuse flag — which feed the platform's peer-trust and
// abuse decisions.
func (self *SequencePeerAudit) Complete() {
	if self.peerAudit == nil {
		return
	}

	peerAudit := &protocol.PeerAudit{
		PeerId:              self.source.SourceId.Bytes(),
		StreamId:            self.source.StreamId.Bytes(),
		Duration:            uint64(math.Ceil((self.peerAudit.lastModifiedTime.Sub(self.peerAudit.startTime)).Seconds())),
		Abuse:               self.peerAudit.Abuse,
		BadContractCount:    uint64(self.peerAudit.BadContractCount),
		DiscardedByteCount:  uint64(self.peerAudit.DiscardedByteCount),
		DiscardedCount:      uint64(self.peerAudit.DiscardedCount),
		BadMessageByteCount: uint64(self.peerAudit.BadMessageByteCount),
		BadMessageCount:     uint64(self.peerAudit.BadMessageCount),
		SendByteCount:       uint64(self.peerAudit.SendByteCount),
		SendCount:           uint64(self.peerAudit.SendCount),
		ResendByteCount:     uint64(self.peerAudit.ResendByteCount),
		ResendCount:         uint64(self.peerAudit.ResendCount),
	}
	frame, err := ToFrame(peerAudit, DefaultProtocolVersion)
	if err != nil {
		self.log.Errorf("[c]could not create audit frame = %s", err)
		return
	}
	self.client.ClientOob().SendControl(
		[]*protocol.Frame{frame},
		func(resultFrames []*protocol.Frame, err error) {
			if err != nil {
				self.log.Errorf("[c]audit send error = %s", err)
			}
		},
	)
	self.peerAudit = nil
}

// MessageByteCount returns the total byte count across all frames,
// including contract frames.
func MessageByteCount(frames []*protocol.Frame) ByteCount {
	// messageByteCount := ByteCount(0)
	// for _, frame := range frames {
	// 	if frame.MessageType != protocol.MessageType_TransferContract {
	// 		messageByteCount += ByteCount(len(frame.MessageBytes))
	// 	}
	// }
	// return messageByteCount
	messageByteCount := ByteCount(0)
	for _, frame := range frames {
		messageByteCount += ByteCount(len(frame.MessageBytes))
	}
	return messageByteCount
}

// func MessageFrames(frames []*protocol.Frame) []*protocol.Frame {
// 	messages := []*protocol.Frame{}
// 	for _, frame := range frames {
// 		if frame.MessageType != protocol.MessageType_TransferContract {
// 			messages = append(messages, frame)
// 		}
// 	}
// 	return messages
// }
