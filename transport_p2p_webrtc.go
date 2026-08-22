package connect

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/datachannel"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/protocol"
)

var (
	ipv6CheckOnce sync.Once
	ipv6OK        bool
)

// stunIPv6Addr is an IPv6 address of one of the STUN servers from
// DefaultWebRtcSettings. Used by ipv6Available to probe whether the host can
// actually route IPv6 UDP to the internet — some servers have link-local or
// tunnel IPv6 but no default route, making pion/ICE's IPv6 candidate gathering
// fail with "network is unreachable" on every STUN attempt.
const stunIPv6Addr = "[2001:4860:4864:5:8000::1]:19302"

// ipv6Available checks once whether the host can reach an IPv6 STUN server.
// When it returns false, ICE candidate gathering is restricted to IPv4, saving
// connection setup time and eliminating log noise on IPv6-unreachable hosts.
func ipv6Available() bool {
	ipv6CheckOnce.Do(func() {
		conn, err := net.DialTimeout("udp6", stunIPv6Addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			ipv6OK = true
		}
	})
	return ipv6OK
}

type WebRtcConn interface {
	net.Conn
	Connected() bool
	AddConnectedCallback(func(connected bool)) func()
}

type SignalSender interface {
	SendSignal(path TransferPath, signal *protocol.Frame)
}

type SignalReceiver interface {
	ReceiveSignal(source TransferPath, signal *protocol.Frame) error
}

// conforms to `SignalSender`
type ClientSignalSender struct {
	client *Client
}

func NewClientSignalSender(client *Client) *ClientSignalSender {
	return &ClientSignalSender{
		client: client,
	}
}

func (self *ClientSignalSender) SendSignal(path TransferPath, signal *protocol.Frame) {
	self.client.Send(signal, path.DestinationMask(), nil)
}

type clientSignalReceiver struct {
	client           *Client
	receiver         SignalReceiver
	ctx              context.Context
	cancel           context.CancelFunc
	queueLock        sync.Mutex
	closed           bool
	queueLimit       int
	receiveFrames    []*receivedSignalFrame
	receiveFrameHead int
	queueMonitor     *Monitor
	spaceMonitor     *Monitor
}

type receivedSignalFrame struct {
	source          TransferPath
	frame           *protocol.Frame
	exchangeSignals *protocol.ExchangeSignals
	candidateBatch  bool
	key             receivedSignalFrameKey
}

type receivedSignalFrameKey struct {
	source   TransferPath
	streamId Id
}

func ReceiveSignalsFromClient(client *Client, receiver SignalReceiver) func() {
	cancelCtx, cancel := context.WithCancel(client.Ctx())
	bufferSize := defaultTransferBufferSize
	if client.settings != nil && client.settings.ReceiveBufferSettings != nil {
		bufferSize = client.settings.ReceiveBufferSettings.SequenceBufferSize
	}
	bufferSize = max(1, bufferSize)
	r := &clientSignalReceiver{
		client:       client,
		receiver:     receiver,
		ctx:          cancelCtx,
		cancel:       cancel,
		queueLimit:   bufferSize,
		queueMonitor: NewMonitor(),
		spaceMonitor: NewMonitor(),
	}
	go HandleError(r.run, cancel)
	go HandleError(func() {
		<-cancelCtx.Done()
		r.Close()
	})
	unsub := client.AddReceiveCallback(r.Receive)
	return func() {
		unsub()
		r.Close()
	}
}

// ReceiveFunction
func (self *clientSignalReceiver) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	for _, frame := range frames {
		self.handleControlFrame(source, frame)
	}
}

func (self *clientSignalReceiver) handleControlFrame(source TransferPath, frame *protocol.Frame) {
	switch frame.MessageType {
	case protocol.MessageType_TransferExchangeSignals:
		select {
		case <-self.ctx.Done():
			return
		default:
		}

		received, err := newReceivedSignalFrame(source, frame)
		if err != nil {
			self.client.log.Infof("[signal]receive frame err=%s\n", err)
			return
		}
		if !self.enqueue(received) {
			received.Close()
		}
	}
}

