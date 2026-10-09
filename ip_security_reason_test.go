package connect

import (
	"testing"
)

func TestSecurityPolicyReasonString(t *testing.T) {
	cases := []struct {
		reason SecurityPolicyReason
		want   string
	}{
		{SecurityPolicyReasonUnknown, "unknown"},
		{SecurityPolicyReasonNetwork, "network"},
		{SecurityPolicyReasonNotPublic, "not-public"},
		{SecurityPolicyReasonCfaaDropIp, "cfaa-drop-ip"},
		{SecurityPolicyReasonCfaaDropPort, "cfaa-drop-port"},
		{SecurityPolicyReasonCfaaAllow, "cfaa-allow"},
		{SecurityPolicyReasonBittorrent, "bittorrent"},
		{SecurityPolicyReasonDropEncrypted, "drop-encrypted"},
		{SecurityPolicyReasonInspecting, "inspecting"},
		{SecurityPolicyReasonAllowPrivileged, "allow-privileged"},
		{SecurityPolicyReasonAllowGaming, "allow-gaming"},
		{SecurityPolicyReasonAllowTls, "allow-web-standard:tls"},
		{SecurityPolicyReasonAllowDtls, "allow-web-standard:dtls"},
		{SecurityPolicyReasonAllowQuic, "allow-web-standard:quic"},
		{SecurityPolicyReasonAllowStun, "allow-web-standard:stun"},
		{SecurityPolicyReasonAllowTurn, "allow-web-standard:turn"},
		{SecurityPolicyReasonAllowRtcp, "allow-web-standard:rtcp"},
		{SecurityPolicyReasonAllowRtp, "allow-rtp"},
		{SecurityPolicyReasonAllowHttp, "allow-http"},
		{SecurityPolicyReasonAllowPlaintext, "allow-plaintext"},
		{SecurityPolicyReasonAllowBudget, "allow-budget"},
		{SecurityPolicyReasonAllowUninspected, "allow-uninspected"},
		{SecurityPolicyReasonAllowWireGuard, "allow-app-standard:wireguard"},
		{SecurityPolicyReasonAllowOpenVpn, "allow-app-standard:openvpn"},
		{SecurityPolicyReasonAllowRtmp, "allow-app-standard:rtmp"},
		{SecurityPolicyReasonAllowLevin, "allow-app-standard:levin"},
		{SecurityPolicyReasonAllowRakNet, "allow-app-standard:raknet"},
		{SecurityPolicyReasonAllowEthereumDiscv4, "allow-app-standard:ethereum-discv4"},
		{SecurityPolicyReasonAllowEthereumRlpx, "allow-app-standard:ethereum-rlpx"},
		{SecurityPolicyReasonAllowMessaging, "allow-messaging"},
		{SecurityPolicyReasonAllowWhatsApp, "allow-app-standard:whatsapp"},
		{SecurityPolicyReason(-1), "unknown"},
		{securityPolicyReasonEnd, "unknown"},
		{SecurityPolicyReason(9999), "unknown"},
	}

	for _, c := range cases {
		if got := c.reason.String(); got != c.want {
			t.Errorf("reason %d String() = %q, want %q", c.reason, got, c.want)
		}
	}
}
