package main

// proxy_target_family.go — the provider's side of the proxy_target_family
// setting: which resolver names a proxied dial's destination. The modes and
// what they do live in connect/proxy_target_family.go.
//
// The key is deliberately live (nothing is relaunched): the mode is read inside
// every proxied dial, so the next dial follows it and connections already open
// keep the address they were connected to. That is what makes a canary safe:
// flip one box, watch it, flip it back, and no session is dropped either way.

import (
	"os"
	"strings"

	"github.com/urnetwork/connect"
)

// proxyTargetFamilyDefault is the value a cleared key and an unset
// URNETWORK_PROXY_TARGET_FAMILY both mean. v4 is the behaviour every release
// before this setting had, so an upgrade changes nothing until an operator asks
// for proxy mode.
const proxyTargetFamilyDefault = "v4"

// applyProxyTargetFamily parses and applies the setting, logging the change so
// an operator can see the running value in the provider log rather than
// inferring it from the CLI's success line.
func applyProxyTargetFamily(value string) error {
	mode, err := connect.ParseProxyTargetFamily(value)
	if err != nil {
		return err
	}
	previous := connect.SetProxyTargetFamily(mode)
	if previous != mode {
		tlog("⚙️ [control] applied proxy_target_family=%s (was %s)\n", mode, previous)
	}
	return nil
}

// resolveProxyTargetFamily is the value in force at startup: the persisted
// control key first, then URNETWORK_PROXY_TARGET_FAMILY, then the default v4.
// The persisted key wins for the same reason it does everywhere else here: an
// operator who set it over the socket meant it to survive a restart, and the
// environment is what a unit file carries from install time.
func resolveProxyTargetFamily(state *controlState) string {
	if v, ok := state.get("proxy_target_family"); ok {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	if v := strings.TrimSpace(os.Getenv("URNETWORK_PROXY_TARGET_FAMILY")); v != "" {
		return v
	}
	return proxyTargetFamilyDefault
}
