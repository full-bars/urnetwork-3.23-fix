//go:build !js

package connect

import (
	"context"
	"net"

	"github.com/pion/datachannel"
	"github.com/pion/transport/v4"
	"github.com/pion/transport/v4/stdnet"
	"github.com/pion/webrtc/v4"
)

type p2pUdpSocketNet struct {
	transport.Net
	socketBufferByteCount int
}

func (n *p2pUdpSocketNet) ListenUDP(network string, loc *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.Net.ListenUDP(network, loc)
	if err != nil {
		return nil, err
	}
	applyP2pUdpSocketBuffers(conn, n.socketBufferByteCount)
	return conn, nil
}

func applyP2pUdpSocketBuffers(connection any, byteCount int) {
	if byteCount <= 0 {
		return
	}
	if setter, ok := connection.(interface{ SetReadBuffer(int) error }); ok {
		_ = setter.SetReadBuffer(byteCount)
	}
	if setter, ok := connection.(interface{ SetWriteBuffer(int) error }); ok {
		_ = setter.SetWriteBuffer(byteCount)
	}
}

// createWebRtcPeerConnection builds one peer connection and returns the cancel
// func for its per-generation ICE name resolver, which the caller must call
// before PeerConnection.Close (see ice_resolve_net.go and peerConn.Run).
//
// The ctx is the peer generation's context; Pion gathers ICE server hostnames
// through the returned resolver, so a lookup parked for this generation is
// canceled when the generation retires.
func createWebRtcPeerConnection(ctx context.Context, active bool, settings *WebRtcSettings) (*webrtc.PeerConnection, context.CancelFunc, error) {
	s := webrtc.SettingEngine{}
	s.LoggerFactory = &pionLoggerFactory{log: loggerOrDefault(settings.Log)}
	s.DetachDataChannels()
	s.SetSCTPMaxReceiveBufferSize( /*16 * 1024 * 1024*/ uint32(settings.ReceiveBufferSize))
	s.SetReceiveMTU( /*16384*/ uint(settings.ReceiveMtu))
	if 0 < settings.SctpCwndCAStep {
		s.SetSCTPCwndCAStep(settings.SctpCwndCAStep)
	}
	s.SetICETimeouts(
		settings.DisconnectedTimeout,
		settings.FailedTimeout,
		settings.KeepAliveTimeout,
	)
	if 0 < settings.StunGatherTimeout {
		s.SetSTUNGatherTimeout(settings.StunGatherTimeout)
	}

	// The socket network is selected exactly as before -- the buffered UDP
	// wrapper when requested, else the standard net that Pion would have
	// created for itself -- and then wrapped in the per-generation
	// cancellation-aware resolver. Socket operations still delegate to the
	// selected network; only the context-free Resolve* calls are overridden.
	stdNet, err := stdnet.NewNet()
	if err != nil {
		return nil, nil, err
	}
	var socketNet transport.Net = stdNet
	if 0 < settings.UdpSocketBufferByteCount {
		socketNet = &p2pUdpSocketNet{
			Net:                   socketNet,
			socketBufferByteCount: int(settings.UdpSocketBufferByteCount),
		}
	}
	resolveNet, cancelIceResolve := newPeerConnectionResolveNet(
		ctx,
		socketNet,
		settings.iceResolverForTest,
		settings.StunGatherTimeout,
	)
	s.SetNet(resolveNet)

	if 0 < settings.StunGatherTimeout {
		s.SetSTUNGatherTimeout(settings.StunGatherTimeout)
	}

	if !ipv6Available() {
		s.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4,
			webrtc.NetworkTypeTCP4,
		})
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			webrtc.ICEServer{
				// per-session random subset of the pool (or the pinned override), drawn
				// once for this manager-scoped factory
				URLs: settings.selectIceServerUrls(),
			},
		},
	})
	if err != nil {
		cancelIceResolve()
		return nil, nil, err
	}
	return pc, cancelIceResolve, nil
}

func detachWithDeadline(dc *webrtc.DataChannel) (datachannel.ReadWriteCloserDeadliner, error) {
	return dc.DetachWithDeadline()
}

// webRtcSctpProgress returns the native association signals used by the lazy
// no-progress watchdog. BytesReceived counts all SCTP packets read from DTLS,
// including SACKs; BufferedAmount covers pending plus in-flight user data.
func webRtcSctpProgress(pc *webrtc.PeerConnection) (bufferedAmount int, bytesReceived uint64, ok bool) {
	if pc == nil {
		return
	}
	sctp := pc.SCTP()
	if sctp == nil {
		return
	}
	stats := sctp.Stats()
	return sctp.BufferedAmount(), stats.BytesReceived, true
}
