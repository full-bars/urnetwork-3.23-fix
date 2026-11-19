package main

import "github.com/urnetwork/connect"

func applyIdentityPeerSettings(settings *connect.ClientSettings, proxy *connect.ProxySettings) {
	settings.WebRtcSettings.DisablePeerConnections = proxy != nil
}
