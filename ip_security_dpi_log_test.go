package connect

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDpiDecisionCountsFlowsOnceUnderTheirReason(t *testing.T) {
	resetDpiLogForTest()
	t.Cleanup(resetDpiLogForTest)

	policy := DefaultSecurityPolicy(context.Background())
	replay := func(name string, srcPort int) {
		fixture := loadSecurityFixture(t, filepath.Join("testdata", "ipsecurity", name+".json"))
		replayFixture(t, policy, fixture, srcPort)
	}

	replay("wireguard-handshake", 47001)
	if n := dpiDecidedFlows[SecurityPolicyReasonAllowWireGuard].Load(); n != 1 {
		t.Fatalf("wireguard flows = %d, want 1", n)
	}
	// replaying the same flow reaches the already decided flow: no recount
	replay("wireguard-handshake", 47001)
	if n := dpiDecidedFlows[SecurityPolicyReasonAllowWireGuard].Load(); n != 1 {
		t.Fatalf("wireguard flows after replay = %d, want 1", n)
	}
	// a different flow of the same kind counts again
	replay("wireguard-handshake", 47002)
	if n := dpiDecidedFlows[SecurityPolicyReasonAllowWireGuard].Load(); n != 2 {
		t.Fatalf("wireguard flows after second flow = %d, want 2", n)
	}

	replay("bittorrent-tcp-51413", 47003)
	if n := dpiDecidedFlows[SecurityPolicyReasonBittorrent].Load(); n != 1 {
		t.Fatalf("bittorrent flows = %d, want 1", n)
	}

	summary := dpiSummaryString()
	for _, want := range []string{"allow-app-standard:wireguard=2", "bittorrent=1"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
}

func TestDpiDecisionIgnoresUndecidedAndOutOfRange(t *testing.T) {
	resetDpiLogForTest()
	t.Cleanup(resetDpiLogForTest)

	path := &IpPath{Protocol: IpProtocolUdp, DestinationPort: 51820}
	recordDpiDecision(path, SecurityPolicyReasonDropEncrypted, false)
	// application-standard admits are counted at admit time, never at the terminal verdict
	recordDpiDecision(path, SecurityPolicyReasonAllowWireGuard, true)
	recordDpiDecision(path, SecurityPolicyReasonUnknown, true)
	recordDpiDecision(path, securityPolicyReasonEnd, true)
	recordDpiDecision(path, SecurityPolicyReason(-3), true)
	if got := dpiSummaryString(); got != "none" {
		t.Fatalf("summary = %q, want none", got)
	}
}

func TestDpiAdmitReasonsCoverEveryAppStandard(t *testing.T) {
	for reason := SecurityPolicyReason(1); reason < securityPolicyReasonEnd; reason++ {
		if strings.HasPrefix(reason.String(), "allow-app-standard:") && !isDpiAdmitReason(reason) {
			t.Fatalf("%s is an application standard but not logged as an admit", reason)
		}
	}
}
