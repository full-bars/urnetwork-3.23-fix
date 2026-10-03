package main

import (
	"os"
	"strings"

	"github.com/urnetwork/connect"
)

// h3Enabled reports whether the operator opted in to H3 (QUIC) with
// URNETWORK_H3. It is OFF unless the variable is on, 1, true or yes.
//
// Stage A is the direct (non-proxied) identity only: runH3 opens a UDP socket on
// the host, which is the right socket for an identity that reaches the platform
// from the host's own address and the wrong one for a proxied identity, whose
// QUIC would leave from the host instead of its proxy. A proxied identity never
// gets H3 from this switch.
func h3Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("URNETWORK_H3"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// h3AllowedFor reports whether this identity may run H3: opted in, no proxy
// involved at all.
func h3AllowedFor(proxySettings *connect.ProxySettings, isNative bool) bool {
	return h3Enabled() && isNative && proxySettings == nil
}

// platformTransportSettingsFor returns the platform transport settings for an
// identity: the engine defaults, with H3 beside H1 only when allowed.
func platformTransportSettingsFor(proxySettings *connect.ProxySettings, isNative bool) *connect.PlatformTransportSettings {
	settings := connect.DefaultPlatformTransportSettings()
	settings.EnableH3 = h3AllowedFor(proxySettings, isNative)
	return settings
}
