package connect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect/protocol"
)

func DefaultStreamManagerSettings() *StreamManagerSettings {
	return &StreamManagerSettings{
		StreamBufferSettings: DefaultStreamBufferSettings(),
		// WebRtcSettings:       DefaultWebRtcSettings(),
	}
}

func DefaultStreamBufferSettings() *StreamBufferSettings {
	return &StreamBufferSettings{
		ReadTimeout:          time.Duration(-1),
		WriteTimeout:         time.Duration(-1),
		P2pTransportSettings: DefaultP2pTransportSettings(),
	}
}

type StreamManagerSettings struct {
	StreamBufferSettings *StreamBufferSettings
}

type StreamManager struct {
	ctx context.Context

	client *Client

	webRtcManager *WebRtcManager

	streamBuffer *StreamBuffer

	streamManagerSettings *StreamManagerSettings
}

func NewStreamManager(ctx context.Context, client *Client, webRtcManager *WebRtcManager, streamManagerSettings *StreamManagerSettings) *StreamManager {
	streamManager := &StreamManager{
		ctx:                   ctx,
		client:                client,
		streamManagerSettings: streamManagerSettings,
	}

	// webRtcManager := NewWebRtcManager(ctx, streamManagerSettings.WebRtcSettings)

	streamManager.initBuffers(webRtcManager)

	return streamManager
}

// initBuffers stores the WebRTC manager and creates the stream buffer;
// called once by NewStreamManager before the manager is used.
func (self *StreamManager) initBuffers(webRtcManager *WebRtcManager) {
	self.webRtcManager = webRtcManager
	self.streamBuffer = NewStreamBuffer(self.ctx, self, self.streamManagerSettings.StreamBufferSettings)
}

// Client returns the client the stream manager was created with; stream
// sequences use it for their P2P transports and logging.
func (self *StreamManager) Client() *Client {
	return self.client
}

// WebRtcManager returns the WebRTC manager the stream manager was created
// with (nil when constructed with none); stream P2P transports use it for
// signaling and peer connections.
func (self *StreamManager) WebRtcManager() *WebRtcManager {
	return self.webRtcManager
}

// ReceiveFunction
func (self *StreamManager) Receive(source TransferPath, frames []*protocol.Frame, peer Peer) {
	if source.IsControlSource() {
		for _, frame := range frames {
			// ignore error
			self.handleControlFrame(frame)
		}
	}
}

