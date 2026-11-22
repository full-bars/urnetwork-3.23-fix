package urnettools

import (
	"strings"
	"testing"
)

func TestProxyIPv6ProbeControl(t *testing.T) {
	for _, key := range []string{"proxy_ipv6_probe", "proxy-ipv6-probe"} {
		canonical, ok := canonicalControlKey(key)
		if !ok || canonical != "proxy_ipv6_probe" {
			t.Fatalf("%s: canonical=%q ok=%v", key, canonical, ok)
		}
	}
	if treatsOffAsClear("proxy_ipv6_probe") {
		t.Fatal("proxy_ipv6_probe: off must be sent as an explicit value, not cleared")
	}
	for _, value := range []string{"on", "off", "ON", "OFF"} {
		if err := validateControlValue("proxy_ipv6_probe", value); err != nil {
			t.Fatalf("valid value %q rejected: %v", value, err)
		}
	}
	if validateControlValue("proxy_ipv6_probe", "invalid") == nil {
		t.Fatal("invalid value accepted")
	}
	found := false
	for _, l := range setKeyHelps {
		if strings.Contains(l, "proxy-ipv6-probe") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("proxy-ipv6-probe missing from setKeyHelps")
	}
}
