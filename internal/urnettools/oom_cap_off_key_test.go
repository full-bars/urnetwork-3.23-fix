package urnettools

import "testing"

// `urnet-tools set oom-cap off` must be sent to the provider as the VALUE
// "off", not as the generic clear. The clear path drops the override and
// re-applies the provider's live default for the key, which for oom_cap is
// "shadow" (decide and log, enforce nothing) rather than off. So the documented
// kill switch never actually turned the feature off, and the provider code that
// forgets a standing cap on "off" never ran.
//
// The other whitelisted keys (hot_restart, ramlogs, proxy_self_heal, metrics)
// legitimately treat "off" as a value, which is why the list exists at all.
func TestOOMCapOffIsSentAsAValueNotAClear(t *testing.T) {
	for _, key := range []string{"oom-cap", "oom_cap", "OOM-Cap"} {
		c, ok := canonicalControlKey(key)
		if !ok || c != "oom_cap" {
			t.Fatalf("canonicalControlKey(%q) = %q, %v; want oom_cap", key, c, ok)
		}
		if treatsOffAsClear(c) {
			t.Errorf("%q maps to oom_cap, which must NOT take the off-as-clear path: "+
				"clearing reverts to the shadow default, not off", key)
		}
	}

	// The keys that already sent "off" as a value before oom_cap joined them.
	// Guards against a future edit flipping one of these back to the clear path.
	for _, k := range []string{"hot_restart", "ramlogs", "proxy_self_heal", "metrics"} {
		if treatsOffAsClear(k) {
			t.Errorf("%s predates oom_cap and must keep sending off as a value", k)
		}
	}

	// Keys with no meaningful off value still take the clear path, unchanged.
	for _, k := range []string{"node_name", "profile", "gomemlimit"} {
		if !treatsOffAsClear(k) {
			t.Errorf("%s should still use the clear path for off", k)
		}
	}
}