// handleControlFrame applies a stream control frame: TransferStreamOpen
// opens the stream, TransferStreamClose closes it, and TransferStreamReset
// cancels all streams and reopens the streams listed in the message. Other
// message types are ignored. It returns a decode error, or nil.
func (self *StreamManager) handleControlFrame(frame *protocol.Frame) error {
	switch frame.MessageType {
	case protocol.MessageType_TransferStreamOpen, protocol.MessageType_TransferStreamClose, protocol.MessageType_TransferStreamReset:
		if message, err := FromFrame(frame); err == nil {

			streamOpen := func(v *protocol.StreamOpen) error {
				var sourceId *Id
				if v.SourceId != nil {
					sourceId_, err := IdFromBytes(v.SourceId)
					if err != nil {
						return err
					}
					sourceId = &sourceId_
				}

				var destinationId *Id
				if v.DestinationId != nil {
					destinationId_, err := IdFromBytes(v.DestinationId)
					if err != nil {
						return err
					}
					destinationId = &destinationId_
				}

				streamId, err := IdFromBytes(v.StreamId)
				if err != nil {
					return err
				}

				self.streamBuffer.OpenStream(sourceId, destinationId, streamId)
				return nil
			}

			switch v := message.(type) {
			case *protocol.StreamOpen:
				err := streamOpen(v)
				if err != nil {
					return err
				}

			case *protocol.StreamClose:
				streamId, err := IdFromBytes(v.StreamId)
				if err != nil {
					return err
				}

				self.streamBuffer.CloseStream(streamId)

			case *protocol.StreamReset:
				self.streamBuffer.ResetStreams()
				for _, m := range v.Streams {
					err := streamOpen(m)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// IsStreamOpen reports whether the given stream id is open in the stream
// buffer.
func (self *StreamManager) IsStreamOpen(streamId Id) bool {
	return self.streamBuffer.IsStreamOpen(streamId)
}

type StreamBufferSettings struct {
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	P2pTransportSettings *P2pTransportSettings
}

type streamSequenceId struct {
	SourceId       Id
	HasSource      bool
	DestinationId  Id
	HasDestination bool
	StreamId       Id
}

func newStreamSequenceId(sourceId *Id, destinationId *Id, streamId Id) streamSequenceId {
	streamSequenceId := streamSequenceId{
		StreamId: streamId,
	}
	if sourceId != nil {
		streamSequenceId.SourceId = *sourceId
		streamSequenceId.HasSource = true
	}
	if destinationId != nil {
		streamSequenceId.DestinationId = *destinationId
		streamSequenceId.HasDestination = true
	}
	return streamSequenceId
}

type StreamBuffer struct {
	ctx context.Context

	streamManager *StreamManager

	streamBufferSettings *StreamBufferSettings

	mutex                     sync.Mutex
	streamSequences           map[streamSequenceId]*StreamSequence
	streamSequencesByStreamId map[Id]*StreamSequence
}

func NewStreamBuffer(ctx context.Context, streamManager *StreamManager, streamBufferSettings *StreamBufferSettings) *StreamBuffer {
	return &StreamBuffer{
		ctx:                       ctx,
		streamManager:             streamManager,
		streamBufferSettings:      streamBufferSettings,
		streamSequences:           map[streamSequenceId]*StreamSequence{},
		streamSequencesByStreamId: map[Id]*StreamSequence{},
	}
}

// ResetStreams cancels every registered stream sequence under the buffer
// lock, which stops their run loops and P2P transports; each sequence is
// unregistered by its own cleanup as it shuts down.
func (self *StreamBuffer) ResetStreams() {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, streamSequence := range self.streamSequences {
		streamSequence.Cancel()
	}
}

// OpenStream creates (or reuses) a stream sequence for the given
// source/destination/stream ids, starts its run loop, and opens it, retrying
// once with a fresh sequence if the first open fails. It returns whether the
// stream ended up open, or the error. An existing sequence for the same
// (source, destination, stream) tuple is reused and re-opened; a sequence
// registered under the same stream id for a different tuple is cancelled and
// replaced. It returns (false, error) when the buffer context is done.
func (self *StreamBuffer) OpenStream(sourceId *Id, destinationId *Id, streamId Id) (bool, error) {
	streamSequenceId := newStreamSequenceId(sourceId, destinationId, streamId)

	initStreamSequence := func(skip *StreamSequence) *StreamSequence {
		self.mutex.Lock()
		defer self.mutex.Unlock()

		streamSequence, ok := self.streamSequences[streamSequenceId]
		if ok {
			if skip == nil || skip != streamSequence {
				return streamSequence
			} else {
				streamSequence.Cancel()
				delete(self.streamSequences, streamSequenceId)
			}
		}

		if streamSequenceByStreamId, ok := self.streamSequencesByStreamId[streamId]; ok {
			streamSequenceByStreamId.Cancel()
			delete(self.streamSequencesByStreamId, streamId)
		}

		streamSequence = NewStreamSequence(self.ctx, self.streamManager, sourceId, destinationId, streamId, self.streamBufferSettings)

		self.streamSequences[streamSequenceId] = streamSequence
		self.streamSequencesByStreamId[streamId] = streamSequence
		go HandleError(func() {
			defer func() {
				self.mutex.Lock()
				defer self.mutex.Unlock()
				streamSequence.Close()
				// clean up
				if streamSequence == self.streamSequences[streamSequenceId] {
					delete(self.streamSequences, streamSequenceId)
				}
				if streamSequence == self.streamSequencesByStreamId[streamId] {
					delete(self.streamSequencesByStreamId, streamId)
				}
			}()
			streamSequence.Run()
		})
		return streamSequence
	}

	var streamSequence *StreamSequence
	var success bool
	var err error
	for i := 0; i < 2; i += 1 {
		select {
		case <-self.ctx.Done():
			return false, errors.New("Done.")
		default:
		}
		streamSequence = initStreamSequence(streamSequence)
		if success, err = streamSequence.Open(); err == nil {
			return success, nil
		}
		// sequence closed
	}
	return success, err
}

// CloseStream cancels the sequence registered for the stream id, if any; it
// is a no-op when the stream is not open. The sequence's map entries are
// removed by its cleanup once its run loop exits.
func (self *StreamBuffer) CloseStream(streamId Id) {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if streamSequence, ok := self.streamSequencesByStreamId[streamId]; ok {
		streamSequence.Cancel()
	}
}

// IsStreamOpen reports whether a stream sequence is currently registered for
// the stream id. A sequence that is shutting down still counts as open until
// its cleanup unregisters it.
func (self *StreamBuffer) IsStreamOpen(streamId Id) bool {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	_, ok := self.streamSequencesByStreamId[streamId]
	return ok
}

type StreamSequence struct {
	ctx    context.Context
	cancel context.CancelFunc

	streamManager *StreamManager

	streamBufferSettings *StreamBufferSettings

	sourceId      *Id
	destinationId *Id
	streamId      Id

	idleCondition *IdleCondition
}

func NewStreamSequence(
	ctx context.Context,
	streamManager *StreamManager,
	sourceId *Id,
	destinationId *Id,
	streamId Id,
	streamBufferSettings *StreamBufferSettings) *StreamSequence {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &StreamSequence{
		ctx:                  cancelCtx,
		cancel:               cancel,
		streamManager:        streamManager,
		streamBufferSettings: streamBufferSettings,
		sourceId:             sourceId,
		destinationId:        destinationId,
		streamId:             streamId,
		idleCondition:        NewIdleCondition(),
	}
}

// Open marks the sequence open: it increments the sequence's open count
// (decremented when the caller is done), so the run loop's idle timeout
// keeps the sequence running. It returns (true, nil) unless the sequence
// context is cancelled or the idle condition is already closed, in which
// case it returns (false, error).
func (self *StreamSequence) Open() (bool, error) {
	select {
	case <-self.ctx.Done():
		return false, errors.New("Done.")
	default:
	}

	if !self.idleCondition.UpdateOpen() {
		return false, errors.New("Done.")
	}
	defer self.idleCondition.UpdateClose()

	return true, nil
}

// Run runs the stream sequence: it starts P2P transports for the stream (to
// the source and/or destination peer, relaying traffic between the two when
// both ends are present) and then blocks until the sequence context is
// cancelled, which also stops the transports. It cancels the context when it
// returns.
func (self *StreamSequence) Run() {
	defer self.cancel()

	if self.sourceId == nil || self.destinationId == nil {
		clientRouteManager := self.streamManager.Client().RouteManager()

		if self.sourceId != nil {
			NewP2pTransport(
				self.ctx,
				self.streamManager.Client(),
				self.streamManager.WebRtcManager(),
				clientRouteManager,
				clientRouteManager,
				*self.sourceId,
				self.streamId,
				PeerTypeSource,
				self.streamBufferSettings.P2pTransportSettings,
			)
		} else if self.destinationId != nil {
			NewP2pTransport(
				self.ctx,
				self.streamManager.Client(),
				self.streamManager.WebRtcManager(),
				clientRouteManager,
				clientRouteManager,
				*self.destinationId,
				self.streamId,
				PeerTypeDestination,
				self.streamBufferSettings.P2pTransportSettings,
			)
		} else {
			// the stream must have one of source or destination
			self.streamManager.Client().log.V(1).Infof("[sm] s(%s) missing source or destination.\n", self.streamId)
			return
		}
	} else {
		p2pToDestinationRouteManager := NewRouteManager(self.ctx, fmt.Sprintf("->s(%s)", self.streamId))
		p2pToSourceRouteManager := NewRouteManager(self.ctx, fmt.Sprintf("<-s(%s)", self.streamId))

		// to destination
		NewP2pTransport(
			self.ctx,
			self.streamManager.Client(),
			self.streamManager.WebRtcManager(),
			p2pToDestinationRouteManager,
			p2pToSourceRouteManager,
			*self.destinationId,
			self.streamId,
			PeerTypeDestination,
			self.streamBufferSettings.P2pTransportSettings,
		)
		// to source
		NewP2pTransport(
			self.ctx,
			self.streamManager.Client(),
			self.streamManager.WebRtcManager(),
			p2pToSourceRouteManager,
			p2pToDestinationRouteManager,
			*self.sourceId,
			self.streamId,
			PeerTypeSource,
			self.streamBufferSettings.P2pTransportSettings,
		)
		self.streamManager.Client().log.Infof("[sm]s(%s) p2p transports created: to-dest, to-source\n", self.streamId)

		forward := func(routeManager *RouteManager) {
			defer self.cancel()

			mrr := routeManager.OpenMultiRouteReader(TransferPath{
				StreamId: self.streamId,
			})
			defer routeManager.CloseMultiRouteReader(mrr)
			mrw := routeManager.OpenMultiRouteWriter(TransferPath{
				StreamId: self.streamId,
			})
			defer routeManager.CloseMultiRouteWriter(mrw)

			for {
				checkpointId := self.idleCondition.Checkpoint()
				transferFrameBytes, err := mrr.Read(self.ctx, self.streamBufferSettings.ReadTimeout)
				if err != nil {
					return
				}
				if transferFrameBytes == nil {
					// idle timeout
					if self.idleCondition.Close(checkpointId) {
						// close the sequence
						return
					}
					// else the sequence was opened again
					continue
				}
				success, err := mrw.WriteDetailed(self.ctx, transferFrameBytes, self.streamBufferSettings.WriteTimeout)
				if err != nil {
					return
				}
				if !success {
					// drop it — WriteDetailed already returned the buffer
				}
			}
		}

		go HandleError(func() {
			forward(p2pToDestinationRouteManager)
		}, self.cancel)
		go HandleError(func() {
			forward(p2pToSourceRouteManager)
		}, self.cancel)
	}

	select {
	case <-self.ctx.Done():
		return
	}
}

// Cancel cancels the sequence context, stopping Run and the stream's P2P
// transports. It is idempotent and safe to call from any goroutine.
func (self *StreamSequence) Cancel() {
	self.cancel()
}

// Close is equivalent to Cancel: it cancels the sequence context, stopping
// the run loop. The stream buffer's cleanup calls it once Run exits.
func (self *StreamSequence) Close() {
	self.cancel()
}
