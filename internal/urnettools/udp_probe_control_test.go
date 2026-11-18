package urnettools

import "testing"

func TestUDPProbeControl(t *testing.T) {
	for _, key := range []string{"udp_probe", "udp-probe"} {
		canonical, ok := canonicalControlKey(key)
		if !ok || canonical != "udp_probe" {
			t.Fatalf("%s: %s %v", key, canonical, ok)
		}
	}
	if treatsOffAsClear("udp_probe") {
		t.Fatal("off must be sent as a value")
	}
	for _, value := range []string{"on", "off", "ON"} {
		if err := validateControlValue("udp_probe", value); err != nil {
			t.Fatal(err)
		}
	}
	if validateControlValue("udp_probe", "invalid") == nil {
		t.Fatal("invalid value accepted")
	}
}
