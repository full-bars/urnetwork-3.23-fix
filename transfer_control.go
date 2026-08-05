package connect

import (
	"context"
	"sync"

	"github.com/urnetwork/connect/protocol"
)

// control sync is a pattern to sync control messages between the server and client
// it ensures:
// - control messages are sent in order
// - only the latest message per scope is retried.
//   Create one `ControlSync` object per scope.
// - if a send fails due to ack timeout or other local error, the send is retried

type ControlSync struct {
	ctx    context.Context
	cancel context.CancelFunc

	client   *Client
	scopeTag string

	monitor *Monitor

	sendLock  sync.Mutex
	syncCount uint64
}

func NewControlSync(ctx context.Context, client *Client, scopeTag string) *ControlSync {
	cancelCtx, cancel := context.WithCancel(ctx)

	return &ControlSync{
		ctx:       cancelCtx,
		cancel:    cancel,
		client:    client,
		scopeTag:  scopeTag,
		monitor:   NewMonitor(),
		syncCount: 0,
	}
}

// Send delivers a control frame to the control destination without waiting
// for the ack: the first attempt is non-blocking, and if it cannot be queued
// the send is retried in the background until it is queued and acknowledged,
// the control or client closes, or a newer Send on this scope supersedes it
// (only the latest send per scope is retried; concurrent Sends on one
// ControlSync are serialized). ackCallback is invoked at most once, with a
// nil error, when the message is acknowledged — failures are retried rather
// than reported, and the callback does not fire when the send is abandoned
// or superseded. updateFrame, when non-nil, is called before each retry to
// supply the latest frame for the scope; a different returned frame replaces
// the one being retried. Send takes ownership of frame.MessageBytes and
// returns them to the message pool when the send concludes.
func (self *ControlSync) Send(frame *protocol.Frame, updateFrame func() *protocol.Frame, ackCallback AckFunction) {
	// 1. try to send non-blocking
	// 2. if fails, send blocking with no timeout
	// 3. keep retying on error until the handle context or client is closed

	safeAckCallback := func(err error) {
		if ackCallback != nil {
			HandleError(func() {
				ackCallback(err)
			})
		}
	}

	handleCtx, handleCancel := context.WithCancel(self.ctx)

	self.sendLock.Lock()
	defer self.sendLock.Unlock()

	self.syncCount += 1
	syncIndex := self.syncCount

	notify := self.monitor.NotifyAll()
	go HandleError(func() {
		defer handleCancel()

		for {
			select {
			case <-notify:
			case <-handleCtx.Done():
				return
			}
			// re-subscribe and read syncCount in the same locked scope. the
			// notify channel is closed by `NotifyAll`, so without the
			// re-subscribe a wake that does not exit the loop would re-select
			// the already closed channel and hot spin. today the only notifier
			// (above) always bumps syncCount first, so this loop always exits on
			// its first wake — a second notifier that did not would spin
			done := false
			func() {
				self.sendLock.Lock()
				defer self.sendLock.Unlock()
				notify = self.monitor.NotifyChannel()
				done = syncIndex != self.syncCount
			}()
			if done {
				return
			}
		}
	}, handleCancel)

	var controlSync func(*protocol.Frame)
	controlSync = func(updatedFrame *protocol.Frame) {
		defer handleCancel()

		defer func() {
			self.sendLock.Lock()
			defer self.sendLock.Unlock()
			if self.syncCount == syncIndex {
				self.client.log.V(2).Infof("[control][%d]stop sync for scope = %s\n", syncIndex, self.scopeTag)
			} else {
				self.client.log.V(2).Infof("[control][%d]replace sync for scope = %s\n", syncIndex, self.scopeTag)
			}
		}()

		for {
			self.client.log.V(2).Infof("[control][%d]start sync for scope = %s\n", syncIndex, self.scopeTag)

			done := false
			success := false
			var err error
			func() {
				self.sendLock.Lock()
				defer self.sendLock.Unlock()

				select {
				case <-handleCtx.Done():
					done = true
				default:
					done = syncIndex != self.syncCount
				}

				if done {
					return
				}

				updatedFrameCopy := &protocol.Frame{
					MessageType:  updatedFrame.MessageType,
					MessageBytes: MessagePoolShareReadOnly(updatedFrame.MessageBytes),
				}
				success, err = self.client.SendWithTimeoutDetailed(
					updatedFrameCopy,
					DestinationId(ControlId),
					func(err error) {
						if err == nil {
							safeAckCallback(nil)
							MessagePoolReturn(updatedFrame.MessageBytes)
						} else {
							go HandleError(func() {
								controlSync(updatedFrame)
							}, handleCancel)
						}
					},
					-1,
					Ctx(handleCtx),
				)
				if !success {
					MessagePoolReturn(updatedFrameCopy.MessageBytes)
				}
			}()
			if done {
				MessagePoolReturn(updatedFrame.MessageBytes)
				return
			}
			if success {
				return
			}
			if err != nil {
				// only stop when the context or client is done
				select {
				case <-handleCtx.Done():
					MessagePoolReturn(updatedFrame.MessageBytes)
					return
				case <-self.client.Done():
					MessagePoolReturn(updatedFrame.MessageBytes)
					return
				default:
				}
			}
			// else try again
			if updateFrame != nil {
				f := updateFrame()
				if f != updatedFrame {
					MessagePoolReturn(updatedFrame.MessageBytes)
					updatedFrame = f
				}
			}
		}
	}

	frameCopy := &protocol.Frame{
		MessageType:  frame.MessageType,
		MessageBytes: MessagePoolShareReadOnly(frame.MessageBytes),
	}
	success := self.client.SendWithTimeout(
		frameCopy,
		DestinationId(ControlId),
		func(err error) {
			if err == nil {
				safeAckCallback(nil)
				MessagePoolReturn(frame.MessageBytes)
			} else {
				go HandleError(func() {
					controlSync(frame)
				}, handleCancel)
			}
		},
		0,
		Ctx(handleCtx),
	)
	if success {
		return
	}
	MessagePoolReturn(frameCopy.MessageBytes)

	go HandleError(func() {
		controlSync(frame)
	}, handleCancel)
}

// Close cancels the control's context, stopping background retries and any
// send not yet queued. A message already queued in the client may still be
// acknowledged afterwards. Close is idempotent and safe to call from any
// goroutine.
func (self *ControlSync) Close() {
	self.cancel()
}