func (self *clientSignalReceiver) Close() {
	self.queueLock.Lock()
	if !self.closed {
		self.closed = true
		for _, received := range self.receiveFrames[self.receiveFrameHead:] {
			if received != nil {
				received.Close()
			}
		}
		self.receiveFrames = nil
		self.receiveFrameHead = 0
	}
	self.queueLock.Unlock()
	self.cancel()
	self.queueMonitor.NotifyAll()
	self.spaceMonitor.NotifyAll()
}

func (self *clientSignalReceiver) run() {
	for {
		received := self.dequeue()
		if received == nil {
			return
		}
		func() {
			defer received.Close()
			if err := received.prepareFrame(); err != nil {
				self.client.log.Infof("[signal]receive frame err=%s\n", err)
				return
			}
			if err := self.receiver.ReceiveSignal(received.source, received.frame); err != nil {
				self.client.log.Infof("[signal]receive err=%s\n", err)
			}
		}()
	}
}

func newReceivedSignalFrame(source TransferPath, frame *protocol.Frame) (*receivedSignalFrame, error) {
	exchangeSignals := &protocol.ExchangeSignals{}
	if err := ProtoUnmarshal(frame.MessageBytes, exchangeSignals); err != nil {
		return &receivedSignalFrame{
			source: source,
			frame: &protocol.Frame{
				MessageType:  frame.MessageType,
				Raw:          frame.Raw,
				MessageBytes: MessagePoolCopy(frame.MessageBytes),
			},
		}, nil
	}

	exchangeSignals = cloneExchangeSignals(exchangeSignals)
	candidateBatch := false
	var key receivedSignalFrameKey
	if isCandidateBatch(exchangeSignals) {
		if streamId, err := IdFromBytes(exchangeSignals.StreamId); err == nil {
			candidateBatch = true
			key = receivedSignalFrameKey{
				source:   source,
				streamId: streamId,
			}
		}
	}
	var messageBytes []byte
	if !candidateBatch {
		messageBytes = MessagePoolCopy(frame.MessageBytes)
	}
	received := &receivedSignalFrame{
		source: source,
		frame: &protocol.Frame{
			MessageType:  frame.MessageType,
			Raw:          frame.Raw,
			MessageBytes: messageBytes,
		},
		exchangeSignals: exchangeSignals,
	}
	if candidateBatch {
		received.candidateBatch = true
		received.key = key
	} else {
		received.exchangeSignals = nil
	}
	return received, nil
}

func (self *receivedSignalFrame) Close() {
	if self.frame != nil {
		if self.frame.MessageBytes != nil {
			MessagePoolReturn(self.frame.MessageBytes)
		}
		self.frame = nil
	}
	self.exchangeSignals = nil
}

func (self *receivedSignalFrame) prepareFrame() error {
	if self.frame == nil || self.frame.MessageBytes != nil || self.exchangeSignals == nil {
		return nil
	}
	messageBytes, err := ProtoMarshal(self.exchangeSignals)
	if err != nil {
		return err
	}
	self.frame.MessageBytes = messageBytes
	return nil
}

func (self *receivedSignalFrame) isCandidateBatch() bool {
	return isCandidateBatch(self.exchangeSignals)
}

func isCandidateBatch(exchangeSignals *protocol.ExchangeSignals) bool {
	if exchangeSignals == nil || exchangeSignals.ResetSignals || len(exchangeSignals.Signals) == 0 {
		return false
	}
	for _, signal := range exchangeSignals.Signals {
		if signal == nil || signal.SignalType != protocol.SignalType_IceCandidate {
			return false
		}
	}
	return true
}

func (self *receivedSignalFrame) appendCandidateBatch(received *receivedSignalFrame) error {
	if !self.candidateBatch || !received.candidateBatch || self.key != received.key {
		return fmt.Errorf("candidate batch mismatch")
	}
	self.exchangeSignals.Signals = append(self.exchangeSignals.Signals, received.exchangeSignals.Signals...)
	return nil
}

