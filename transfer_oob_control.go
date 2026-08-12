package connect

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"encoding/base64"

	// "google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
)

// control messages for a client out of band with the client sequence
// some control messages require blocking response, but there is a potential deadlock
// when a send blocks to wait for a control receive, or vice versa, since
// all clients messages are multiplexed in the same client sequence
// and the receive/send may be blocked on the send/receive
// for example think of a remote provider setup forwarding traffic as fast as possible
// to an "echo" server with a finite buffer

type OobResultFunction = func(resultFrames []*protocol.Frame, err error)

type OutOfBandControl interface {
	SendControl(frames []*protocol.Frame, callback OobResultFunction)
}

type ApiOutOfBandControl struct {
	api *BringYourApi
	// audit401Count counts 401 responses observed by SendControl since the
	// last reset. The provider's client-JWT renewal watcher uses it as an
	// immediate-renewal trigger: a 401 on the OOB path means the bearer
	// client JWT was rejected (expired or revoked), and waiting up to an
	// hour for the next scheduled renewal keeps the proxy a black hole.
	audit401Count atomic.Uint64
	// on401, when set, is invoked (non-blocking) whenever SendControl
	// observes a 401. The renewal watcher registers a callback that pings its
	// renew-now channel, so a 401 triggers immediate renewal instead of
	// waiting for the next hourly tick.
	on401 atomic.Value // func()
}

// isUnauthorizedError reports whether err represents an HTTP 401 rejection.
// The transport builds these as fmt.Errorf("%s: %s", res.Status, body) where
// res.Status is the canonical Go status line "401 Unauthorized". We match
// that exact phrase — never the bare "401" (which appears in unrelated
// bodies like "502 Bad Gateway: upstream returned 401 for port 401" and
// would trigger a false renewal storm) and never the bare "Unauthorized"
// (which appears in 400-level bodies like "unauthorized access attempt").
func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "401 Unauthorized")
}

// SetOn401 registers a callback invoked on every 401 observed by SendControl.
// The callback must not block; it runs synchronously on the SendControl
// callback goroutine.
func (self *ApiOutOfBandControl) SetOn401(fn func()) {
	if fn == nil {
		self.on401.Store((func())(nil))
		return
	}
	self.on401.Store(fn)
}

func (self *ApiOutOfBandControl) fireOn401() {
	if fn, ok := self.on401.Load().(func()); ok && fn != nil {
		fn()
	}
}

func NewApiOutOfBandControl(
	ctx context.Context,
	clientStrategy *ClientStrategy,
	byJwt string,
	apiUrl string,
) *ApiOutOfBandControl {
	api := NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)
	return &ApiOutOfBandControl{
		api: api,
	}
}

func NewApiOutOfBandControlWithApi(api *BringYourApi) *ApiOutOfBandControl {
	return &ApiOutOfBandControl{
		api: api,
	}
}

// SetByJwt updates the bearer token used by future out-of-band control
// requests. Long-lived clients call this when their renewable client JWT is
// rotated; BringYourApi provides the synchronization for concurrent sends.
func (self *ApiOutOfBandControl) SetByJwt(byJwt string) {
	self.api.SetByJwt(byJwt)
}

// Audit401Count returns how many 401 responses SendControl has observed since
// the last reset.
func (self *ApiOutOfBandControl) Audit401Count() uint64 {
	return self.audit401Count.Load()
}

// ResetAudit401Count zeroes the 401 counter.
func (self *ApiOutOfBandControl) ResetAudit401Count() {
	self.audit401Count.Store(0)
}

// SendControl consumes the caller's frames: their MessageBytes are returned
// to the message pool when SendControl returns. On the success path that is
// before the async callback executes; on the ProtoMarshal error path the
// callback is dispatched first (the pool return is deferred until return),
// so a callback must not retain frame bytes across the call.
func (self *ApiOutOfBandControl) SendControl(
	frames []*protocol.Frame,
	callback OobResultFunction,
) {
	safeCallback := func(resultFrames []*protocol.Frame, err error) {
		if callback != nil {
			HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}

	pack := &protocol.Pack{
		Frames: frames,
	}
	defer func() {
		for _, frame := range frames {
			MessagePoolReturn(frame.MessageBytes)
		}
	}()
	packBytes, err := ProtoMarshal(pack)
	if err != nil {
		safeCallback(nil, err)
		return
	}
	defer MessagePoolReturn(packBytes)

	self.api.ConnectControl(
		&ConnectControlArgs{
			Pack: EncodeBase64(base64.StdEncoding, packBytes),
		},
		NewApiCallback(func(result *ConnectControlResult, err error) {
			if err != nil {
				if isUnauthorizedError(err) {
					self.audit401Count.Add(1)
					self.fireOn401()
				}
				safeCallback(nil, err)
				return
			}

			packBytes, err := DecodeBase64(base64.StdEncoding, result.Pack)
			if err != nil {
				safeCallback(nil, err)
				return
			}
			defer MessagePoolReturn(packBytes)

			responsePack := &protocol.Pack{}
			err = ProtoUnmarshal(packBytes, responsePack)
			if err != nil {
				safeCallback(nil, err)
				return
			}

			safeCallback(responsePack.Frames, nil)
		}),
	)
}

type NoContractClientOob struct {
}

func NewNoContractClientOob() *NoContractClientOob {
	return &NoContractClientOob{}
}

func (self *NoContractClientOob) SendControl(frames []*protocol.Frame, callback func(resultFrames []*protocol.Frame, err error)) {
	safeCallback := func(resultFrames []*protocol.Frame, err error) {
		if callback != nil {
			HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}

	safeCallback(nil, errors.New("Not supported."))
}
