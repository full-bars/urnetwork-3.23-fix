package urnettools

import (
	"strings"
	"testing"
)

// The set/off path used to print a restart warning and then, unconditionally,
// a status block that read as though the value were already in force. The two
// must agree: a restart-required change prints one statement about what was
// persisted and when it starts, and the status block is skipped; a change the
// provider applied live prints no notice and lets the status block report the
// value now in force. The provider's needs_restart flag is the only source of
// that distinction, so a key that regains a live path needs no change here.
func TestSmartDialerChangeOutputIsSelfConsistent(t *testing.T) {
	var b strings.Builder
	if printStatus := reportSmartDialerChange(&b, "on", controlResponse{OK: true, NeedsRestart: true}); printStatus {
		t.Fatal("a restart-required change also asked for the active status block")
	}
	out := b.String()
	for _, want := range []string{"persisted", "not yet in force", "next start"} {
		if !strings.Contains(out, want) {
			t.Errorf("restart-required output %q missing %q", out, want)
		}
	}
	// Nothing in it may claim the running provider already prefers the new
	// transport, which is what the status block says.
	if strings.Contains(out, "The provider prefers") || strings.Contains(out, "Smart dialer: on") {
		t.Errorf("restart-required output claims an active effect: %q", out)
	}

	// The live case: the provider says no restart is needed, so the caller
	// falls through to the status block, which reports the applied value.
	b.Reset()
	if printStatus := reportSmartDialerChange(&b, "on", controlResponse{OK: true}); !printStatus {
		t.Fatal("a live change suppressed the status block")
	}
	if b.Len() != 0 {
		t.Errorf("a live change printed a restart notice: %q", b.String())
	}
	printSmartDialerStatus(&b, controlResponse{OK: true, Settings: map[string]SettingInfo{"smart_dialer": {Value: "on"}}})
	if !strings.Contains(b.String(), "Smart dialer: on") {
		t.Errorf("status after a live change does not report the applied value: %q", b.String())
	}
}

// `urnet-tools set smart-dialer on|off` is the measured-cost transport
// preference's switch. The provider side accepts smart_dialer on its control
// socket, but the CLI did not know the key at all: `set smart-dialer on` was
// rejected as unknown even though the same setting worked through
// `urnet-tools smart-dialer on`. The key must resolve to the socket's
// canonical name, be validated before anything is sent or queued, and appear in
// the help. The accepted values must be exactly the provider's (on and off).
func TestSmartDialerKeyIsWiredIntoTheCLI(t *testing.T) {
	for _, in := range []string{"smart-dialer", "smart_dialer", "Smart-Dialer"} {
		c, ok := canonicalControlKey(in)
		if !ok || c != "smart_dialer" {
			t.Errorf("canonicalControlKey(%q) = %q, %v; want smart_dialer", in, c, ok)
		}
	}
	for _, v := range []string{"on", "off", "ON", "Off"} {
		if err := validateControlValue("smart_dialer", v); err != nil {
			t.Errorf("value %q rejected: %v", v, err)
		}
	}
	// The provider's validator accepts only on and off for smart_dialer; so
	// must this one, or the CLI would queue a value the provider rejects.
	for _, v := range []string{"", "maybe", "1", "true", "shadow"} {
		if err := validateControlValue("smart_dialer", v); err == nil {
			t.Errorf("value %q must be rejected", v)
		}
	}
	found := false
	for _, h := range setKeyHelps {
		if strings.Contains(h, "smart-dialer") {
			found = true
		}
	}
	if !found {
		t.Error("setKeyHelps has no smart-dialer line")
	}
}
