package connect

import (
	"net"
	"testing"
)

// The fork has no upstream ip_security_cfaa_test.go; this pins the SMTP
// port-classification changes from the SMTP policy port: TCP/587 moves to
// pass (the SMTP guard enforces STARTTLS), UDP/587 and both TCP/UDP/25 stay
// drop, and the existing secure-email ports stay pass.

func TestForkCfaaSmtpPortClassification(t *testing.T) {
	detector := newCfaaDetector(DefaultCfaaSecurityPolicySettings())
	ip := net.IPv4(203, 0, 113, 10)

	tests := []struct {
		name     string
		port     int
		protocol IpProtocol
		version  int
		want     cfaaVerdict
	}{
		{"587 tcp pass", smtpStartTlsPort, IpProtocolTcp, 4, cfaaPass},
		{"587 udp drop", smtpStartTlsPort, IpProtocolUdp, 4, cfaaDrop},
		{"25 tcp drop", smtpLocalPort, IpProtocolTcp, 4, cfaaDrop},
		{"25 udp drop", smtpLocalPort, IpProtocolUdp, 4, cfaaDrop},
		{"465 tcp pass", smtpImplicitTlsPort, IpProtocolTcp, 4, cfaaPass},
		{"993 tcp pass", 993, IpProtocolTcp, 4, cfaaPass},
		{"995 tcp pass", 995, IpProtocolTcp, 4, cfaaPass},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := detector.inspect(ip, test.port, test.protocol, test.version); got != test.want {
				t.Fatalf("cfaa port %d protocol %v = %v, want %v", test.port, test.protocol, got, test.want)
			}
		})
	}
}
