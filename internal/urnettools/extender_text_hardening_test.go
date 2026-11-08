package urnettools

import (
	"strings"
	"testing"
)

func TestExtenderHelpDocumentsManualDNSAndFreeBSD(t *testing.T) {
	for name, text := range map[string]string{"help": newExtenderCmd().Long, "about": extenderAboutText} {
		for _, want := range []string{"UDP 53", "user-managed", "FreeBSD", "untested"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
	for _, want := range []string{"53:53/udp", "Compose", "IPv6", "hot-swap", "log follower"} {
		if !strings.Contains(extenderAboutText, want) {
			t.Errorf("about missing %q", want)
		}
	}
}
