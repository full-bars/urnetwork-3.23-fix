package connect

import "errors"

// ErrExtenderPacketCarrierUnavailable indicates that no proxy packet path exists.
var ErrExtenderPacketCarrierUnavailable = errors.New("proxy packet carrier unavailable")

// The udp of a space whose egress is a proxy.
//
// A socks proxy carries streams only. Every udp socket a space opens is this
// host's own: a quic or dns extender carrier, an h3 platform transport, the ice
// of a webrtc carrier. Each would send the space's traffic from this host's
// address, outside the proxy the space is configured with. So a space whose
// egress is a proxy fails closed for udp, and its tcp carriers keep dialing
// through the proxy:
//   - The quic and dns extender carriers dial only through an endpoint the
//     owner supplied (DialContextSettings.PacketConnFactory). They dial an ip
//     literal, so nothing is looked up.
//   - The h3 platform transport modes are off, endpoint or not: each resolves
//     a platform name on this host before it dials, which is a lookup outside
//     the proxy.
//   - The webrtc carrier is off: ice gathers this host's own candidates.
//
// A space whose egress is a dialer of its own with no proxy keeps that type's
// contract: a nil PacketConnFactory is the platform's ordinary socket.
//
// The carriers are left out where they are chosen, and the sockets refuse too
// (openExtenderPacketConn, openH3PacketConn), whoever reaches them.

// Whether the quic and dns extender carriers of these settings have a path: no
// proxy, or an endpoint the owner supplied.
func connectSettingsHavePacketPath(connectSettings *ConnectSettings) bool {
	if connectSettings == nil || connectSettings.ProxySettings == nil {
		return true
	}
	return connectSettings.DialContextSettings != nil && connectSettings.DialContextSettings.PacketConnFactory != nil
}

// Whether a carrier of this connect mode can be dialed under these settings.
func extenderConnectModeDialable(connectSettings *ConnectSettings, connectMode ExtenderConnectMode) bool {
	switch connectMode {
	case ExtenderConnectModeQuic, ExtenderConnectModeDns:
		return connectSettingsHavePacketPath(connectSettings)
	case ExtenderConnectModeWebRtc:
		// ice opens its own sockets, whatever endpoint the owner supplied
		return connectSettings == nil || connectSettings.ProxySettings == nil
	default:
		return true
	}
}

// The configs of `extenderConfigs` these settings can dial, in order. A carrier
// the space's egress cannot carry is left out.
func dialableExtenderConfigs(connectSettings *ConnectSettings, extenderConfigs []*ExtenderConfig) []*ExtenderConfig {
	dialableConfigs := make([]*ExtenderConfig, 0, len(extenderConfigs))
	for _, extenderConfig := range extenderConfigs {
		if extenderConfig != nil && extenderConnectModeDialable(connectSettings, extenderConfig.Profile.ConnectMode) {
			dialableConfigs = append(dialableConfigs, extenderConfig)
		}
	}
	return dialableConfigs
}
