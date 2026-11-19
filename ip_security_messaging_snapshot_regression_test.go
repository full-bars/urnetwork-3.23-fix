package connect

import "testing"

func TestMetaMessagingSnapshotNewRanges(t *testing.T) {
	settings := DefaultMessagingSecurityPolicySettings()
	for _, address := range []string{"57.141.26.0", "57.141.31.255", "57.141.32.0", "57.141.33.255"} {
		if !isSanctionedMessagingEndpoint(settings, whatsAppTestPath(address, IpProtocolTcp, 5222, false)) {
			t.Errorf("messaging endpoint %s did not match", address)
		}
	}
	for _, address := range []string{"57.141.34.0", "57.141.35.255"} {
		if isSanctionedMessagingEndpoint(settings, whatsAppTestPath(address, IpProtocolTcp, 5222, false)) {
			t.Errorf("outside address %s matched", address)
		}
	}
}