func (self *clientSignalReceiver) enqueue(received *receivedSignalFrame) bool {
	for {
		var spaceNotify chan struct{}
		self.queueLock.Lock()
		if self.closed || self.ctx.Err() != nil {
			self.queueLock.Unlock()
			return false
		}
		if received.candidateBatch {
			if batch := self.tailLocked(); batch != nil && batch.candidateBatch && batch.key == received.key {
				err := batch.appendCandidateBatch(received)
				self.queueLock.Unlock()
				if err != nil {
					self.client.log.Infof("[signal]coalesce candidate err=%s\n", err)
					return false
				}
				received.Close()
				return true
			}
		}
		if self.queueLenLocked() < self.queueLimit {
			self.receiveFrames = append(self.receiveFrames, received)
			self.queueLock.Unlock()
			self.queueMonitor.NotifyAll()
			return true
		}
		spaceNotify = self.spaceMonitor.NotifyChannel()
		self.queueLock.Unlock()

		select {
		case <-self.ctx.Done():
			return false
		case <-spaceNotify:
		}
	}
}

func (self *clientSignalReceiver) queueLenLocked() int {
	return len(self.receiveFrames) - self.receiveFrameHead
}

func (self *clientSignalReceiver) tailLocked() *receivedSignalFrame {
	if self.queueLenLocked() == 0 {
		return nil
	}
	return self.receiveFrames[len(self.receiveFrames)-1]
}

func (self *clientSignalReceiver) dequeue() *receivedSignalFrame {
	for {
		var queueNotify chan struct{}
		self.queueLock.Lock()
		if 0 < self.queueLenLocked() {
			received := self.receiveFrames[self.receiveFrameHead]
			self.receiveFrames[self.receiveFrameHead] = nil
			self.receiveFrameHead += 1
			if self.receiveFrameHead == len(self.receiveFrames) {
				self.receiveFrames = self.receiveFrames[:0]
				self.receiveFrameHead = 0
			}
			self.queueLock.Unlock()
			self.spaceMonitor.NotifyAll()
			return received
		}
		if self.closed || self.ctx.Err() != nil {
			self.queueLock.Unlock()
			return nil
		}
		queueNotify = self.queueMonitor.NotifyChannel()
		self.queueLock.Unlock()

		select {
		case <-self.ctx.Done():
			return nil
		case <-queueNotify:
		}
	}
}

func cloneExchangeSignals(exchangeSignals *protocol.ExchangeSignals) *protocol.ExchangeSignals {
	out := &protocol.ExchangeSignals{
		StreamId:     append([]byte(nil), exchangeSignals.StreamId...),
		ResetSignals: exchangeSignals.ResetSignals,
		Signals:      make([]*protocol.ExchangeSignal, 0, len(exchangeSignals.Signals)),
	}
	for _, signal := range exchangeSignals.Signals {
		out.Signals = append(out.Signals, cloneExchangeSignal(signal))
	}
	return out
}

func cloneExchangeSignal(signal *protocol.ExchangeSignal) *protocol.ExchangeSignal {
	if signal == nil {
		return nil
	}
	return &protocol.ExchangeSignal{
		SignalType:   signal.SignalType,
		Sdp:          append([]byte(nil), signal.Sdp...),
		IceCandidate: append([]byte(nil), signal.IceCandidate...),
	}
}

