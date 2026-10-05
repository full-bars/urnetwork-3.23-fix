package main

import (
	"os"
	"strings"

	"github.com/urnetwork/connect"
)

// h3Enabled reports whether the operator opted in to H3 (QUIC) with
// URNETWORK_H3. It is OFF unless the variable is on, 1, true or yes. This is
// only the startup default for the runtime gate: the h3 control key overrides it
// and can be changed without a restart (see resolveH3).
func h3Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("URNETWORK_H3"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

// resolveH3 is whether H3 should be running: the h3 control key when it has
// been set, otherwise URNETWORK_H3.
func resolveH3(state *controlState) bool {
	if state != nil {
		if v, ok := state.get("h3"); ok {
			return strings.EqualFold(v, "on")
		}
	}
	return h3Enabled()
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// h3Eligible reports whether this identity may ever run H3: no proxy involved
// at all.
//
// Stage A is the direct (non-proxied) identity only: runH3 opens a UDP socket on
// the host, which is the right socket for an identity that reaches the platform
// from the host's own address and the wrong one for a proxied identity, whose
// QUIC would leave from the host instead of its proxy. A proxied identity never
// gets H3 from the switch. Eligible is not running: the runtime gate
// (connect.SetH3Enabled, driven by the h3 control key or URNETWORK_H3) decides
// whether an eligible identity actually dials.
func h3Eligible(proxySettings *connect.ProxySettings, isNative bool) bool {
	return isNative && proxySettings == nil
}

// platformTransportSettingsFor returns the platform transport settings for an
// identity: the engine defaults, with H3 eligibility only for a direct identity.
func platformTransportSettingsFor(proxySettings *connect.ProxySettings, isNative bool) *connect.PlatformTransportSettings {
	settings := connect.DefaultPlatformTransportSettings()
	settings.EnableH3 = h3Eligible(proxySettings, isNative)
	return settings
}
