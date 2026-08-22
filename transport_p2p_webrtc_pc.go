//go:build !js

package connect

import (
	"context"

	"github.com/pion/datachannel"
	"github.com/pion/webrtc/v4"
)

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

	if !ipv6Available() {
		s.SetNetworkTypes([]webrtc.NetworkType{
			webrtc.NetworkTypeUDP4,
			webrtc.NetworkTypeTCP4,
		})
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	return api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			webrtc.ICEServer{
				URLs: settings.IceServerUrls,
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
