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

func createWebRtcPeerConnection(ctx context.Context, active bool, settings *WebRtcSettings) (*webrtc.PeerConnection, error) {
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

	if 0 < settings.UdpSocketBufferByteCount {
		if stdNet, err := stdnet.NewNet(); err == nil {
			s.SetNet(&p2pUdpSocketNet{
				Net:                   stdNet,
				socketBufferByteCount: int(settings.UdpSocketBufferByteCount),
			})
		}
	}

	if !ipv6Available() {
		s.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4,
			webrtc.NetworkTypeTCP4,
		})
	}

	// Filter out STUN servers that have failed recently.
	healthyURLs := filterSTUNURLs(settings.IceServerUrls)
	if len(healthyURLs) < len(settings.IceServerUrls) {
		loggerOrDefault(settings.Log).V(2).Infof("[stun-cache] filtered %d/%d STUN URLs (healthy=%v)",
			len(settings.IceServerUrls)-len(healthyURLs), len(settings.IceServerUrls), healthyURLs)
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			webrtc.ICEServer{
				URLs: healthyURLs,
			},
		},
	})
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