func DefaultWebRtcSettings() *WebRtcSettings {
	return &WebRtcSettings{
		// FIXME
		// SendBufferSize: mib(1),

		ReceiveBufferSize: mib(4),
		// Pion's receive MTU is a per-packet demux buffer, not the SCTP
		// receive window. Its SCTP path MTU stays below Ethernet's 1500-byte
		// packet size, so the former 4 KiB value only enlarged persistent and
		// scratch buffers with no effect on the actual window.
		ReceiveMtu:          ByteCount(1500),
		DisconnectedTimeout: 30 * time.Second,
		FailedTimeout:       30 * time.Second,
		KeepAliveTimeout:    1 * time.Second,

		// Pion's Reno-style congestion avoidance otherwise adds one ~1.2 KiB
		// MTU only after a complete cwnd is acknowledged. On a measured 50 ms
		// path with independent wireless loss, recovery from a 20-50 KiB
		// window took seconds and held throughput below 1 MiB/s. Four MTUs
		// retains loss response and a zero minimum, while making recovery
		// competitive.
		SctpCwndCAStep: 4 * 1200,
		// ICE consent can remain healthy while the SCTP/data plane is
		// half-open. Once a write leaves unacknowledged SCTP bytes, require a
		// reverse SCTP packet within this bound or rebuild the association.
		// The worker is lazy and has no idle timer/radio wakeups.
		SctpNoProgressTimeout: 10 * time.Second,

		DataChannelLabel: "data",
		IceServerUrls: []string{
			"stun:openrelay.metered.ca:80",
			"stun:openrelay.metered.ca:443",
			"stun:stun.stunprotocol.org:3478",
			"stun:stun.l.google.com:19302",
			"stun:stun1.l.google.com:19302",
			"stun:stun2.l.google.com:19302",
			"stun:stun3.l.google.com:19302",
			"stun:stun4.l.google.com:19302",
		},
	}
}

type WebRtcSettings struct {
	// Log, when set, is used by the webrtc manager, p2p transports, and the
	// pion stack. nil resolves to DefaultLogger().
	// NewClientWithTag propagates the client log here when nil.
	Log Logger

	ReceiveBufferSize   ByteCount
	ReceiveMtu          ByteCount
	DisconnectedTimeout time.Duration
	FailedTimeout       time.Duration
	KeepAliveTimeout    time.Duration

	// SCTP congestion controls are byte counts. Zero retains Pion's RFC-style
	// default. CwndCAStep changes only additive recovery, not the loss
	// response or minimum window.
	SctpCwndCAStep uint32
	// SctpNoProgressTimeout bounds a half-open data plane after outbound
	// activity. Zero disables the watchdog. It observes reverse SCTP
	// packets, not application callbacks, so transfer send/receive/forward
	// callbacks retain their synchronous backpressure semantics.
	SctpNoProgressTimeout time.Duration

	DataChannelLabel string

	// add stun:xxx urls here
	IceServerUrls []string
}

// pionLoggerFactory routes pion logs through a Logger, so the webrtc stack
// follows the same logger as the peer connection that created it (and is
// silenced with it). Without this, pion writes to its own default factory
// (stdout), bypassing per-client logging entirely.
// pion levels map: Error->Errorf, Warn->Warningf, Info->V(1), Debug/Trace->V(2).
type pionLoggerFactory struct {
	log Logger
}

func (self *pionLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &pionLeveledLogger{
		log:   self.log,
		scope: scope,
	}
}

type pionLeveledLogger struct {
	log   Logger
	scope string
}

func (self *pionLeveledLogger) Trace(msg string) {
	self.log.V(2).Infof("[pion:%s]%s", self.scope, msg)
}

