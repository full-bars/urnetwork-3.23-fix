package urnettools

import (
	"strings"
	"testing"
)

// `urnet-tools set oom-cap on|off|shadow` is the OOM-aware start cap's kill
// switch. It must resolve to the socket's canonical key, be validated before
// anything is sent or queued, and appear in the help.
func TestOOMCapKeyIsWiredIntoTheCLI(t *testing.T) {
	for _, in := range []string{"oom-cap", "oom_cap", "OOM-Cap"} {
		c, ok := canonicalControlKey(in)
		if !ok || c != "oom_cap" {
			t.Errorf("canonicalControlKey(%q) = %q, %v; want oom_cap", in, c, ok)
		}
	}
	for _, v := range []string{"on", "off", "shadow", "ON"} {
		if err := validateControlValue("oom_cap", v); err != nil {
			t.Errorf("value %q rejected: %v", v, err)
		}
	}
	for _, v := range []string{"", "maybe", "1", "enforce"} {
		if err := validateControlValue("oom_cap", v); err == nil {
			t.Errorf("value %q must be rejected", v)
		}
	}
	found := false
	for _, h := range setKeyHelps {
		if strings.Contains(h, "oom-cap") {
			found = true
		}
	}
	if !found {
		t.Error("setKeyHelps has no oom-cap line")
	}
}
