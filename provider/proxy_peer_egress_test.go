package main

import (
	"github.com/urnetwork/connect"
	"testing"
)

func TestIdentityPeerSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		proxy    *connect.ProxySettings
		disabled bool
	}{{"direct", nil, false}, {"proxied", &connect.ProxySettings{}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			settings := connect.DefaultClientSettings()
			applyIdentityPeerSettings(settings, tc.proxy)
			if settings.WebRtcSettings.DisablePeerConnections != tc.disabled {
				t.Fatalf("peer connections disabled = %v", settings.WebRtcSettings.DisablePeerConnections)
			}
		})
	}
}