func (self *pionLeveledLogger) Tracef(format string, args ...any) {
	if v := self.log.V(2); v.Enabled() {
		v.Infof("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Debug(msg string) {
	self.log.V(2).Infof("[pion:%s]%s", self.scope, msg)
}

func (self *pionLeveledLogger) Debugf(format string, args ...any) {
	if v := self.log.V(2); v.Enabled() {
		v.Infof("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Info(msg string) {
	self.log.V(1).Infof("[pion:%s]%s", self.scope, msg)
}

func (self *pionLeveledLogger) Infof(format string, args ...any) {
	if v := self.log.V(1); v.Enabled() {
		v.Infof("[pion:"+self.scope+"]"+format, args...)
	}
}

func (self *pionLeveledLogger) Warn(msg string) {
	self.log.Warningf("[pion:%s]%s", self.scope, msg)
}

func (self *pionLeveledLogger) Warnf(format string, args ...any) {
	self.log.Warningf("[pion:"+self.scope+"]"+format, args...)
}

func (self *pionLeveledLogger) Error(msg string) {
	self.log.Errorf("[pion:%s]%s", self.scope, msg)
}

func (self *pionLeveledLogger) Errorf(format string, args ...any) {
	self.log.Errorf("[pion:"+self.scope+"]"+format, args...)
}

type WebRtcManager struct {
	ctx          context.Context
	log          Logger
	signalSender SignalSender
	settings     *WebRtcSettings

	stateLock sync.Mutex
	peerConns map[peerConnKey]*peerConn
}

func NewWebRtcManager(ctx context.Context, signalSender SignalSender, settings *WebRtcSettings) *WebRtcManager {
	return &WebRtcManager{
		ctx:          ctx,
		log:          loggerOrDefault(settings.Log),
		signalSender: signalSender,
		settings:     settings,
		peerConns:    map[peerConnKey]*peerConn{},
	}
}

// SignalReceiver
func (self *WebRtcManager) ReceiveSignal(source TransferPath, frame *protocol.Frame) error {
	message, err := FromFrame(frame)
	if err != nil {
		return err
	}
	switch v := message.(type) {
	case *protocol.ExchangeSignals:
		streamId, err := IdFromBytes(v.StreamId)
		if err != nil {
			return err
		}
		key := peerConnKey{
			PeerId:   source.SourceId,
			StreamId: streamId,
		}
		var conn *peerConn
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			conn = self.peerConns[key]
			if self.log.V(2).Enabled() {
				if conn == nil {
					self.log.Infof("[signal]miss %s (%v)\n", key, self.peerConns)
				}
			}
		}()
		if conn != nil {
			for _, signal := range v.Signals {
				self.log.V(2).Infof("[signal]%s\n", signal.SignalType)
				err := conn.ReceiveSignalFromPeer(signal)
				if err != nil {
					return err
				}
			}
		}

	}
	return nil
}

func (self *WebRtcManager) NewP2pConnActive(ctx context.Context, path TransferPath) (WebRtcConn, error) {
	return self.newP2pConn(ctx, path, true)
}

func (self *WebRtcManager) NewP2pConnPassive(ctx context.Context, path TransferPath) (WebRtcConn, error) {
	return self.newP2pConn(ctx, path, false)
}

func (self *WebRtcManager) newP2pConn(ctx context.Context, path TransferPath, active bool) (conn *peerConn, err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	key := peerConnKey{
		PeerId:   path.DestinationId,
		StreamId: path.StreamId,
	}

	conn, err = newPeerConn(ctx, key, path.SourceId, active, self.signalSender, self.settings)
	if err != nil {
		return
	}
	go HandleError(func() {
		defer func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			conn.Cancel()
			if conn == self.peerConns[key] {
				delete(self.peerConns, key)
			}
		}()
		conn.Run()
	})

	replacedConn := self.peerConns[key]
	if replacedConn != nil {
		replacedConn.Cancel()
	}
	self.peerConns[key] = conn
	return
}

type peerConnKey struct {
	PeerId   Id
	StreamId Id
}

func (self peerConnKey) String() string {
	return fmt.Sprintf("s(%s) <>%s", self.StreamId, self.PeerId)
}

// conforms to WebRtcConn
type peerConn struct {
	ctx    context.Context
	cancel context.CancelFunc
	log    Logger

	key      peerConnKey
	sourceId Id
	active   bool

	signalSender SignalSender
	settings     *WebRtcSettings

	// api *webrtc.API
	pc *webrtc.PeerConnection

	connectedCallbacks *CallbackList[func(connected bool)]
	connMonitor        *Monitor

	stateLock sync.Mutex
	// signals []*protocol.ExchangeSignal
	conn      datachannel.ReadWriteCloserDeadliner
	connected bool
	offer     *protocol.ExchangeSignal
	answer    *protocol.ExchangeSignal

	readDeadline  time.Time
	writeDeadline time.Time

	outboundProgress  chan struct{}
	progressWatchOnce sync.Once
}

func newPeerConn(ctx context.Context, key peerConnKey, sourceId Id, active bool, signalSender SignalSender, settings *WebRtcSettings) (*peerConn, error) {
	pc, err := createWebRtcPeerConnection(ctx, active, settings)
	if err != nil {
		return nil, err
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	conn := &peerConn{
		ctx:          cancelCtx,
		cancel:       cancel,
		log:          loggerOrDefault(settings.Log),
		key:          key,
		sourceId:     sourceId,
		active:       active,
		signalSender: signalSender,
		settings:     settings,
		// api:                api,
		pc:                 pc,
		connectedCallbacks: NewCallbackList[func(connected bool)](),
		connMonitor:        NewMonitor(),
		outboundProgress:   make(chan struct{}, 1),
	}
	return conn, nil
}

func (self *peerConn) Run() {
	defer func() {
		self.cancel()

		self.stateLock.Lock()
		conn := self.conn
		self.stateLock.Unlock()
		if conn != nil {
			conn.Close()
		}

		self.pc.Close()
		self.connMonitor.NotifyAll()
	}()

	self.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		connected := state == webrtc.ICEConnectionStateConnected
		self.log.V(2).Infof("[peerconn]state=%v (%t)\n", state, connected)
		self.setConnected(connected)
	})

	self.addIceCandidates()

	if self.active {
		dc, err := self.pc.CreateDataChannel(self.settings.DataChannelLabel, nil)
		if err != nil {
			return
		}

		dc.OnOpen(func() {
			self.setOpenDataChannel(dc)
		})
	} else {
		self.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			dc.OnOpen(func() {
				self.setOpenDataChannel(dc)
			})
		})
	}

	if self.active {
		offer, err := self.pc.CreateOffer(nil)
		if err != nil {
			return
		}
		err = self.pc.SetLocalDescription(offer)
		if err != nil {
			return
		}

		offerBytes, err := json.Marshal(&offer)
		if err != nil {
			return
		}

		signal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_SdpOffer,
			Sdp:        offerBytes,
		}
		self.setOfferSignal(signal)
		self.sendSignal(signal)
	} else {
		signal := &protocol.ExchangeSignal{
			SignalType: protocol.SignalType_WaitingForSdpOffer,
		}
		self.sendSignal(signal)
	}

	select {
	case <-self.ctx.Done():
	}
}

