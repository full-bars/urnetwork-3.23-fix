//go:build !darwin && !ios && !linux && !android

package connect

import "errors"

// The shared socket lifecycle is not implemented for this platform. The
// buffer keeps the established portable per-flow reader.
func newUdpSocketPollBackend() (udpSocketPollBackend, error) {
	return nil, errors.New("udp socket readiness poller unsupported")
}