func (self *peerConn) ReceiveSignalFromPeer(signal *protocol.ExchangeSignal) error {
	switch signal.SignalType {
	case protocol.SignalType_SdpOffer:
		if !self.active && self.setOfferSignal(signal) {
			var offer webrtc.SessionDescription
			err := json.Unmarshal(signal.Sdp, &offer)
			if err != nil {
				return err
			}
			err = self.pc.SetRemoteDescription(offer)
			if err != nil {
				return err
			}
			answer, err := self.pc.CreateAnswer(nil)
			if err != nil {
				return err
			}
			err = self.pc.SetLocalDescription(answer)
			if err != nil {
				return err
			}

			answerBytes, err := json.Marshal(&answer)
			if err != nil {
				return err
			}

			signal := &protocol.ExchangeSignal{
				SignalType: protocol.SignalType_SdpAnswer,
				Sdp:        answerBytes,
			}
			self.setAnswerSignal(signal)
			self.sendSignal(signal)
		}
	case protocol.SignalType_SdpAnswer:
		if self.active && self.setAnswerSignal(signal) {
			var answer webrtc.SessionDescription
			err := json.Unmarshal(signal.Sdp, &answer)
			if err != nil {
				return err
			}
			err = self.pc.SetRemoteDescription(answer)
			if err != nil {
				return err
			}
		}
	case protocol.SignalType_IceCandidate:
		var candidate webrtc.ICECandidateInit
		err := json.Unmarshal(signal.IceCandidate, &candidate)
		if err != nil {
			return err
		}
		err = self.pc.AddICECandidate(candidate)
		if err != nil {
			return err
		}
	case protocol.SignalType_WaitingForSdpOffer:
		if self.active {

			if self.answerSignal() == nil {
				if signal := self.offerSignal(); signal != nil {
					self.sendSignal(signal)
				}
			} else {
				// already negotiated an answer with a peer
				// cancel this connection so a new one can start in the expected state
				self.cancel()
			}
		}
		// else ignore
	}
	return nil
}

func (self *peerConn) addIceCandidates() {
	self.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		candidateBytes, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			return
		}
		signal := &protocol.ExchangeSignal{
			SignalType:   protocol.SignalType_IceCandidate,
			IceCandidate: candidateBytes,
		}
		self.sendSignal(signal)
	})
}

func (self *peerConn) Connected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.connected
}

func (self *peerConn) setConnected(connected bool) {
	changed := false

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.connected != connected {
			self.connected = connected
			changed = true
		}
	}()

	if changed {
		if connected {
			self.log.Infof("🔗 [signal] peer connected %s\n", self.key.String())
		} else {
			self.log.Infof("🔗 [signal] peer disconnected %s\n", self.key.String())
		}
		self.connectedChanged(self.Connected())
	}
}

func (self *peerConn) AddConnectedCallback(connectedCallback func(connected bool)) func() {
	callbackId := self.connectedCallbacks.Add(connectedCallback)
	return func() {
		self.connectedCallbacks.Remove(callbackId)
	}
}

func (self *peerConn) connectedChanged(connected bool) {
	for _, callback := range self.connectedCallbacks.Get() {
		HandleError(func() {
			callback(connected)
		})
	}
}

func (self *peerConn) setOfferSignal(offer *protocol.ExchangeSignal) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.offer != nil {
		return false
	}
	self.offer = offer
	return true
}

func (self *peerConn) offerSignal() *protocol.ExchangeSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.offer
}

func (self *peerConn) setAnswerSignal(answer *protocol.ExchangeSignal) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.answer != nil {
		return false
	}
	self.answer = answer
	return true
}

func (self *peerConn) answerSignal() *protocol.ExchangeSignal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.answer
}

func (self *peerConn) sendSignal(signal *protocol.ExchangeSignal) {
	signals := &protocol.ExchangeSignals{
		StreamId: self.key.StreamId.Bytes(),
		Signals:  []*protocol.ExchangeSignal{signal},
	}
	self.signalSender.SendSignal(
		DestinationId(self.key.PeerId).AddSource(self.sourceId),
		RequireToFrameWithDefaultProtocolVersion(signals),
	)
}

func (self *peerConn) setOpenDataChannel(dc *webrtc.DataChannel) error {
	conn, err := detachWithDeadline(dc)
	if err != nil {
		return err
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.conn != nil {
			self.conn.Close()
		}
		self.conn = conn
		self.connMonitor.NotifyAll()
	}()

	return nil
}

func (self *peerConn) dataChannelConn(deadline time.Time) (datachannel.ReadWriteCloserDeadliner, error) {
	conn := func() (datachannel.ReadWriteCloserDeadliner, chan struct{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.conn, self.connMonitor.NotifyChannel()
	}
	for {
		c, update := conn()
		if c != nil {
			return c, nil
		}
		if deadline.IsZero() {
			select {
			case <-self.ctx.Done():
				return nil, os.ErrClosed
			case <-update:
			}
		} else {
			timeout := deadline.Sub(time.Now())
			if timeout <= 0 {
				return nil, os.ErrDeadlineExceeded
			}
			select {
			case <-self.ctx.Done():
				return nil, os.ErrClosed
			case <-update:
			case <-time.After(timeout):
				return nil, os.ErrDeadlineExceeded
			}
		}
	}
}

func (self *peerConn) Read(b []byte) (n int, err error) {
	var deadline time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		deadline = self.readDeadline
	}()
	var c datachannel.ReadWriteCloserDeadliner
	c, err = self.dataChannelConn(deadline)
	if err != nil {
		return
	}
	c.SetReadDeadline(deadline)
	n, err = c.Read(b)
	return
}

func (self *peerConn) Write(b []byte) (n int, err error) {
	var deadline time.Time
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		deadline = self.writeDeadline
	}()
	var c datachannel.ReadWriteCloserDeadliner
	c, err = self.dataChannelConn(deadline)
	if err != nil {
		return
	}
	c.SetWriteDeadline(deadline)
	n, err = c.Write(b)
	if 0 < n {
		self.noteOutboundSctpActivity()
	}
	return
}

func (self *peerConn) noteOutboundSctpActivity() {
	if self.settings.SctpNoProgressTimeout <= 0 {
		return
	}
	self.progressWatchOnce.Do(func() {
		go HandleError(self.runSctpProgressWatchdog, self.cancel)
	})
	select {
	case self.outboundProgress <- struct{}{}:
	default:
	}
}

// runSctpProgressWatchdog closes a half-open data plane that ICE consent
// cannot see. Pion's reliable SCTP association retries indefinitely; if the
// remote SCTP endpoint disappears while its ICE agent/socket still answers,
// PeerConnection remains "connected" and a small first write after an idle
// period can otherwise sit unacknowledged forever.
//
// The worker starts only after the first successful application write. It has
// no idle ticker: while the SCTP buffered amount is zero it waits on the
// coalescing outbound-activity channel. With data outstanding, any reverse
// SCTP packet (normally a SACK) refreshes the deadline. This observes
// transport progress without putting a timeout around intentional transfer
// callback backpressure.
func (self *peerConn) runSctpProgressWatchdog() {
	timeout := self.settings.SctpNoProgressTimeout
	sampleInterval := min(250*time.Millisecond, timeout/4)
	if sampleInterval <= 0 {
		sampleInterval = time.Millisecond
	}
	var sampleTimer *time.Timer
	defer func() {
		if sampleTimer != nil {
			sampleTimer.Stop()
		}
	}()

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.outboundProgress:
		}

		bufferedAmount, lastReceived, ok := webRtcSctpProgress(self.pc)
		if !ok {
			continue
		}
		lastProgress := time.Now()
		for 0 < bufferedAmount {
			remaining := timeout - time.Since(lastProgress)
			if remaining <= 0 {
				self.log.Infof(
					"[peerconn]SCTP no progress for %s with %d bytes buffered; reconnecting\n",
					timeout,
					bufferedAmount,
				)
				self.cancel()
				return
			}

			sample := min(sampleInterval, remaining)
			sampleC := resetOrCreateTimer(&sampleTimer, sample)
			select {
			case <-self.ctx.Done():
				return
			case <-sampleC:
			}

			var received uint64
			bufferedAmount, received, ok = webRtcSctpProgress(self.pc)
			if !ok {
				break
			}
			if received != lastReceived {
				lastReceived = received
				lastProgress = time.Now()
			}
		}
	}
}

// LocalAddr returns the local network address, if known.
func (self *peerConn) LocalAddr() net.Addr {
	sctp := self.pc.SCTP()
	if sctp == nil {
		return newWebRtcAddr("")
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return newWebRtcAddr("")
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return newWebRtcAddr("")
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return newWebRtcAddr("")
	}
	return newWebRtcAddr(pair.Local.Address)
}

// RemoteAddr returns the remote network address, if known.
func (self *peerConn) RemoteAddr() net.Addr {
	sctp := self.pc.SCTP()
	if sctp == nil {
		return newWebRtcAddr("")
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return newWebRtcAddr("")
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return newWebRtcAddr("")
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return newWebRtcAddr("")
	}
	return newWebRtcAddr(pair.Remote.Address)
}

func (self *peerConn) SetDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t
	self.writeDeadline = t

	return nil
}

func (self *peerConn) SetReadDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.readDeadline = t

	return nil
}

func (self *peerConn) SetWriteDeadline(t time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.writeDeadline = t

	return nil
}

func (self *peerConn) Close() error {
	self.cancel()
	return nil
}

func (self *peerConn) Cancel() {
	self.cancel()
}

// conforms to `net.Addr`
type webRtcAddr struct {
	addr string
}

func newWebRtcAddr(addr string) net.Addr {
	return &webRtcAddr{
		addr: addr,
	}
}

func (self *webRtcAddr) Network() string {
	return "udp"
}

func (self *webRtcAddr) String() string {
	return self.addr
}
